package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pingcap/errors"
	"io"
	"k8s.io/apimachinery/pkg/api/resource"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Provider implements the virtual-kubelet provider interface for RunPod
type Provider struct {
	nodeName           string
	clientset          *kubernetes.Clientset
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int
	logger             *slog.Logger
	config             config.Config
	runpodClient       *Client
	runpodAvailable    bool
	deletedPods        map[string]string
	deletedPodsMutex   sync.Mutex
	maxPrice           float64

	// For pod tracking (replaces resourceManager)
	pods        map[string]*v1.Pod     // Map of podKey -> Pod
	podStatus   map[string]*RunPodInfo // Map of podKey -> RunPodInfo
	podsMutex   sync.RWMutex           // Mutex for thread-safe access to pods maps
	notifyFunc  func(*v1.Pod)          // Function called when pod status changes
	notifyMutex sync.RWMutex           // Mutex for thread-safe access to notify function
}

// startPeriodicStatusUpdates polls the RunPod API to keep pod statuses up to date
func (p *Provider) startPeriodicStatusUpdates() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.updateAllPodStatuses()
			p.checkRunPodAPIHealth()
		}
	}
}

// startPeriodicCleanup handles periodic cleanup of terminated pods
func (p *Provider) startPeriodicCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.cleanupDeletedPods()
		}
	}
}

// checkRunPodAPIHealth checks if the RunPod API is available
func (p *Provider) checkRunPodAPIHealth() {
	// Skip check if no API key
	if p.runpodClient.apiKey == "" {
		p.runpodAvailable = false
		p.logger.Warn("RunPod API key not set, skipping API health check")
		return
	}

	// Make a simple API call to check health
	_, err := p.runpodClient.makeRESTRequest("GET", "gpuTypes", nil)
	p.runpodAvailable = (err == nil)
}

// NewProvider creates a new RunPod virtual kubelet provider
func NewProvider(ctx context.Context, nodeName, operatingSystem string, internalIP string,
	daemonEndpointPort int, config config.Config, clientset *kubernetes.Clientset, logger *slog.Logger) (*Provider, error) {

	// Create a new RunPod client
	runpodClient := NewRunPodClient(logger)

	provider := &Provider{
		nodeName:           nodeName,
		clientset:          clientset,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
		logger:             logger,
		config:             config,
		runpodClient:       runpodClient,
		runpodAvailable:    false,
		deletedPods:        make(map[string]string),
		deletedPodsMutex:   sync.Mutex{},
		maxPrice:           DefaultMaxPrice, // Use default or from config
		pods:               make(map[string]*v1.Pod),
		podStatus:          make(map[string]*RunPodInfo),
		podsMutex:          sync.RWMutex{},
	}

	// Initialize provider
	provider.checkRunPodAPIHealth()

	// Start background processes
	go provider.startPeriodicStatusUpdates()
	go provider.startPeriodicCleanup()

	return provider, nil
}

// Implementation of required interface methods to fulfill the PodLifecycleHandler interface
// CreatePod takes a Kubernetes Pod and deploys it within the provider
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	// Implementation for creating a pod
	p.logger.Info("Creating pod", "pod", pod.Name, "namespace", pod.Namespace)

	// Store the pod in our tracking map
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	p.podsMutex.Lock()
	p.pods[podKey] = pod.DeepCopy()
	p.podStatus[podKey] = &RunPodInfo{
		PodName:      pod.Name,
		Namespace:    pod.Namespace,
		Status:       string(PodStarting),
		CreationTime: time.Now(),
	}
	p.podsMutex.Unlock()

	return nil
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider
func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	// Implementation for updating a pod
	p.logger.Info("Updating pod", "pod", pod.Name, "namespace", pod.Namespace)

	// Update the pod in our tracking map
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	p.podsMutex.Lock()
	p.pods[podKey] = pod.DeepCopy()
	p.podsMutex.Unlock()

	return nil
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	p.logger.Info("Deleting pod", "pod", pod.Name, "namespace", pod.Namespace)

	// Check if this pod is backed by a RunPod instance
	podID := pod.Annotations[RunpodPodIDAnnotation]
	if podID != "" {
		// Track the deleted pod for cleanup
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		p.deletedPodsMutex.Lock()
		p.deletedPods[podKey] = podID
		p.deletedPodsMutex.Unlock()

		// Attempt to terminate RunPod instance
		if err := p.runpodClient.TerminatePod(podID); err != nil {
			p.logger.Error("Failed to terminate RunPod instance",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"podID", podID,
				"error", err)
		}
	}

	// Remove the pod from our tracking map
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	p.podsMutex.Lock()
	delete(p.pods, podKey)
	delete(p.podStatus, podKey)
	p.podsMutex.Unlock()

	return nil
}

// GetPod retrieves a pod by name from the provider
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	// Implementation for getting a pod
	podKey := fmt.Sprintf("%s-%s", namespace, name)

	p.podsMutex.RLock()
	pod, exists := p.pods[podKey]
	p.podsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("pod %s not found", podKey)
	}

	return pod, nil
}

// GetPodStatus retrieves the status of a pod by name from the provider
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	// Implementation for getting pod status
	podKey := fmt.Sprintf("%s-%s", namespace, name)

	p.podsMutex.RLock()
	podInfo, exists := p.podStatus[podKey]
	p.podsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("pod status not found for %s", podKey)
	}

	// Translate RunPod status to Kubernetes PodStatus
	return p.translateRunPodStatus(podInfo.Status, podInfo.StatusMessage), nil
}

// GetPods retrieves a list of all pods running on the provider
func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	// Implementation for getting all pods
	p.podsMutex.RLock()
	defer p.podsMutex.RUnlock()

	pods := make([]*v1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}

	return pods, nil
}

// NotifyPods Implement NotifyPods for async status updates
func (p *Provider) NotifyPods(ctx context.Context, notifyFunc func(*v1.Pod)) {
	p.notifyMutex.Lock()
	p.notifyFunc = notifyFunc
	p.notifyMutex.Unlock()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.updateAllPodStatuses()
			}
		}
	}()
}

func (p *Provider) updateAllPodStatuses() {
	// Get all pods we're tracking
	p.podsMutex.RLock()
	podKeys := make([]string, 0, len(p.pods))
	for podKey := range p.pods {
		podKeys = append(podKeys, podKey)
	}
	p.podsMutex.RUnlock()

	for _, podKey := range podKeys {
		p.podsMutex.RLock()
		pod := p.pods[podKey]
		podInfo := p.podStatus[podKey]
		p.podsMutex.RUnlock()

		if pod == nil || podInfo == nil {
			continue
		}

		// Ignore pods that are already in a terminal state
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}

		// Get the RunPod ID from annotations
		podID := pod.Annotations[RunpodPodIDAnnotation]
		if podID == "" {
			continue
		}

		// Check current status from RunPod API
		status, err := p.runpodClient.GetPodStatusREST(podID)
		if err != nil {
			p.logger.Error("Failed to get pod status from RunPod API",
				"pod", pod.Name, "namespace", pod.Namespace,
				"runpodID", podID, "error", err)
			continue
		}

		// Update pod info if status changed
		if string(status) != podInfo.Status {
			p.logger.Info("Pod status changed",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"prevStatus", podInfo.Status,
				"newStatus", string(status))

			// Update status in our tracking map
			p.podsMutex.Lock()
			podInfo.Status = string(status)
			p.podStatus[podKey] = podInfo
			p.podsMutex.Unlock()

			// Handle pod completion if needed
			if status == PodExited {
				p.handlePodCompletion(pod, podInfo)
			}

			// Notify status change if a notify function is registered
			p.notifyMutex.RLock()
			notifyFunc := p.notifyFunc
			p.notifyMutex.RUnlock()

			if notifyFunc != nil {
				// Update pod status before notification
				newStatus := p.translateRunPodStatus(string(status), podInfo.StatusMessage)
				updatedPod := pod.DeepCopy()
				updatedPod.Status = *newStatus

				p.podsMutex.Lock()
				p.pods[podKey] = updatedPod
				p.podsMutex.Unlock()

				notifyFunc(updatedPod)
			}
		}
	}
}

// Modify handlePodCompletion to work with new pod tracking
func (p *Provider) handlePodCompletion(pod *v1.Pod, podInfo *RunPodInfo) {
	// Get the RunPod ID from annotations
	podID := pod.Annotations[RunpodPodIDAnnotation]
	if podID == "" {
		return
	}

	// Get detailed status to determine if successful or failed
	status, err := p.runpodClient.GetDetailedPodStatus(podID)
	if err != nil {
		p.logger.Error("Failed to get detailed pod status for completion",
			"pod", pod.Name, "namespace", pod.Namespace,
			"runpodID", podID, "error", err)
		return
	}

	// Extract completion details
	exitCode := 0
	exitMessage := ""
	if status.Runtime != nil {
		exitCode = status.Runtime.Container.ExitCode
		exitMessage = status.Runtime.Container.Message

		// Update pod info with exit details
		podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
		p.podsMutex.Lock()
		podInfo.ExitCode = exitCode
		podInfo.StatusMessage = exitMessage
		p.podStatus[podKey] = podInfo
		p.podsMutex.Unlock()
	}

	// Determine if success or failure
	isSuccess := IsSuccessfulCompletion(status)

	// Update pod status
	podStatus := p.translateRunPodStatus(string(PodExited), exitMessage)
	if isSuccess {
		podStatus.Phase = v1.PodSucceeded
		if len(podStatus.ContainerStatuses) > 0 && podStatus.ContainerStatuses[0].State.Terminated != nil {
			podStatus.ContainerStatuses[0].State.Terminated.ExitCode = int32(exitCode)
			podStatus.ContainerStatuses[0].State.Terminated.Reason = "Completed"
		}
	} else {
		podStatus.Phase = v1.PodFailed
		if len(podStatus.ContainerStatuses) > 0 && podStatus.ContainerStatuses[0].State.Terminated != nil {
			podStatus.ContainerStatuses[0].State.Terminated.ExitCode = int32(exitCode)
			podStatus.ContainerStatuses[0].State.Terminated.Reason = "Error"
		}
	}

	// Update the pod with the new status
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	p.podsMutex.Lock()
	updatedPod := pod.DeepCopy()
	updatedPod.Status = *podStatus
	p.pods[podKey] = updatedPod
	p.podsMutex.Unlock()

	// Notify about the status change
	p.notifyMutex.RLock()
	notifyFunc := p.notifyFunc
	p.notifyMutex.RUnlock()

	if notifyFunc != nil {
		notifyFunc(updatedPod)
	}
}

// Implement NodeProvider interface

// Ping checks if the node is still active
func (p *Provider) Ping(ctx context.Context) error {
	p.checkRunPodAPIHealth() // Update the health status
	if !p.runpodAvailable {
		return fmt.Errorf("RunPod API is not available")
	}
	return nil
}

// NotifyNodeStatus is used to asynchronously monitor the node
func (p *Provider) NotifyNodeStatus(ctx context.Context, cb func(*v1.Node)) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Create a node with current status
				node := p.GetNodeStatus()
				cb(node)
			}
		}
	}()
}

// Helper to get current node status
func (p *Provider) GetNodeStatus() *v1.Node {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.nodeName,
			Labels: map[string]string{
				"type":                   "virtual-kubelet",
				"kubernetes.io/role":     "agent",
				"beta.kubernetes.io/os":  p.operatingSystem,
				"kubernetes.io/os":       p.operatingSystem,
				"kubernetes.io/hostname": p.nodeName,
			},
		},
		Spec: v1.NodeSpec{
			Taints: []v1.Taint{
				{
					Key:    "virtual-kubelet.io/provider",
					Value:  "runpod",
					Effect: v1.TaintEffectNoSchedule,
				},
			},
		},
		Status: v1.NodeStatus{
			NodeInfo: v1.NodeSystemInfo{
				OperatingSystem: p.operatingSystem,
				Architecture:    "amd64",
				KubeletVersion:  "v1.19.0",
			},
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("20"),
				v1.ResourceMemory: resource.MustParse("100Gi"),
				v1.ResourcePods:   resource.MustParse("100"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("20"),
				v1.ResourceMemory: resource.MustParse("100Gi"),
				v1.ResourcePods:   resource.MustParse("100"),
			},
			Conditions: []v1.NodeCondition{
				{
					Type:               v1.NodeReady,
					Status:             v1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletReady",
					Message:            "kubelet is ready.",
				},
				{
					Type:               v1.NodeDiskPressure,
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasSufficientDisk",
					Message:            "kubelet has sufficient disk space available",
				},
				{
					Type:               v1.NodeMemoryPressure,
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasSufficientMemory",
					Message:            "kubelet has sufficient memory available",
				},
				{
					Type:               v1.NodePIDPressure,
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasSufficientPID",
					Message:            "kubelet has sufficient PID available",
				},
			},
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: p.internalIP,
				},
			},
			DaemonEndpoints: v1.NodeDaemonEndpoints{
				KubeletEndpoint: v1.DaemonEndpoint{
					Port: int32(p.daemonEndpointPort),
				},
			},
		},
	}

	return node
}

// cleanupDeletedPods checks and cleans up any pods that have been deleted from K8s
// but may still exist in RunPod
func (p *Provider) cleanupDeletedPods() {
	p.deletedPodsMutex.Lock()
	defer p.deletedPodsMutex.Unlock()

	for podKey, runpodID := range p.deletedPods {
		parts := strings.Split(podKey, "/")
		if len(parts) != 2 {
			delete(p.deletedPods, podKey)
			continue
		}

		namespace := parts[0]
		name := parts[1]

		// Check if pod still exists in K8s
		_, err := p.clientset.CoreV1().Pods(namespace).Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)

		if err != nil && k8serrors.IsNotFound(err) {
			// Pod is gone from K8s, terminate it in RunPod if needed
			p.logger.Info("Cleaning up deleted pod in RunPod",
				"namespace", namespace,
				"name", name,
				"runpodID", runpodID)

			if err := p.runpodClient.TerminatePod(runpodID); err != nil {
				p.logger.Error("Failed to terminate RunPod instance during cleanup",
					"runpodID", runpodID, "err", err)
			}

			// Remove from tracking
			delete(p.deletedPods, podKey)
		}
	}
}

// ExtractEnvVars extracts environment variables from a pod
func (p *Provider) ExtractEnvVars(pod *v1.Pod) ([]RunPodEnv, error) {
	var envVars []RunPodEnv
	var secretsToFetch = make(map[string]bool)
	var secretEnvVars = make(map[string][]struct {
		SecretKey string
		EnvKey    string
	})
	var secretRefEnvs = make(map[string]bool)

	// First pass: collect all secrets we need to fetch
	if len(pod.Spec.Containers) > 0 {
		container := pod.Spec.Containers[0]

		// Add regular environment variables
		for _, env := range container.Env {
			// If this is a regular env var, add it directly
			if env.Value != "" {
				envVars = append(envVars, RunPodEnv{
					Key:   env.Name,
					Value: env.Value,
				})
			} else if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				// This is a secret reference, track it for fetching
				secretName := env.ValueFrom.SecretKeyRef.Name
				secretsToFetch[secretName] = true

				if _, ok := secretEnvVars[secretName]; !ok {
					secretEnvVars[secretName] = []struct {
						SecretKey string
						EnvKey    string
					}{}
				}

				// Store the mapping between secret key and env var name
				secretEnvVars[secretName] = append(secretEnvVars[secretName], struct {
					SecretKey string
					EnvKey    string
				}{
					SecretKey: env.ValueFrom.SecretKeyRef.Key,
					EnvKey:    env.Name,
				})
			}
		}

		// Handle envFrom references
		for _, envFrom := range container.EnvFrom {
			if envFrom.SecretRef != nil {
				secretName := envFrom.SecretRef.Name
				secretsToFetch[secretName] = true
				secretRefEnvs[secretName] = true
			}
		}
	}

	// Also check for secrets mounted as volumes that should be included as env vars
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			secretName := volume.Secret.SecretName
			secretsToFetch[secretName] = true

			// For volume mounts with specific items
			if items := volume.Secret.Items; len(items) > 0 {
				if _, ok := secretEnvVars[secretName]; !ok {
					secretEnvVars[secretName] = []struct {
						SecretKey string
						EnvKey    string
					}{}
				}

				for _, item := range items {
					// When mounted as a volume item, use the path as env var name if not specified
					envKey := item.Path
					if envKey == "" {
						envKey = item.Key
					}

					secretEnvVars[secretName] = append(secretEnvVars[secretName], struct {
						SecretKey string
						EnvKey    string
					}{
						SecretKey: item.Key,
						EnvKey:    envKey,
					})
				}
			}
		}
	}

	// Second pass: fetch all needed secrets and extract values
	for secretName := range secretsToFetch {
		secret, err := p.clientset.CoreV1().Secrets(pod.Namespace).Get(
			context.Background(),
			secretName,
			metav1.GetOptions{},
		)
		if err != nil {
			p.logger.Error("failed to get secret",
				"namespace", pod.Namespace,
				"secret", secretName, "err", err)
			continue
		}

		// Handle specific secret keys mapped to env vars
		if mappings, ok := secretEnvVars[secretName]; ok {
			for _, mapping := range mappings {
				if secretValue, ok := secret.Data[mapping.SecretKey]; ok {
					// Add it as an environment variable with the correct name
					envVars = append(envVars, RunPodEnv{
						Key:   mapping.EnvKey,
						Value: strings.ReplaceAll(string(secretValue), "\n", "\\n"),
					})
				} else {
					p.logger.Warn("Secret key not found",
						"namespace", pod.Namespace,
						"secret", secretName,
						"key", mapping.SecretKey)
				}
			}
		}

		// Handle envFrom that should import all keys from the secret
		if secretRefEnvs[secretName] {
			for key, value := range secret.Data {
				envVars = append(envVars, RunPodEnv{
					Key:   key,
					Value: strings.ReplaceAll(string(value), "\n", "\\n"),
				})
			}
		}
	}

	return envVars, nil
}

// LoadRunning loads from the runpod api the running pods and if missing adds them to the list of virtual pods in the cluster
func (p *Provider) LoadRunning() {
	// Skip check if no API key
	if p.runpodClient.apiKey == "" {
		p.runpodAvailable = false
		p.logger.Warn("RunPod API key not set, skipping LoadRunning")
		return
	}

	// Fetch RunPod instances from API
	runningPods, exitedPods, ok := p.fetchRunPodInstances()
	if !ok {
		p.runpodAvailable = false
		return
	}

	// API is available
	p.runpodAvailable = true

	// Combine running and exited pods
	allPods := append(runningPods, exitedPods...)

	// Map existing RunPod instances in the cluster
	existingRunPodMap := p.mapExistingRunPodInstances()

	// Process each pod from RunPod API
	for _, runpodInstance := range allPods {
		p.processRunPodInstance(runpodInstance, existingRunPodMap)
	}
}

// fetchRunPodInstances fetches both running and exited pods from the RunPod API
func (p *Provider) fetchRunPodInstances() (running []RunPodInstance, exited []RunPodInstance, ok bool) {
	// Make a request to the RunPod API to get all running pods
	runningPods, ok := p.fetchRunPodInstancesByStatus("RUNNING")
	if !ok {
		return nil, nil, false
	}

	// Also check for exited pods
	exitedPods, _ := p.fetchRunPodInstancesByStatus("EXITED")
	// We don't care if exited pods fetch fails, we'll continue with running pods

	return runningPods, exitedPods, true
}

// processRunPodInstance processes a single RunPod instance
func (p *Provider) processRunPodInstance(runpodInstance RunPodInstance, existingRunPodMap map[string]RunPodInfo) {
	// Skip if this RunPod instance is already represented in the cluster
	if _, exists := existingRunPodMap[runpodInstance.ID]; exists {
		if runpodInstance.DesiredStatus == "EXITED" || runpodInstance.CurrentStatus == "EXITED" {
			p.handleRunPodExitStatus(runpodInstance)
		}
		return
	}

	// Handle running instance
	p.CreateVirtualPod(runpodInstance)
}

// CreateVirtualPod creates a virtual Pod representation of a RunPod instance
func (p *Provider) CreateVirtualPod(runpodInstance RunPodInstance) error {
	// Create a virtual pod for this RunPod instance
	p.logger.Info("Creating virtual pod for existing RunPod instance",
		"podID", runpodInstance.ID,
		"jobName", runpodInstance.Name)
	// Use consistent naming format for virtual pods
	podName := fmt.Sprintf("runpod-%s", runpodInstance.ID)

	// Create labels to link Pod to Job
	podLabels := make(map[string]string)
	podLabels[RunpodManagedLabel] = "true"
	podLabels[RunpodPodIDAnnotation] = runpodInstance.ID

	// Create annotations for the Pod
	podAnnotations := make(map[string]string)
	podAnnotations[RunpodPodIDAnnotation] = runpodInstance.ID
	podAnnotations[RunpodCostAnnotation] = fmt.Sprintf("%f", runpodInstance.CostPerHr)
	podAnnotations["runpod.io/external"] = "true"

	// Create Pod object
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Labels:      podLabels,
			Annotations: podAnnotations,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "runpod-proxy",
					Image:   runpodInstance.ImageName,
					Command: []string{"/bin/sh", "-c", "echo 'This pod represents a RunPod instance'; sleep infinity"},
				},
			},
			RestartPolicy: "Never",
			NodeName:      "runpod-virtual-node",
			NodeSelector: map[string]string{
				"runpod.io/virtual": "true",
			},
			Tolerations: []v1.Toleration{
				{
					Key:      "runpod.io/virtual",
					Operator: v1.TolerationOpExists,
					Effect:   v1.TaintEffectNoSchedule,
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{
					Type:               v1.PodReady,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	//create pod in default namespace
	_, err := p.clientset.CoreV1().Pods("default").Create(
		context.Background(),
		pod,
		metav1.CreateOptions{},
	)
	if err != nil {
		p.logger.Error("Failed to create virtual pod for RunPod instance",
			"pod", pod.Name, "runpodID", runpodInstance.ID, "err", err)
		return fmt.Errorf("failed to create virtual pod: %w", err)
	}
	return nil
}

// fetchRunPodInstancesByStatus fetches RunPod instances with the given status
func (p *Provider) fetchRunPodInstancesByStatus(status string) ([]RunPodInstance, bool) {
	resp, err := p.runpodClient.makeRESTRequest("GET", fmt.Sprintf("pods?desiredStatus=%s", status), nil)
	if err != nil {
		p.logger.Error("Failed to get pods from RunPod API", "status", status, "err", err)
		return nil, false
	}

	// Ensure we always close the response body
	if resp != nil && resp.Body != nil {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				p.logger.Error("Failed to close response body", "err", err)
			}
		}()
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			p.logger.Error("Failed to read error response body",
				"statusCode", resp.StatusCode,
				"readErr", readErr)
		} else {
			p.logger.Error("RunPod API returned error",
				"statusCode", resp.StatusCode,
				"response", string(body))
		}
		return nil, false
	}

	// Parse the response
	var pods []RunPodInstance
	if err := json.NewDecoder(resp.Body).Decode(&pods); err != nil {
		p.logger.Error("Failed to decode RunPod API response", "status", status, "err", err)
		return nil, false
	}

	return pods, true
}

// mapExistingRunPodInstances maps existing RunPod instances in the cluster
func (p *Provider) mapExistingRunPodInstances() map[string]RunPodInfo {
	// Get existing pods in the cluster
	existingPods, err := p.clientset.CoreV1().Pods("").List(
		context.Background(),
		v1.ListOptions{
			LabelSelector: RunpodManagedLabel + "=true",
		},
	)
	if err != nil {
		p.logger.Error("Failed to list existing pods", "err", err)
		return make(map[string]RunPodInfo)
	}

	// Create a map of existing RunPod IDs to job info
	existingRunPodMap := make(map[string]RunPodInfo)
	for _, pod := range existingPods.Items {
		if podID, exists := pod.Annotations[RunpodPodIDAnnotation]; exists {
			if jobName, exists := pod.Labels["job-name"]; exists {
				existingRunPodMap[podID] = RunPodInfo{
					JobName:   jobName,
					Namespace: pod.Namespace,
				}
			}
		}
	}

	return existingRunPodMap
}

// PrepareRunPodParameters prepares parameters for RunPod deployment
func (p *Provider) PrepareRunPodParameters(job batchv1.Job, graphql bool) (map[string]interface{}, error) {
	// Determine cloud type - default to COMMUNITY but allow override via annotation
	cloudType := "SECURE"
	if cloudTypeVal, exists := job.Annotations[RunpodCloudTypeAnnotation]; exists {
		// Validate and normalize the cloud type value
		cloudTypeUpperCase := strings.ToUpper(cloudTypeVal)
		if cloudTypeUpperCase == "SECURE" || cloudTypeUpperCase == "COMMUNITY" {
			cloudType = cloudTypeUpperCase
		} else {
			p.logger.Warn("Invalid cloud type specified, using default",
				"job", job.Name,
				"namespace", job.Namespace,
				"specifiedValue", cloudTypeVal,
				"defaultValue", cloudType)
		}
	}

	// Extract container registry auth ID if provided
	containerRegistryAuthId := ""
	if authID, exists := job.Annotations[RunpodContainerRegistryAuthAnnotation]; exists && authID != "" {
		containerRegistryAuthId = authID
	}

	// Extract template ID if provided
	templateId := ""
	if tplID, exists := job.Annotations[RunpodTemplateIdAnnotation]; exists && tplID != "" {
		templateId = tplID
	}

	// Determine minimum GPU memory required
	minRAMPerGPU := 16 // Default minimum memory
	if memStr, exists := job.Annotations[GpuMemoryAnnotation]; exists {
		if mem, err := strconv.Atoi(memStr); err == nil {
			minRAMPerGPU = mem
		}
	}

	// Get GPU types - pass the cloud type to filter correctly
	gpuTypes, err := p.runpodClient.GetGPUTypes(minRAMPerGPU, p.maxPrice, cloudType)
	if err != nil {
		return nil, fmt.Errorf("failed to get GPU types: %w", err)
	}

	// Extract environment variables from job
	envVars, err := p.ExtractEnvVars(job)
	if err != nil {
		return nil, fmt.Errorf("failed to extract environment variables: %w", err)
	}

	var formattedEnvVars interface{}
	if graphql {
		formattedEnvVars = FormatEnvVarsForGraphQL(envVars)
	} else {
		formattedEnvVars = FormatEnvVarsForREST(envVars)
	}
	// Determine image name from job
	var imageName string
	if len(job.Spec.Template.Spec.Containers) > 0 {
		imageName = job.Spec.Template.Spec.Containers[0].Image
	} else {
		return nil, fmt.Errorf("job has no containers")
	}

	// Use the job name directly without namespace prefix
	runpodJobName := job.Name

	// Default values
	volumeInGb := 0
	containerDiskInGb := 15

	// Create deployment parameters - use the same cloudType as used for filtering https://graphql-spec.runpod.io/#definition-PodFindAndDeployOnDemandInput
	params := map[string]interface{}{
		"cloudType":         cloudType,
		"volumeInGb":        volumeInGb,
		"containerDiskInGb": containerDiskInGb,
		"minRAMPerGPU":      minRAMPerGPU,
		"gpuTypeIds":        gpuTypes, // Use the array directly, don't stringify it
		"name":              runpodJobName,
		"imageName":         imageName,
		"templateId":        templateId,
		"env":               formattedEnvVars,
	}

	// Add templateId to params if it exists
	if templateId != "" {
		params["templateId"] = templateId
	}

	if containerRegistryAuthId != "" {
		params["containerRegistryAuthId"] = containerRegistryAuthId
	}

	return params, nil
}

// ForceDeletePod forcefully removes a pod from the Kubernetes API
func (c *Provider) ForceDeletePod(namespace, name string) error {
	// Create zero grace period for immediate deletion
	gracePeriod := int64(0)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &[]metav1.DeletionPropagation{metav1.DeletePropagationBackground}[0],
	}

	err := c.clientset.CoreV1().Pods(namespace).Delete(
		context.Background(),
		name,
		deleteOptions,
	)

	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	c.logger.Info("Successfully force deleted pod", "pod", name, "namespace", namespace)
	return nil
}

// handleRunPodFailure handles a RunPod instance failure using job annotations as tracking
func (c *Provider) handleRunPodFailure(podID string, reason string) error {
	c.logger.Debug("Handling RunPod instance failure",
		"podID", podID,
		"reason", reason)
	var namespace = "default"
	// Step 1: Check if the virtual pod exists before attempting to delete it
	podName := fmt.Sprintf("runpod-%s", podID)

	_, err := c.clientset.CoreV1().Pods(namespace).Get(
		context.Background(),
		podName,
		metav1.GetOptions{},
	)

	// Only attempt deletion if the pod exists
	if err == nil {
		c.logger.Info("Found virtual pod for failed RunPod instance, deleting",
			"pod", podName)

		if err := c.ForceDeletePod(namespace, podName); err != nil {
			c.logger.Error("Failed to cleanup virtual pod during failure handling",
				"pod", podName,
				"err", err)
		} else {
			c.logger.Info("Successfully force deleted pod",
				"pod", podName)
		}
	} else if !k8serrors.IsNotFound(err) {
		// Some other error occurred when trying to get the pod
		c.logger.Error("Error checking if pod exists",
			"pod", podName,
			"err", err)
	}

	// Step 2: Update job with retries
	retryCount := 0
	maxRetries := DefaultRetryCount

	for retryCount < maxRetries {
		// Get the latest version of the job to avoid conflicts
		currentJob, err := c.clientset.BatchV1().Jobs(job.Namespace).Get(
			context.Background(),
			job.Name,
			metav1.GetOptions{},
		)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				c.logger.Info("Job not found during failure handling, skipping",
					"job", job.Name,
					"namespace", job.Namespace,
					"podID", podID)
				return nil
			}
			return fmt.Errorf("failed to get current job state: %w", err)
		}

		// Make a deep copy to avoid modifying the cache
		jobCopy := currentJob.DeepCopy()

		// Check if job has been updated with this specific failure info already
		if jobCopy.Annotations != nil {
			specificFailureKey := fmt.Sprintf("runpod.io/failure-%s", podID)
			if _, hasFailureAnnotation := jobCopy.Annotations[specificFailureKey]; hasFailureAnnotation {
				c.logger.Debug("Job already marked for this failure, skipping update",
					"job", job.Name,
					"namespace", job.Namespace,
					"podID", podID)
				return nil
			}
		}

		// Increment the failure counter in both status and annotations for tracking
		jobCopy.Status.Failed++

		if jobCopy.Annotations == nil {
			jobCopy.Annotations = make(map[string]string)
		}

		// Track failure count in annotations too for visibility
		failCount := 1
		if countStr, exists := jobCopy.Annotations[RunpodFailureAnnotation]; exists {
			if count, err := strconv.Atoi(countStr); err == nil {
				failCount = count + 1
			}
		}
		jobCopy.Annotations[RunpodFailureAnnotation] = strconv.Itoa(failCount)

		// Add an annotation to identify this specific failure with timestamp and reason
		specificFailureKey := fmt.Sprintf("runpod.io/failure-%s", podID)
		jobCopy.Annotations[specificFailureKey] = fmt.Sprintf("%s|%s",
			time.Now().Format(time.RFC3339), reason)

		// Remove RunPod annotations to reset the offload state
		delete(jobCopy.Annotations, RunpodOffloadedAnnotation)
		delete(jobCopy.Annotations, RunpodPodIDAnnotation)

		// First update annotations
		if err := c.UpdateJobWithRetry(jobCopy); err != nil {
			c.logger.Error("Failed to update job annotations",
				"job", job.Name,
				"namespace", job.Namespace,
				"err", err)
			retryCount++
			time.Sleep(DefaultRetryDelay)
			continue
		}

		// Then update job status with retry logic
		_, err = c.clientset.BatchV1().Jobs(jobCopy.Namespace).UpdateStatus(
			context.Background(),
			jobCopy,
			metav1.UpdateOptions{},
		)

		if err == nil {
			// Update succeeded
			c.logger.Info("Successfully updated job to reflect RunPod failure",
				"job", job.Name,
				"namespace", job.Namespace,
				"podID", podID,
				"failureCount", jobCopy.Status.Failed)
			return nil
		}

		// Check if this is a conflict error
		if !k8serrors.IsConflict(err) {
			c.logger.Error("Failed to update job status for failure, non-conflict error",
				"job", jobCopy.Name,
				"namespace", jobCopy.Namespace,
				"err", err)
			// For non-conflict errors, we still continue with retries
		}

		// Increment retry count
		retryCount++

		// Wait briefly before retrying
		time.Sleep(DefaultRetryDelay)

		c.logger.Info("Retrying job status update after error",
			"job", job.Name,
			"namespace", job.Namespace,
			"retry", retryCount,
			"error", err)
	}

	if retryCount == maxRetries {
		c.logger.Error("Failed to update job status after maximum retries",
			"job", job.Name,
			"namespace", job.Namespace,
			"maxRetries", maxRetries)
		return fmt.Errorf("failed to update job status after %d retries", maxRetries)
	}

	return nil
}

// handleRunPodExitStatus handles a RunPod instance that has exited
func (p *Provider) handleRunPodExitStatus(runpodInstance RunPodInstance) error {
	p.logger.Info("Found exited RunPod instance during API check",
		"podID", runpodInstance.ID,
		"namespace", info.Namespace,
		"status", runpodInstance.DesiredStatus)
	// Get detailed status from RunPod API
	status, err := p.runpodClient.GetDetailedPodStatus(runpodInstance.ID)
	if err != nil {
		p.logger.Error("Failed to get detailed pod status",
			"podID", runpodInstance.ID,
			"err", err)

		// If we can't get details, assume failure to be safe
		return p.handleRunPodFailure(runpodInstance.ID, "Failed to get status details")
	}

	// Determine if this was a successful completion
	isSuccess := IsSuccessfulCompletion(status)

	// Get exit details
	exitCode := 0
	exitMessage := ""
	if status.Runtime != nil {
		exitCode = status.Runtime.Container.ExitCode
		exitMessage = status.Runtime.Container.Message
	}

	p.logger.Info("Processing RunPod exit status",
		"job", job.Name,
		"namespace", job.Namespace,
		"podID", podID,
		"exitCode", exitCode,
		"isSuccess", isSuccess,
		"message", exitMessage)

	if isSuccess {
		// Handle as a success
		p.handleRunPodSuccess(podID, exitCode, exitMessage)
		return nil
	} else {
		// Handle as a failure
		return p.handleRunPodFailure(podID, fmt.Sprintf("ExitCode: %d, Message: %s", exitCode, exitMessage))
	}
}

// handleRunPodSuccess handles a successfully completed RunPod instance
func (p *Provider) handleRunPodSuccess(podID string, exitCode int, message string) error {
	p.logger.Info("Handling RunPod instance success",
		"job", job.Name,
		"namespace", job.Namespace,
		"podID", podID,
		"exitCode", exitCode)

	// Get current job to make sure we're working with the latest version
	currentJob, err := p.clientset.BatchV1().Jobs(job.Namespace).Get(
		context.Background(),
		job.Name,
		v1.GetOptions{},
	)
	if err != nil {
		if errors.IsNotFound(err) {
			p.logger.Info("Job not found during success handling, skipping",
				"job", job.Name,
				"namespace", job.Namespace,
				"podID", podID)
			return nil
		}
		return fmt.Errorf("failed to get job: %w", err)
	}

	// Make a deep copy to avoid modifying the cache
	jobCopy := currentJob.DeepCopy()

	// Update job annotations to record success
	if jobCopy.Annotations == nil {
		jobCopy.Annotations = make(map[string]string)
	}

	successKey := fmt.Sprintf("io/success-%s", podID)
	jobCopy.Annotations[successKey] = fmt.Sprintf("exitCode=%d,time=%s",
		exitCode, time.Now().Format(time.RFC3339))

	// Update the job
	if err := p.UpdateJobWithRetry(jobCopy); err != nil {
		p.logger.Error("Failed to update job annotations for success",
			"job", job.Name,
			"namespace", job.Namespace,
			"podID", podID,
			"err", err)
	}

	// Clean up the RunPod virtual pod if it exists
	podName := fmt.Sprintf("runpod-%s", podID)
	if err := p.ForceDeletePod(job.Namespace, podName); err != nil {
		if !errors.IsNotFound(err) {
			p.logger.Error("Failed to delete virtual pod for successful RunPod instance",
				"pod", podName,
				"namespace", job.Namespace,
				"err", err)
		}
	}

	// Create a success pod to properly record the completion in Kubernetes
	if err := p.createSuccessPod(&job, podID, exitCode, message); err != nil {
		p.logger.Error("Failed to create success pod",
			"job", job.Name,
			"namespace", job.Namespace,
			"podID", podID,
			"err", err)
	}

	// Update job status to mark as succeeded
	jobStatusCopy := jobCopy.DeepCopy()
	jobStatusCopy.Status.Succeeded++

	// Try to update the job status
	_, err = p.clientset.BatchV1().Jobs(jobStatusCopy.Namespace).UpdateStatus(
		context.Background(),
		jobStatusCopy,
		v1.UpdateOptions{},
	)

	if err != nil {
		p.logger.Error("Failed to update job status for success",
			"job", job.Name,
			"namespace", job.Namespace,
			"podID", podID,
			"err", err)
		// Continue anyway, this isn't a critical error
	}

	return nil
}

// translateRunPodStatus converts a RunPod status string to a Kubernetes PodStatus
func (p *Provider) translateRunPodStatus(runpodStatus string, statusMessage string) *v1.PodStatus {
	now := metav1.NewTime(time.Now())

	// Initialize the container status
	containerStatus := v1.ContainerStatus{
		Name:         "runpod-container", // Default container name
		RestartCount: 0,
		Ready:        runpodStatus == string(PodRunning),
		Image:        "runpod-image", // This should be replaced with actual image if available
		ImageID:      "",
		ContainerID:  "",
	}

	// Set the pod phase and container state based on RunPod status
	var phase v1.PodPhase
	var containerState v1.ContainerState

	switch runpodStatus {
	case string(PodRunning):
		phase = v1.PodRunning
		containerState = v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: now,
			},
		}
	case string(PodStarting):
		phase = v1.PodPending
		containerState = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  "Starting",
				Message: statusMessage,
			},
		}
	case string(PodExited):
		// For exited pods, we need to check if it was successful or not
		// This is typically determined by the exit code which would be set by handlePodCompletion
		if strings.Contains(statusMessage, "Completed") || strings.Contains(statusMessage, "Success") {
			phase = v1.PodSucceeded
			containerState = v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					ExitCode:   0, // Default to 0 for success
					Reason:     "Completed",
					Message:    statusMessage,
					FinishedAt: now,
					StartedAt:  now, // We may not know actual start time
				},
			}
		} else {
			phase = v1.PodFailed
			containerState = v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					ExitCode:   1, // Default to 1 for error
					Reason:     "Error",
					Message:    statusMessage,
					FinishedAt: now,
					StartedAt:  now, // We may not know actual start time
				},
			}
		}
	case string(PodTerminating):
		phase = v1.PodRunning
		containerState = v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: now,
			},
		}
	case string(PodTerminated):
		phase = v1.PodSucceeded // Assume success for clean termination
		containerState = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode:   0,
				Reason:     "Terminated",
				Message:    statusMessage,
				FinishedAt: now,
				StartedAt:  now, // We may not know actual start time
			},
		}
	case string(PodNotFound):
		phase = v1.PodUnknown
		containerState = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  "NotFound",
				Message: "Pod not found in RunPod API",
			},
		}
	default:
		phase = v1.PodUnknown
		containerState = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  "Unknown",
				Message: fmt.Sprintf("Unknown RunPod status: %s", runpodStatus),
			},
		}
	}

	// Set the container state
	containerStatus.State = containerState

	// Create the pod status
	podStatus := &v1.PodStatus{
		Phase:             phase,
		Conditions:        []v1.PodCondition{},
		Message:           statusMessage,
		Reason:            "",
		HostIP:            "",
		PodIP:             "", // This should be set if available
		StartTime:         &now,
		QOSClass:          v1.PodQOSGuaranteed, // Default to guaranteed QoS
		ContainerStatuses: []v1.ContainerStatus{containerStatus},
	}

	// Set pod conditions based on the phase
	podStatus.Conditions = append(podStatus.Conditions, v1.PodCondition{
		Type:               v1.PodInitialized,
		Status:             v1.ConditionTrue,
		LastTransitionTime: now,
		LastProbeTime:      now,
		Reason:             "",
		Message:            "",
	})

	readyConditionStatus := v1.ConditionFalse
	if phase == v1.PodRunning {
		readyConditionStatus = v1.ConditionTrue
	}

	podStatus.Conditions = append(podStatus.Conditions, v1.PodCondition{
		Type:               v1.PodReady,
		Status:             readyConditionStatus,
		LastTransitionTime: now,
		LastProbeTime:      now,
		Reason:             "",
		Message:            "",
	})

	scheduledConditionStatus := v1.ConditionTrue
	if phase == v1.PodPending {
		scheduledConditionStatus = v1.ConditionFalse
	}

	podStatus.Conditions = append(podStatus.Conditions, v1.PodCondition{
		Type:               v1.PodScheduled,
		Status:             scheduledConditionStatus,
		LastTransitionTime: now,
		LastProbeTime:      now,
		Reason:             "",
		Message:            "",
	})

	// Add the ContainersReady condition
	podStatus.Conditions = append(podStatus.Conditions, v1.PodCondition{
		Type:               v1.ContainersReady,
		Status:             readyConditionStatus,
		LastTransitionTime: now,
		LastProbeTime:      now,
		Reason:             "",
		Message:            "",
	})

	return podStatus
}

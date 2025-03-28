package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"io"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
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

	// Create a new RunPod client and pass the clientset
	runpodClient := NewRunPodClient(logger, clientset)

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
		pods:               make(map[string]*v1.Pod),
		podStatus:          make(map[string]*RunPodInfo),
		podsMutex:          sync.RWMutex{},
	}

	// Initialize provider
	provider.checkRunPodAPIHealth()

	// Start background processes
	go provider.startPeriodicStatusUpdates()
	go provider.startPeriodicCleanup()
	go provider.startPendingPodProcessor() // Add the pending pod processor

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

	// Deploy to RunPod if needed
	// This is where we integrate with the RunPod API
	err := p.DeployPodToRunPod(pod)
	if err != nil {
		p.logger.Error("Failed to deploy pod to RunPod",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)

		// Even if RunPod deployment fails, we still track the pod
		// The pod will be in a pending state in Kubernetes
		return nil
	}

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

// DeployPodToRunPod handles the deployment of a Kubernetes pod to RunPod
func (p *Provider) DeployPodToRunPod(pod *v1.Pod) error {
	p.logger.Info("Deploying pod to RunPod",
		"pod", pod.Name,
		"namespace", pod.Namespace)

	// Check if RunPod API is available
	if !p.runpodAvailable {
		return fmt.Errorf("RunPod API is not available")
	}

	// Prepare RunPod parameters - use REST API by default
	params, err := p.runpodClient.PrepareRunPodParameters(pod, false)
	if err != nil {
		p.logger.Error("Failed to prepare RunPod parameters",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)
		return err
	}

	// Deploy to RunPod using REST API
	podID, costPerHr, err := p.runpodClient.DeployPodREST(params)
	if err != nil {
		p.logger.Error("Failed to deploy pod to RunPod",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)
		return err
	}

	p.logger.Info("Successfully deployed pod to RunPod",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"runpodID", podID,
		"costPerHr", costPerHr)

	// Update pod with RunPod annotations
	return p.updatePodWithRunPodInfo(pod, podID, costPerHr)
}

// updatePodWithRunPodInfo updates the pod with RunPod instance information
func (p *Provider) updatePodWithRunPodInfo(pod *v1.Pod, podID string, costPerHr float64) error {
	// Get the latest version of the pod to avoid conflicts
	currentPod, err := p.clientset.CoreV1().Pods(pod.Namespace).Get(
		context.Background(),
		pod.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get current pod state: %w", err)
	}

	// Make a deep copy to avoid modifying the cache
	podCopy := currentPod.DeepCopy()

	// Update annotations
	if podCopy.Annotations == nil {
		podCopy.Annotations = make(map[string]string)
	}
	podCopy.Annotations[RunpodPodIDAnnotation] = podID
	podCopy.Annotations[RunpodCostAnnotation] = fmt.Sprintf("%f", costPerHr)

	// Update the pod
	_, err = p.clientset.CoreV1().Pods(podCopy.Namespace).Update(
		context.Background(),
		podCopy,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update pod with RunPod info: %w", err)
	}

	// Update our local tracking map
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	p.podsMutex.Lock()
	p.pods[podKey] = podCopy
	if podInfo, exists := p.podStatus[podKey]; exists {
		podInfo.ID = podID
		podInfo.CostPerHr = costPerHr
		podInfo.Status = string(PodStarting)
		p.podStatus[podKey] = podInfo
	} else {
		p.podStatus[podKey] = &RunPodInfo{
			ID:           podID,
			CostPerHr:    costPerHr,
			PodName:      pod.Name,
			Namespace:    pod.Namespace,
			Status:       string(PodStarting),
			CreationTime: time.Now(),
		}
	}
	p.podsMutex.Unlock()

	return nil
}

// Resize implements the api.AttachIO interface for terminal resizing
// Note: RunPod.io doesn't support terminal access, so this is a no-op implementation
func (p *Provider) Resize(ctx context.Context, namespace, podName, containerName string, size api.TermSize) error {
	p.logger.Debug("Resize request received but RunPod doesn't support terminal access",
		"pod", podName,
		"namespace", namespace,
		"container", containerName)

	// Since RunPod doesn't support terminal access, we just return nil
	// This satisfies the interface without attempting to use unsupported functionality
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

// startPendingPodProcessor starts a background process to check for pending pods
func (p *Provider) startPendingPodProcessor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.processPendingPods()
		}
	}
}

// processPendingPods checks for pending pods that need to be deployed to RunPod
func (p *Provider) processPendingPods() {
	p.podsMutex.RLock()
	podKeys := make([]string, 0, len(p.pods))
	for podKey, pod := range p.pods {
		// Check if the pod is pending deployment to RunPod
		if pod.Status.Phase == v1.PodPending {
			podKeys = append(podKeys, podKey)
		}
	}
	p.podsMutex.RUnlock()

	for _, podKey := range podKeys {
		p.podsMutex.RLock()
		pod := p.pods[podKey]
		p.podsMutex.RUnlock()

		if pod == nil {
			continue
		}

		// Try to deploy the pod to RunPod
		err := p.DeployPodToRunPod(pod)
		if err != nil {
			p.logger.Error("Failed to deploy pending pod to RunPod",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"error", err)

			// Check if we've reached the retry limit
			podInfo, exists := p.podStatus[podKey]
			if exists {
				// If pod has been pending too long, mark it as failed
				if time.Since(podInfo.CreationTime) > 15*time.Minute {
					p.logger.Warn("Pod has been pending too long, marking as failed",
						"pod", pod.Name,
						"namespace", pod.Namespace)

					updatedPod := pod.DeepCopy()
					updatedPod.Status.Phase = v1.PodFailed
					updatedPod.Status.Reason = "RunPodDeploymentFailed"
					updatedPod.Status.Message = "Failed to deploy pod to RunPod after multiple attempts"

					p.podsMutex.Lock()
					p.pods[podKey] = updatedPod
					p.podsMutex.Unlock()

					// Notify about status change
					p.notifyMutex.RLock()
					notifyFunc := p.notifyFunc
					p.notifyMutex.RUnlock()

					if notifyFunc != nil {
						notifyFunc(updatedPod)
					}
				}
			}
		}
	}
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

// handlePodCompletion processes a pod that has completed execution
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
		metav1.ListOptions{
			LabelSelector: RunpodManagedLabel + "=true",
		},
	)
	if err != nil {
		p.logger.Error("Failed to list existing pods", "err", err)
		return make(map[string]RunPodInfo)
	}

	// Create a map of existing RunPod IDs to pod info
	existingRunPodMap := make(map[string]RunPodInfo)
	for _, pod := range existingPods.Items {
		if podID, exists := pod.Annotations[RunpodPodIDAnnotation]; exists {
			existingRunPodMap[podID] = RunPodInfo{
				PodName:   pod.Name,
				Namespace: pod.Namespace,
			}
		}
	}

	return existingRunPodMap
}

// ForceDeletePod forcefully removes a pod from the Kubernetes API
func (p *Provider) ForceDeletePod(namespace, name string) error {
	// Create zero grace period for immediate deletion
	gracePeriod := int64(0)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &[]metav1.DeletionPropagation{metav1.DeletePropagationBackground}[0],
	}

	err := p.clientset.CoreV1().Pods(namespace).Delete(
		context.Background(),
		name,
		deleteOptions,
	)

	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	p.logger.Info("Successfully force deleted pod", "pod", name, "namespace", namespace)
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

// RunInContainer implements the ContainerExecHandlerFunc interface
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	p.logger.Info("RunInContainer called but not supported by RunPod",
		"namespace", namespace,
		"pod", podName,
		"container", containerName)
	return fmt.Errorf("running commands in container is not supported by RunPod")
}

// GetContainerLogs implements the ContainerLogsHandlerFunc interface
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	p.logger.Info("GetContainerLogs called",
		"namespace", namespace,
		"pod", podName,
		"container", containerName)

	// Get the RunPod ID from the pod
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, fmt.Errorf("error getting pod for logs: %w", err)
	}

	podID := pod.Annotations[RunpodPodIDAnnotation]
	if podID == "" {
		return nil, fmt.Errorf("pod %s/%s has no RunPod ID annotation", namespace, podName)
	}

	// If RunPod doesn't support container logs, return an error
	return nil, fmt.Errorf("container logs not supported by RunPod")

	// If RunPod supports logs, you would implement something like:
	/*
	   logs, err := p.runpodClient.GetPodLogs(podID)
	   if err != nil {
	       return nil, fmt.Errorf("failed to get container logs: %w", err)
	   }

	   // Convert string to ReadCloser
	   return io.NopCloser(strings.NewReader(logs)), nil
	*/
}

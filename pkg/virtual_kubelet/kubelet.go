package runpod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/bsvogler/k8s-runpod-kubelet/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"io"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Provider implements the virtual-kubelet provider interface for RunPod
type Provider struct {
	nodeName           string
	clientset          kubernetes.Interface
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
	pods        map[string]*v1.Pod       // Map of podKey -> Pod
	podStatus   map[string]*InstanceInfo // Map of podKey -> InstanceInfo
	podsMutex   sync.RWMutex             // Mutex for thread-safe access to pods maps
	notifyFunc  func(*v1.Pod)            // Function called when pod status changes
	notifyMutex sync.RWMutex             // Mutex for thread-safe access to notify function
	
	// Heartbeat configuration
	heartbeatInterval time.Duration      // Interval between heartbeats
	heartbeatStop     chan struct{}      // Channel to stop heartbeat goroutine
	heartbeatCtx      context.Context    // Context for heartbeat operations
	heartbeatCancel   context.CancelFunc // Cancel function for heartbeat context
}

// RegistrationPayload represents the data sent to the conduit registration endpoint
type RegistrationPayload struct {
	ClusterName  string            `json:"cluster_name"`
	Namespace    string            `json:"namespace"`
	NodeName     string            `json:"node_name"`
	Version      *string           `json:"version"`
	Capabilities []string          `json:"capabilities"`
	Metadata     map[string]string `json:"metadata"`
}

// registerWithConduit registers the kubelet with the conduit service
func (p *Provider) registerWithConduit() error {
	apiToken := os.Getenv("CONDUIT_API_TOKEN")
	if apiToken == "" {
		p.logger.Info("CONDUIT_API_TOKEN unset; skipping GPU Conduit registration")
		return nil
	}

	// Get cluster name from environment or generate one
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "k8s-runpod-cluster"
	}

	// Get namespace (default to kube-system if not set)
	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}

	// Get version info
	version := runtime.Version()

	// Define capabilities
	capabilities := []string{
		"gpu-scheduling",
		"runpod-integration",
		"virtual-kubelet",
	}

	// Create metadata
	metadata := map[string]string{
		"provider":         "runpod",
		"operating_system": p.operatingSystem,
		"internal_ip":      p.internalIP,
		"go_version":       runtime.Version(),
		"arch":             runtime.GOARCH,
		"os":               runtime.GOOS,
	}

	payload := RegistrationPayload{
		ClusterName:  clusterName,
		Namespace:    namespace,
		NodeName:     p.nodeName,
		Version:      &version,
		Capabilities: capabilities,
		Metadata:     metadata,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal registration payload: %w", err)
	}

	// Get conduit host from environment or use default
	conduitHost := os.Getenv("CONDUIT_HOST")
	if conduitHost == "" {
		conduitHost = "https://gpuconduit.io"
	}
	
	// Create HTTP request
	req, err := http.NewRequest("PUT", conduitHost+"/api/kubelet/register", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create registration request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken)

	// Make the request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make registration request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("authentication failed - invalid API token. Please register at %s/dashboard/installation/ to obtain a valid API token", conduitHost)
		}
		return fmt.Errorf("registration failed with status %d: %s. Please check your account status at %s", resp.StatusCode, string(body), conduitHost)
	}

	p.logger.Info("Successfully registered with conduit service",
		"cluster_name", clusterName,
		"namespace", namespace,
		"node_name", p.nodeName)

	// Start heartbeat after successful registration
	p.startHeartbeat()

	return nil
}

// sendHeartbeat sends a heartbeat to the conduit service using the same registration call
func (p *Provider) sendHeartbeat() error {
	apiToken := os.Getenv("CONDUIT_API_TOKEN")
	if apiToken == "" {
		// If no API token, skip heartbeat silently
		return nil
	}

	// Get conduit host from environment or use default
	conduitHost := os.Getenv("CONDUIT_HOST")
	if conduitHost == "" {
		conduitHost = "https://gpuconduit.io"
	}

	// Get cluster name from environment or generate one
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "k8s-runpod-cluster"
	}

	// Get namespace (default to kube-system if not set)
	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}

	// Get version info
	version := runtime.Version()

	// Define capabilities
	capabilities := []string{
		"gpu-scheduling",
		"runpod-integration",
		"virtual-kubelet",
	}

	// Create metadata
	metadata := map[string]string{
		"provider":         "runpod",
		"operating_system": p.operatingSystem,
		"internal_ip":      p.internalIP,
		"go_version":       runtime.Version(),
		"arch":             runtime.GOARCH,
		"os":               runtime.GOOS,
	}

	payload := RegistrationPayload{
		ClusterName:  clusterName,
		Namespace:    namespace,
		NodeName:     p.nodeName,
		Version:      &version,
		Capabilities: capabilities,
		Metadata:     metadata,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat payload: %w", err)
	}

	// Create HTTP request with timeout
	ctx, cancel := context.WithTimeout(p.heartbeatCtx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "PUT", conduitHost+"/api/kubelet/register", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create heartbeat request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken)

	// Make the request
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send heartbeat: %w", err)
	}
	defer resp.Body.Close()

	// Log success or warning based on response
	if resp.StatusCode == http.StatusOK {
		p.logger.Debug("Heartbeat sent successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// startHeartbeat starts sending periodic heartbeats to the conduit service
func (p *Provider) startHeartbeat() {
	// Skip if heartbeat is disabled (interval is 0 or negative)
	if p.heartbeatInterval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(p.heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := p.sendHeartbeat(); err != nil {
					p.logger.Warn("Heartbeat failed", "error", err)
				}
			case <-p.heartbeatStop:
				return
			case <-p.heartbeatCtx.Done():
				return
			}
		}
	}()
}

// StopHeartbeat stops the heartbeat goroutine
func (p *Provider) StopHeartbeat() {
	if p.heartbeatCancel != nil {
		p.heartbeatCancel()
	}
	if p.heartbeatStop != nil {
		close(p.heartbeatStop)
	}
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
			func() {
				defer func() {
					if r := recover(); r != nil {
						p.logger.Error("Panic in syncEndpointSlices recovered", "panic", r)
					}
				}()
				p.syncEndpointSlices()
			}()
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
			p.cleanupStuckTerminatingPods()
			p.cleanupFailedPods()
		}
	}
}

// checkRunPodAPIHealth checks if the RunPod API is available
func (p *Provider) checkRunPodAPIHealth() {
	// Skip check if no API key
	if p.runpodClient.apiKey == "" {
		p.runpodAvailable = false
		p.logger.Warn("runpod key (via RUNPOD_API_KEY environment variable) not set, skipping API health check")
		return
	}

	// Make a simple API call to check health
	_, err := p.runpodClient.makeRESTRequest("GET", "gpuTypes", nil)
	p.runpodAvailable = (err == nil)
}

// NewProvider creates a new RunPod virtual kubelet provider
func NewProvider(ctx context.Context, nodeName, operatingSystem string, internalIP string,
	daemonEndpointPort int, config config.Config, clientset *kubernetes.Clientset, logger *slog.Logger, heartbeatIntervalSec int) (*Provider, error) {

	// Create a new RunPod client and pass the clientset and config
	runpodClient := NewRunPodClient(logger, clientset, &config)

	// Initialize heartbeat context
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)

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
		podStatus:          make(map[string]*InstanceInfo),
		podsMutex:          sync.RWMutex{},
		heartbeatInterval:  time.Duration(heartbeatIntervalSec) * time.Second,
		heartbeatStop:      make(chan struct{}),
		heartbeatCtx:       heartbeatCtx,
		heartbeatCancel:    heartbeatCancel,
	}

	// Initialize provider
	provider.checkRunPodAPIHealth()
	provider.cleanupStuckTerminatingPods()

	// Register with conduit service (mandatory for DRM)
	if err := provider.registerWithConduit(); err != nil {
		return nil, fmt.Errorf("kubelet registration failed: %w", err)
	}

	provider.cleanupFailedPods()

	// Start background processes
	go provider.startPeriodicStatusUpdates()
	go provider.startPeriodicCleanup()
	go provider.startPendingPodProcessor() // Add the pending pod processor

	return provider, nil
}

// isSystemPod checks if a pod is a system pod that should not be deployed to RunPod
func (p *Provider) isSystemPod(pod *v1.Pod) bool {
	// Skip pods in system namespaces
	systemNamespaces := []string{
		"kube-system",
		"kube-public",
		"kube-node-lease",
	}
	for _, ns := range systemNamespaces {
		if pod.Namespace == ns {
			return true
		}
	}

	// Skip pods owned by DaemonSets (typically system components)
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}

	// Skip pods with system labels
	if pod.Labels != nil {
		// CSI drivers
		if pod.Labels["app"] == "csi-cinder-nodeplugin" ||
			pod.Labels["app.kubernetes.io/name"] == "csi-cinder-nodeplugin" {
			return true
		}
		// Other CSI drivers pattern
		if strings.HasPrefix(pod.Labels["app"], "csi-") ||
			strings.HasPrefix(pod.Labels["app.kubernetes.io/name"], "csi-") {
			return true
		}
		// kube-proxy
		if pod.Labels["component"] == "kube-proxy" ||
			pod.Labels["k8s-app"] == "kube-proxy" {
			return true
		}
		// CNI plugins
		if pod.Labels["app"] == "calico-node" ||
			pod.Labels["app"] == "flannel" ||
			pod.Labels["k8s-app"] == "calico-node" ||
			pod.Labels["k8s-app"] == "flannel" {
			return true
		}
	}

	return false
}

// Implementation of required interface methods to fulfill the PodLifecycleHandler interface

// CreatePod takes a Kubernetes Pod and deploys it within the provider
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	// Skip system pods that should not be deployed to RunPod
	if p.isSystemPod(pod) {
		p.logger.Debug("Skipping system pod",
			"pod", pod.Name,
			"namespace", pod.Namespace)
		// Return nil to indicate success but don't actually deploy
		// This prevents the pod from being retried
		return nil
	}

	// Store the pod in our tracking map
	podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
	
	// Get the requested ports for this pod
	requestedPorts := p.runpodClient.GetRequestedPorts(pod)
	
	p.podsMutex.Lock()
	p.pods[podKey] = pod.DeepCopy()
	p.podStatus[podKey] = &InstanceInfo{
		PodName:        pod.Name,
		Namespace:      pod.Namespace,
		Status:         string(PodStarting),
		CreationTime:   time.Now(),
		RequestedPorts: requestedPorts,
		PortsExposed:   false,
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
	// Add datacenter IDs annotation if globally configured
	if p.config.DatacenterIDs != "" && pod.Annotations[DatacenterAnnotation] == "" {
		// Copy pod to add annotation
		podCopy := pod.DeepCopy()
		if podCopy.Annotations == nil {
			podCopy.Annotations = make(map[string]string)
		}
		podCopy.Annotations[DatacenterAnnotation] = p.config.DatacenterIDs

		// Update pod with annotation
		_, err := p.clientset.CoreV1().Pods(pod.Namespace).Update(
			context.Background(),
			podCopy,
			metav1.UpdateOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to update pod with datacenter ID: %w", err)
		}
		pod = podCopy
	}

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

	// Log the parameters being sent to RunPod (excluding env vars for security)
	logParams := make(map[string]interface{})
	for k, v := range params {
		if k != "env" { // Exclude environment variables from logs
			logParams[k] = v
		} else {
			// Just log the count of environment variables
			if envMap, ok := v.(map[string]string); ok {
				logParams["envCount"] = len(envMap)
			}
		}
	}
	paramsJSON, _ := json.MarshalIndent(logParams, "", "  ")
	p.logger.Info("Deploying pod with parameters",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"params", string(paramsJSON))

	// Deploy to RunPod using REST API
	podID, costPerHr, err := p.runpodClient.DeployPodREST(params)
	if err != nil {
		p.logger.Error("Failed to deploy pod to RunPod",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)
		p.emitEvent(pod, v1.EventTypeWarning, "RunPodCreateFailed", err.Error())
		return err
	}
	p.emitEvent(pod, v1.EventTypeNormal, "RunPodCreated", "created RunPod instance "+podID)

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
	podCopy.Annotations[PodIDAnnotation] = podID
	podCopy.Annotations[CostAnnotation] = fmt.Sprintf("%f", costPerHr)

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
		// Get requested ports for new InstanceInfo
		requestedPorts := p.runpodClient.GetRequestedPorts(pod)
		p.podStatus[podKey] = &InstanceInfo{
			ID:             podID,
			CostPerHr:      costPerHr,
			PodName:        pod.Name,
			Namespace:      pod.Namespace,
			Status:         string(PodStarting),
			CreationTime:   time.Now(),
			RequestedPorts: requestedPorts,
			PortsExposed:   false,
		}
	}
	p.podsMutex.Unlock()

	return nil
}

// checkPortsExposed checks if the requested ports are actually exposed by comparing
// with RunPod's port mappings
func (p *Provider) checkPortsExposed(portMappings map[string]int, requestedPorts []string) bool {
	// If no ports were requested, consider it ready (no port requirement)
	if len(requestedPorts) == 0 {
		return true
	}
	
	// Extract the exposed ports from RunPod's port mappings
	// The map key is the internal port (as a string), value is the external port
	exposedPorts := make(map[string]bool)
	for internalPort := range portMappings {
		// RunPod returns the internal port as a string key for TCP ports
		// We need to match it with the requested format "port/protocol"
		exposedPorts[internalPort+"/tcp"] = true
		exposedPorts[internalPort+"/http"] = true
	}
	
	// Check if all requested ports are exposed
	for _, requestedPort := range requestedPorts {
		if exposedPorts[requestedPort] {
			// Port is explicitly in portMappings
			continue
		}
		
		// Handle RunPod's different behavior for HTTP vs TCP ports
		// HTTP ports might not appear in portMappings but are handled by RunPod's proxy
		if strings.HasSuffix(requestedPort, "/http") {
			p.logger.Debug("Assuming HTTP port is available (RunPod handles HTTP through proxy)",
				"requestedPort", requestedPort)
			continue
		}
		
		// For TCP ports, they must be in portMappings to be considered exposed
		p.logger.Debug("Requested TCP port not yet exposed",
			"requestedPort", requestedPort,
			"exposedPorts", exposedPorts)
		return false
	}
	
	return true
}

// getReadinessProbe extracts the httpGet readiness probe from the first container in pod spec.
// Returns nil if no httpGet readiness probe is configured.
func getReadinessProbe(pod *v1.Pod) *v1.Probe {
	container := SelectWorkloadContainer(pod)
	if container == nil {
		return nil
	}
	probe := container.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		return nil
	}
	return probe
}

// buildReadinessProbeURL constructs the URL to check for readiness.
// Supports both TCP ports (via publicIP:externalPort) and HTTP ports (via RunPod proxy).
func buildReadinessProbeURL(podID string, probe *v1.Probe, requestedPorts []string, publicIP string, portMappings map[string]int) string {
	port := probe.HTTPGet.Port.IntValue()
	path := probe.HTTPGet.Path
	portStr := fmt.Sprintf("%d", port)

	// Check if this port is declared as HTTP in the pod annotations
	isHTTPPort := false
	for _, rp := range requestedPorts {
		if rp == portStr+"/http" {
			isHTTPPort = true
			break
		}
	}

	if isHTTPPort {
		// Use RunPod proxy URL for HTTP ports
		return fmt.Sprintf("https://%s-%d.proxy.runpod.net%s", podID, port, path)
	}

	// TCP port — use publicIP + external port mapping
	if publicIP != "" && portMappings != nil {
		if externalPort, ok := portMappings[portStr]; ok {
			return fmt.Sprintf("http://%s:%d%s", publicIP, externalPort, path)
		}
	}

	return "" // Can't reach the pod
}

// performReadinessCheck does an HTTP GET to the readiness probe URL.
// Returns true if the response status is 200-399 (same as K8s convention).
func (p *Provider) performReadinessCheck(url string, timeoutSeconds int) bool {
	if url == "" {
		return false
	}

	timeout := 5 * time.Second
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// K8s readiness probe convention: 200-399 = success
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// isPodReadyForProbe checks if enough time has passed since the pod started running
// to begin readiness probing (respects initialDelaySeconds).
func isPodReadyForProbe(podInfo *InstanceInfo, probe *v1.Probe) bool {
	if podInfo.RunningStartTime.IsZero() {
		return false
	}
	delay := time.Duration(probe.InitialDelaySeconds) * time.Second
	return time.Since(podInfo.RunningStartTime) >= delay
}

// computeIsReady determines the Ready state for a pod, taking readiness probe into account.
// If no probe is configured, Ready = portsExposed (existing behavior).
func (p *Provider) computeIsReady(pod *v1.Pod, podInfo *InstanceInfo) bool {
	if podInfo.Status != string(PodRunning) || !podInfo.PortsExposed {
		return false
	}

	probe := getReadinessProbe(pod)
	if probe == nil {
		// No readiness probe — ports exposed means ready (existing behavior)
		return true
	}

	return podInfo.ReadinessProbeReady
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
	podID := pod.Annotations[PodIDAnnotation]
	if podID != "" {
		// Track the deleted pod for cleanup
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		p.deletedPodsMutex.Lock()
		p.deletedPods[podKey] = podID
		p.deletedPodsMutex.Unlock()

		// Attempt to terminate RunPod instance
		if err := p.terminateRunPodInstance(podID, pod.Name, pod.Namespace, "DeletePod: K8s requested pod deletion (KEDA scale-down or manual)"); err != nil {
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
	pod := p.pods[podKey]
	p.podsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("pod status not found for %s", podKey)
	}

	// For GetPodStatus, we should check current port exposure if the pod is running
	hasExposedPorts := true // Default to true for non-running states
	if podInfo.Status == string(PodRunning) && len(podInfo.RequestedPorts) > 0 && pod != nil {
		// Get current port exposure status from RunPod
		if podID := pod.Annotations[PodIDAnnotation]; podID != "" {
			if detailedStatus, err := p.runpodClient.GetDetailedPodStatus(podID); err == nil {
				hasExposedPorts = p.checkPortsExposed(detailedStatus.PortMappings, podInfo.RequestedPorts)
			}
		}
	}

	// Translate RunPod status to Kubernetes PodStatus
	isReady := p.computeIsReady(pod, podInfo)
	return p.translateRunPodStatus(podInfo.Status, podInfo.StatusMessage, hasExposedPorts, isReady), nil
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

		// Check if pod already has a RunPod ID - if so, skip it
		if podID := pod.Annotations[PodIDAnnotation]; podID != "" {
			p.logger.Debug("Skipping deployment of pod with existing RunPod ID",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"runpodID", podID)
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
		podID := pod.Annotations[PodIDAnnotation]
		if podID == "" {
			continue
		}

		// Get detailed status from RunPod API (includes port mappings)
		detailedStatus, err := p.runpodClient.GetDetailedPodStatus(podID)
		if err != nil {
			p.logger.Error("Failed to get detailed pod status from RunPod API",
				"pod", pod.Name, "namespace", pod.Namespace,
				"runpodID", podID, "error", err)

			// Skip this update cycle for this pod - don't set to NotFound immediately
			continue
		}

		// Convert detailed status to our PodStatus enum
		status := PodStatus(detailedStatus.DesiredStatus)

		// If status is NOT_FOUND, use shared handler
		if status == PodNotFound {
			p.handleMissingRunPodInstance(pod, podKey, podID)
			continue // Skip the rest of the status update logic
		}

		// Check if the requested ports are exposed
		hasExposedPorts := p.checkPortsExposed(detailedStatus.PortMappings, podInfo.RequestedPorts)

		// Update network info for readiness probes
		p.podsMutex.Lock()
		podInfo.PublicIP = detailedStatus.PublicIP
		podInfo.ExternalPortMappings = detailedStatus.PortMappings
		// Ленивая инициализация RequestedPorts — если LoadRunning не заполнил.
		if len(podInfo.RequestedPorts) == 0 && pod != nil {
			podInfo.RequestedPorts = p.runpodClient.GetRequestedPorts(pod)
		}
		// Track when pod first enters RUNNING (for initialDelaySeconds)
		if string(status) == string(PodRunning) && podInfo.RunningStartTime.IsZero() {
			podInfo.RunningStartTime = time.Now()
		}
		p.podsMutex.Unlock()

		// Update pod info if status changed OR port exposure changed
		statusChanged := string(status) != podInfo.Status
		portsExposureChanged := hasExposedPorts != podInfo.PortsExposed
		
		if statusChanged || portsExposureChanged {
			// Update status in our tracking map
			p.podsMutex.Lock()
			oldStatus := podInfo.Status
			podInfo.Status = string(status)
			podInfo.PortsExposed = hasExposedPorts
			p.podStatus[podKey] = podInfo
			p.podsMutex.Unlock()

			// Create the new Kubernetes PodStatus with port exposure information
			isReady := p.computeIsReady(pod, podInfo)
			newStatus := p.translateRunPodStatus(string(status), podInfo.StatusMessage, hasExposedPorts, isReady)

			// Keep existing container state if possible
			if len(pod.Status.ContainerStatuses) > 0 && newStatus != nil {
				// Make sure we don't lose any existing container information
				p.mergeContainerStatus(newStatus, pod.Status.ContainerStatuses[0])
			}

			if statusChanged {
				p.logger.Info("Pod status changed",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"prevStatus", oldStatus,
					"newStatus", string(status),
					"portsExposed", hasExposedPorts,
					"requestedPorts", podInfo.RequestedPorts)
				if status == PodRunning {
					p.emitEvent(pod, v1.EventTypeNormal, "RunPodRunning", "RunPod instance is running")
				}
			} else if portsExposureChanged {
				p.logger.Info("Port exposure changed",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"status", string(status),
					"portsExposed", hasExposedPorts,
					"requestedPorts", podInfo.RequestedPorts)
			}

			// Handle pod completion if needed
			if status == PodExited {
				p.handlePodCompletion(pod, podInfo)
			}

			// Try to update the pod status directly in the Kubernetes API first
			// This is more reliable than using the notifyFunc
			err := p.updatePodStatusInK8s(pod, newStatus)
			if err != nil {
				p.logger.Error("Failed to update pod status in Kubernetes API",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"error", err)

				// Fall back to traditional notification method
				updatedPod := pod.DeepCopy()
				updatedPod.Status = *newStatus

				// Update our local tracking with the updated pod
				p.podsMutex.Lock()
				p.pods[podKey] = updatedPod
				p.podsMutex.Unlock()

				// Notify status change if a notify function is registered
				p.notifyMutex.RLock()
				notifyFunc := p.notifyFunc
				p.notifyMutex.RUnlock()

				if notifyFunc != nil {
					// Use defer-recover to prevent panic
					func() {
						defer func() {
							if r := recover(); r != nil {
								p.logger.Error("Panic in notifyFunc recovered",
									"pod", pod.Name,
									"namespace", pod.Namespace,
									"panic", r)
							}
						}()

						p.logger.Debug("Calling notifyFunc for pod status update",
							"pod", updatedPod.Name,
							"namespace", updatedPod.Namespace,
							"status", updatedPod.Status.Phase)

						notifyFunc(updatedPod)
					}()
				} else {
					p.logger.Warn("No notifyFunc registered for pod status updates")
				}
			} else {
				// Update was successful through API, update our local cache
				// Get the latest pod state to stay in sync
				latestPod, err := p.clientset.CoreV1().Pods(pod.Namespace).Get(
					context.Background(),
					pod.Name,
					metav1.GetOptions{},
				)
				if err == nil {
					p.podsMutex.Lock()
					p.pods[podKey] = latestPod
					p.podsMutex.Unlock()
				}
			}
		}

		// Readiness probe check — runs independently of status/port changes
		probe := getReadinessProbe(pod)
		if probe != nil && podInfo.Status == string(PodRunning) && podInfo.PortsExposed && !podInfo.ReadinessProbeReady {
			if isPodReadyForProbe(podInfo, probe) {
				// Check if enough time passed since last check (respect periodSeconds)
				period := time.Duration(probe.PeriodSeconds) * time.Second
				if period == 0 {
					period = 10 * time.Second // K8s default
				}
				if time.Since(podInfo.ReadinessLastCheck) >= period {
					probeURL := buildReadinessProbeURL(podID, probe, podInfo.RequestedPorts, podInfo.PublicIP, podInfo.ExternalPortMappings)
					timeoutSeconds := int(probe.TimeoutSeconds)
					if timeoutSeconds == 0 {
						timeoutSeconds = 1 // K8s default
					}
					success := p.performReadinessCheck(probeURL, timeoutSeconds)

					p.podsMutex.Lock()
					podInfo.ReadinessLastCheck = time.Now()
					if success {
						podInfo.ReadinessProbeReady = true
						podInfo.ReadinessFailCount = 0
						p.logger.Info("Readiness probe passed",
							"pod", pod.Name,
							"namespace", pod.Namespace,
							"url", probeURL)
					} else {
						podInfo.ReadinessFailCount++
						failureThreshold := int(probe.FailureThreshold)
						if failureThreshold == 0 {
							failureThreshold = 3 // K8s default
						}
						p.logger.Debug("Readiness probe failed",
							"pod", pod.Name,
							"namespace", pod.Namespace,
							"url", probeURL,
							"failCount", podInfo.ReadinessFailCount,
							"threshold", failureThreshold)
					}
					p.podsMutex.Unlock()

					// If readiness state just changed to true, notify K8s
					if success {
						isReady := true
						newStatus := p.translateRunPodStatus(podInfo.Status, podInfo.StatusMessage, podInfo.PortsExposed, isReady)
						if err := p.updatePodStatusInK8s(pod, newStatus); err != nil {
							p.logger.Error("Failed to update readiness status in K8s",
								"pod", pod.Name,
								"namespace", pod.Namespace,
								"error", err)
						}
					}
				}
			}
		}
	}
}

// Helper function to translate RunPod status to Kubernetes PodPhase
// for cleaner logging and debugging
func translateRunPodStatusToPhase(runpodStatus string) string {
	switch runpodStatus {
	case string(PodRunning):
		return string(v1.PodRunning)
	case string(PodStarting):
		return string(v1.PodPending)
	case string(PodExited):
		return string(v1.PodSucceeded) // Default to succeeded, handlePodCompletion will set to Failed if needed
	case string(PodTerminating):
		return string(v1.PodRunning)
	case string(PodTerminated):
		return string(v1.PodSucceeded)
	case string(PodNotFound):
		return string(v1.PodUnknown)
	default:
		return string(v1.PodUnknown)
	}
}

// handlePodCompletion processes a pod that has completed execution
func (p *Provider) handlePodCompletion(pod *v1.Pod, podInfo *InstanceInfo) {
	// Get the RunPod ID from annotations
	podID := pod.Annotations[PodIDAnnotation]
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

	// Update pod status - for exited pods, port exposure is not relevant
	podStatus := p.translateRunPodStatus(string(PodExited), exitMessage, true, false)
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
			// ProviderID identifies this node to cloud controller managers (CCMs).
			// Without it, CCMs like OpenStack CCM treat the node as a cloud instance,
			// fail to find it in the cloud provider, and delete it — which wipes
			// Spec.Taints on every recreation cycle.
			ProviderID: "runpod://" + p.nodeName,
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
				v1.ResourceCPU:    resource.MustParse(p.config.NodeCapacityCPU),
				v1.ResourceMemory: resource.MustParse(p.config.NodeCapacityMemory),
				v1.ResourcePods:   resource.MustParse(p.config.NodeCapacityPods),
				"nvidia.com/gpu":  resource.MustParse(p.config.NodeCapacityGPU),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(p.config.NodeCapacityCPU),
				v1.ResourceMemory: resource.MustParse(p.config.NodeCapacityMemory),
				v1.ResourcePods:   resource.MustParse(p.config.NodeCapacityPods),
				"nvidia.com/gpu":  resource.MustParse(p.config.NodeCapacityGPU),
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

			if err := p.terminateRunPodInstance(runpodID, name, namespace, "cleanupDeletedPods: pod gone from K8s, terminating RunPod instance"); err != nil {
				p.logger.Error("Failed to terminate RunPod instance during cleanup",
					"runpodID", runpodID, "err", err)
			}

			// Remove from tracking
			delete(p.deletedPods, podKey)
		}
	}
}

// failedPodTTL is the minimum age of a Failed pod before cleanupFailedPods removes it.
// Buffers in-flight reconciliation and leaves a short window for operator inspection;
// real diagnostics live in the provider's logs, not on the pod object.
const failedPodTTL = 5 * time.Minute

// cleanupFailedPods removes terminal Failed pods on this node older than failedPodTTL.
// Closes the GC gap left by processPendingPods / translateRunPodStatus, which mark pods
// Failed but never delete them — the cluster-wide podGC threshold (default 12500) won't
// fire on a single virtual node before zombies accumulate into etcd pressure.
// Authority to delete here rests on spec.nodeName=<virtual node>: this node has no real
// kubelet, so any Failed pod on it was Failed-marked by this provider.
func (p *Provider) cleanupFailedPods() {
	pods, err := p.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s,status.phase=%s", p.nodeName, v1.PodFailed),
		},
	)
	if err != nil {
		p.logger.Error("cleanup_failed_pods list failed", "err", err)
		return
	}

	scanned := len(pods.Items)
	deleted := 0
	skipped := 0

	for i := range pods.Items {
		pod := &pods.Items[i]

		// Belt-and-suspenders: the field selector already filters phase server-side,
		// but fake clients ignore status field selectors and a future API change shouldn't
		// silently turn this into a delete-anything loop.
		if pod.Status.Phase != v1.PodFailed {
			skipped++
			continue
		}

		if pod.DeletionTimestamp != nil {
			skipped++
			continue
		}

		age := failedPodAge(pod)
		if age < failedPodTTL {
			skipped++
			continue
		}

		reason := fmt.Sprintf("cleanup_failed_pods: TTL expired (status.reason=%q, age=%s)", pod.Status.Reason, age.Truncate(time.Second))
		if err := p.forceDeleteK8sPod(pod.Namespace, pod.Name, reason); err != nil {
			p.logger.Error("cleanup_failed_pods delete failed",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"reason", pod.Status.Reason,
				"age", age,
				"err", err)
			continue
		}
		deleted++
	}

	if scanned > 0 || deleted > 0 {
		p.logger.Info("cleanup_failed_pods", "scanned", scanned, "deleted", deleted, "skipped", skipped, "ttl", failedPodTTL)
	}
}

// failedPodAge approximates time since the pod entered Failed by taking the most recent
// observable timestamp on the object. Avoids storing our own "failed-at" annotation,
// so the value survives provider restart without persistent state.
func failedPodAge(pod *v1.Pod) time.Duration {
	latest := pod.CreationTimestamp.Time
	for _, c := range pod.Status.Conditions {
		if c.LastTransitionTime.After(latest) {
			latest = c.LastTransitionTime.Time
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.FinishedAt.After(latest) {
			latest = cs.State.Terminated.FinishedAt.Time
		}
	}
	return time.Since(latest)
}

// cleanupStuckTerminatingPods finds pods that are stuck in Terminating state
// and forcefully removes them if they no longer exist in RunPod
func (p *Provider) cleanupStuckTerminatingPods() {
	// Get all pods in all namespaces that are stuck in Terminating state
	pods, err := p.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", p.nodeName),
		},
	)
	if err != nil {
		p.logger.Error("Failed to list pods for termination cleanup", "err", err)
		return
	}

	terminatingCount := 0
	deletedCount := 0
	for _, pod := range pods.Items {
		// Check if pod is terminating (has a deletion timestamp but still exists)
		if pod.DeletionTimestamp != nil {
			terminatingCount++

			// Get the RunPod ID from annotations
			podID := pod.Annotations[PodIDAnnotation]
			if podID == "" {
				// Pod has no RunPod ID - it was never deployed to RunPod
				// We should force delete it as it has nothing to clean up remotely
				p.logger.Info("Force deleting terminating pod with no RunPod ID",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"deletionTimestamp", pod.DeletionTimestamp)

				err = p.forceDeleteK8sPod(pod.Namespace, pod.Name, "cleanupStuckTerminating: pod has no RunPod ID, never deployed")
				if err != nil {
					p.logger.Error("Failed to force delete terminating pod without RunPod ID",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"error", err)
				} else {
					deletedCount++
				}
				continue
			}

			// Check if the pod still exists in RunPod
			status, err := p.runpodClient.GetPodStatusREST(podID)
			if err != nil {
				p.logger.Error("Failed to check pod status in RunPod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"runpodID", podID,
					"error", err)

				// Even if we can't check the status, if the pod has been terminating
				// for a long time, we should try to force delete it
				terminatingDuration := time.Since(pod.DeletionTimestamp.Time)
				if terminatingDuration > 10*time.Minute {
					p.logger.Info("Pod has been terminating for too long with status check errors, force deleting",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"runpodID", podID,
						"terminatingDuration", terminatingDuration)

					err = p.forceDeleteK8sPod(pod.Namespace, pod.Name, fmt.Sprintf("cleanupStuckTerminating: terminating >10min with status check errors, runpodID=%s", podID))
					if err != nil {
						p.logger.Error("Failed to force delete terminating pod with status check errors",
							"pod", pod.Name,
							"namespace", pod.Namespace,
							"error", err)
					} else {
						deletedCount++
					}
				}

				continue
			}

			// If pod doesn't exist in RunPod or is in terminated/exited state, force delete it from Kubernetes
			if status == PodNotFound || status == PodExited || status == PodTerminated {
				p.logger.Info("Force deleting stuck terminating pod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"runpodID", podID,
					"runpodStatus", status)

				// Force delete the pod
				err = p.forceDeleteK8sPod(pod.Namespace, pod.Name, fmt.Sprintf("cleanupStuckTerminating: RunPod instance gone/exited/terminated (status=%s), runpodID=%s", status, podID))
				if err != nil {
					p.logger.Error("Failed to force delete terminating pod",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"error", err)
				} else {
					deletedCount++
				}
			} else {
				p.logger.Info("Pod is terminating but RunPod instance still exists",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"runpodID", podID,
					"runpodStatus", status)

				// If pod is still in RunPod but has been terminating for too long, try to terminate it again
				terminatingDuration := time.Since(pod.DeletionTimestamp.Time)
				if terminatingDuration > 5*time.Minute {
					p.logger.Info("Pod has been terminating for too long, re-attempting RunPod termination",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"runpodID", podID,
						"terminatingDuration", terminatingDuration)

					// Try to terminate the RunPod instance again
					if err := p.terminateRunPodInstance(podID, pod.Name, pod.Namespace, fmt.Sprintf("cleanupStuckTerminating: re-terminate after >5min terminating, RunPod still alive (status=%s)", status)); err != nil {
						p.logger.Error("Failed to re-terminate RunPod instance",
							"pod", pod.Name,
							"namespace", pod.Namespace,
							"runpodID", podID,
							"error", err)
					}

					// If it's been terminating for an extremely long time, force delete regardless of remote status
					if terminatingDuration > 15*time.Minute {
						p.logger.Info("Pod has been terminating for too long, force deleting despite remote instance",
							"pod", pod.Name,
							"namespace", pod.Namespace,
							"runpodID", podID,
							"runpodStatus", status,
							"terminatingDuration", terminatingDuration)

						err = p.forceDeleteK8sPod(pod.Namespace, pod.Name, fmt.Sprintf("cleanupStuckTerminating: force delete after >15min despite remote instance (status=%s, runpodID=%s)", status, podID))
						if err != nil {
							p.logger.Error("Failed to force delete long-terminating pod",
								"pod", pod.Name,
								"namespace", pod.Namespace,
								"error", err)
						} else {
							deletedCount++
						}
					}
				}
			}
		}
	}

	p.logger.Debug("Terminating pod cleanup complete",
		"found", terminatingCount,
		"forcefullyDeleted", deletedCount,
		"node", p.nodeName)
}

// LoadRunning loads existing pods and reconciles state between Kubernetes and RunPod
func (p *Provider) LoadRunning() {
	// Skip check if no API key
	if p.runpodClient.apiKey == "" {
		p.runpodAvailable = false
		p.logger.Warn("RunPod API key not set, skipping LoadRunning")
		return
	}

	// Step 1: Get all pods assigned to this node from Kubernetes
	k8sPods, err := p.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", p.nodeName),
		},
	)
	if err != nil {
		p.logger.Error("Failed to list pods assigned to this node", "error", err)
		return
	}

	p.logger.Debug("Found pods assigned to this node", "count", len(k8sPods.Items))

	// Step 2: Fetch RunPod instances from API
	runningPods, exitedPods, ok := p.fetchRunPodInstances()
	if !ok {
		p.runpodAvailable = false
		return
	}

	// API is available
	p.runpodAvailable = true

	// Step 3: Create a map of RunPod IDs to instances for lookup
	runpodInstanceMap := make(map[string]RunPodInstance)
	for _, instance := range append(runningPods, exitedPods...) {
		runpodInstanceMap[instance.ID] = instance
	}

	// Step 4: Process kubernetes pods assigned to this node
	for _, pod := range k8sPods.Items {
		// Skip system pods
		if p.isSystemPod(&pod) {
			p.logger.Debug("Skipping system pod in LoadRunning",
				"pod", pod.Name,
				"namespace", pod.Namespace)
			continue
		}

		// Skip pods that are already completed, failed, or terminating
		if pod.Status.Phase == v1.PodSucceeded ||
			pod.Status.Phase == v1.PodFailed ||
			pod.DeletionTimestamp != nil {
			// If a pod is being deleted (has DeletionTimestamp), skip it
			p.logger.Debug("Skipping pod that is completed, failed, or terminating",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"phase", pod.Status.Phase,
				"deletionTimestamp", pod.DeletionTimestamp)
			continue
		}

		podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)

		// First check if we're already tracking this pod
		p.podsMutex.RLock()
		_, alreadyTracking := p.pods[podKey]
		p.podsMutex.RUnlock()

		if alreadyTracking {
			// Skip pods we're already tracking to avoid redundancy with CreatePod
			p.logger.Debug("Skipping pod already tracked by controller",
				"pod", pod.Name,
				"namespace", pod.Namespace)
			continue
		}

		// Store in our tracking map since we're not tracking it yet
		p.podsMutex.Lock()
		p.pods[podKey] = pod.DeepCopy()

		// Check if pod already has a RunPod ID annotation
		podID, hasRunpodID := pod.Annotations[PodIDAnnotation]

		if hasRunpodID {
			// Pod already has RunPod ID - check if it exists in RunPod
			if instance, exists := runpodInstanceMap[podID]; exists {
				// Pod exists in RunPod - update status
				p.logger.Info("Found existing RunPod instance for pod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"runpodID", podID)

				// Map CurrentStatus from API to our internal status
				podStatus := string(PodRunning)
				if instance.CurrentStatus == "EXITED" {
					podStatus = string(PodExited)
				} else if instance.CurrentStatus == "STARTING" {
					podStatus = string(PodStarting)
				} else if instance.CurrentStatus == "TERMINATING" {
					podStatus = string(PodTerminating)
				}

				// Update pod status in cache
				p.podStatus[podKey] = &InstanceInfo{
					ID:             podID,
					PodName:        pod.Name,
					Namespace:      pod.Namespace,
					Status:         podStatus,
					CostPerHr:      instance.CostPerHr,
					CreationTime:   pod.CreationTimestamp.Time,
					RequestedPorts: p.runpodClient.GetRequestedPorts(&pod),
					PortsExposed:   false,
				}
			} else {
				// Pod has ID but instance not found in RunPod - use shared handler
				p.handleMissingRunPodInstance(&pod, podKey, podID)
			}
		} else {
			// Pod doesn't have RunPod ID - add to tracking as pending
			// The periodic pod processor will handle deployment
			p.logger.Info("Found pod with no RunPod ID - marking internally as pending",
				"pod", pod.Name,
				"namespace", pod.Namespace)

			// Mark as pending deployment
			p.podStatus[podKey] = &InstanceInfo{
				PodName:        pod.Name,
				Namespace:      pod.Namespace,
				Status:         string(PodStarting),
				CreationTime:   pod.CreationTimestamp.Time,
				RequestedPorts: p.runpodClient.GetRequestedPorts(&pod),
				PortsExposed:   false,
			}

			// Don't try to deploy here - let the existing periodic processor handle it
			// This avoids redundancy with CreatePod
		}
		p.podsMutex.Unlock()
	}

	// Step 5: Find RunPod instances not represented in Kubernetes
	existingRunPodMap := p.mapExistingRunPodInstances()
	// Only check running pods - we don't want to create K8s pods for stopped/exited RunPod instances
	for _, runpodInstance := range runningPods {
		if _, exists := existingRunPodMap[runpodInstance.ID]; !exists {
			// This RunPod instance has no representation in K8s
			p.logger.Info("Found running RunPod instance with no Kubernetes pod",
				"runpodID", runpodInstance.ID,
				"name", runpodInstance.Name,
				"status", runpodInstance.CurrentStatus)

			// Create virtual pod for this RunPod instance
			p.CreateVirtualPod(runpodInstance)
		}
	}
	
	// Log exited pods for visibility but don't create K8s pods for them
	for _, runpodInstance := range exitedPods {
		if _, exists := existingRunPodMap[runpodInstance.ID]; !exists {
			p.logger.Debug("Found exited RunPod instance with no Kubernetes pod (ignoring)",
				"runpodID", runpodInstance.ID,
				"name", runpodInstance.Name,
				"status", runpodInstance.CurrentStatus)
		}
	}
}

// fetchRunPodInstances fetches both running and exited pods from the RunPod API
func (p *Provider) fetchRunPodInstances() (running []RunPodInstance, exited []RunPodInstance, ok bool) {
	// Make a request to the RunPod API to get all running pods.
	// NOTE: RunPod API only supports desiredStatus=RUNNING|EXITED|TERMINATED.
	// STARTING pods are not queryable — they will appear as RUNNING once ready.
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
func (p *Provider) processRunPodInstance(runpodInstance RunPodInstance, existingRunPodMap map[string]InstanceInfo) {
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
	podLabels[PodIDAnnotation] = runpodInstance.ID

	// Create annotations for the Pod
	podAnnotations := make(map[string]string)
	podAnnotations[PodIDAnnotation] = runpodInstance.ID
	podAnnotations[CostAnnotation] = fmt.Sprintf("%f", runpodInstance.CostPerHr)
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
func (p *Provider) mapExistingRunPodInstances() map[string]InstanceInfo {
	// Get existing pods in the cluster assigned to this virtual node
	existingPods, err := p.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", p.nodeName),
		},
	)
	if err != nil {
		p.logger.Error("Failed to list existing pods", "err", err)
		return make(map[string]InstanceInfo)
	}

	// Create a map of existing RunPod IDs to pod info
	existingRunPodMap := make(map[string]InstanceInfo)
	for _, pod := range existingPods.Items {
		if podID, exists := pod.Annotations[PodIDAnnotation]; exists {
			existingRunPodMap[podID] = InstanceInfo{
				PodName:   pod.Name,
				Namespace: pod.Namespace,
			}
		}
	}

	return existingRunPodMap
}

// handleMissingRunPodInstance handles the case where a pod has a RunPod ID annotation
// but the instance is not found in the RunPod API. This is used both during startup
// and periodic reconciliation to ensure consistent behavior.
func (p *Provider) handleMissingRunPodInstance(pod *v1.Pod, podKey string, runpodID string) {
	p.logger.Warn("Pod has RunPod ID but instance not found in RunPod API",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"runpodID", runpodID)

	// Get current version of the pod and update it
	currentPod, err := p.clientset.CoreV1().Pods(pod.Namespace).Get(
		context.Background(),
		pod.Name,
		metav1.GetOptions{},
	)
	if err == nil {
		// Make a deep copy to avoid modifying the cache
		podCopy := currentPod.DeepCopy()

		// Remove the RunPod annotations
		if podCopy.Annotations != nil {
			delete(podCopy.Annotations, PodIDAnnotation)
			delete(podCopy.Annotations, CostAnnotation)

			// Update the pod
			_, updateErr := p.clientset.CoreV1().Pods(podCopy.Namespace).Update(
				context.Background(),
				podCopy,
				metav1.UpdateOptions{},
			)
			if updateErr != nil {
				p.logger.Error("Failed to remove RunPod annotations from pod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"error", updateErr)
			} else {
				p.logger.Info("Removed RunPod annotations from pod",
					"pod", pod.Name,
					"namespace", pod.Namespace)

				// Update our local cache as well
				p.podsMutex.Lock()
				p.pods[podKey] = podCopy
				if pInfo, exists := p.podStatus[podKey]; exists {
					pInfo.ID = "" // Clear the RunPod ID
					pInfo.Status = string(PodExited)
					pInfo.StatusMessage = "RunPod instance not found"
					p.podStatus[podKey] = pInfo
				}
				p.podsMutex.Unlock()
			}
		}
	} else {
		p.logger.Error("Failed to get current pod state",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)
	}

	// Update pod status to Failed to prevent redeployment - port exposure is not relevant for failed pods
	failedStatus := p.translateRunPodStatus(string(PodNotFound), "RunPod instance was deleted", true, false)
	err = p.updatePodStatusInK8s(currentPod, failedStatus)
	if err != nil {
		p.logger.Error("Failed to update pod status to Failed",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"error", err)
	}
}

// terminateRunPodInstance wraps TerminatePod with a reason for debugging pod lifecycle.
func (p *Provider) terminateRunPodInstance(podID, podName, namespace, reason string) error {
	p.logger.Warn(">>> TERMINATING RunPod instance",
		"reason", reason,
		"pod", podName,
		"namespace", namespace,
		"runpodID", podID)
	return p.runpodClient.TerminatePod(podID)
}

// forceDeleteK8sPod wraps ForceDeletePod with a reason for debugging pod lifecycle.
func (p *Provider) forceDeleteK8sPod(namespace, name, reason string) error {
	p.logger.Warn(">>> FORCE DELETING K8s pod",
		"reason", reason,
		"pod", name,
		"namespace", namespace)
	return p.ForceDeletePod(namespace, name)
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

func (p *Provider) mergeContainerStatus(newStatus *v1.PodStatus, existingContainerStatus v1.ContainerStatus) {
	if newStatus == nil || len(newStatus.ContainerStatuses) == 0 {
		return
	}

	// Keep certain fields from existing container status
	if existingContainerStatus.ContainerID != "" {
		newStatus.ContainerStatuses[0].ContainerID = existingContainerStatus.ContainerID
	}

	if existingContainerStatus.ImageID != "" {
		newStatus.ContainerStatuses[0].ImageID = existingContainerStatus.ImageID
	}

	// If the container was previously started, keep that information
	if existingContainerStatus.Started != nil && *existingContainerStatus.Started {
		trueVal := true
		newStatus.ContainerStatuses[0].Started = &trueVal
	}

	// Preserve restart count
	newStatus.ContainerStatuses[0].RestartCount = existingContainerStatus.RestartCount
}

func (p *Provider) updatePodStatusInK8s(pod *v1.Pod, newStatus *v1.PodStatus) error {
	// Create a patch to update just the status
	patchBytes, err := json.Marshal(map[string]interface{}{
		"status": newStatus,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal status patch: %w", err)
	}

	// Try to patch the pod status
	_, err = p.clientset.CoreV1().Pods(pod.Namespace).Patch(
		context.Background(),
		pod.Name,
		types.StrategicMergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		return fmt.Errorf("failed to patch pod status: %w", err)
	}

	return nil
}

// translateRunPodStatus converts a RunPod status string to a Kubernetes PodStatus.
// isReady controls the Ready condition: for pods with readinessProbe it reflects probe state,
// for pods without it reflects port exposure state.
func (p *Provider) translateRunPodStatus(runpodStatus string, statusMessage string, hasExposedPorts bool, isReady bool) *v1.PodStatus {
	now := metav1.NewTime(time.Now())
	startTime := metav1.NewTime(now.Add(-1 * time.Hour)) // Default start time

	// Initialize the container status with default values
	containerStatus := v1.ContainerStatus{
		Name:         "runpod-container",
		RestartCount: 0,
		Ready:        false,
		Image:        "runpod-image",
		ImageID:      "",
		ContainerID:  "runpod://",
	}

	// Default to Unknown phase
	phase := v1.PodUnknown

	// Set container state and phase based on RunPod status and port exposure
	switch runpodStatus {
	case string(PodRunning):
		if hasExposedPorts {
			// Container is running; readiness depends on probe (or port exposure if no probe)
			phase = v1.PodRunning
			containerStatus.State = v1.ContainerState{
				Running: &v1.ContainerStateRunning{
					StartedAt: startTime,
				},
			}
			containerStatus.Ready = isReady
			trueVal := true
			containerStatus.Started = &trueVal
		} else {
			// RunPod reports RUNNING but ports not yet exposed - keep in pending state
			phase = v1.PodPending
			containerStatus.State = v1.ContainerState{
				Waiting: &v1.ContainerStateWaiting{
					Reason:  "ContainerCreating",
					Message: "Container reported as running but ports not yet exposed",
				},
			}
			falseVal := false
			containerStatus.Started = &falseVal
		}

	case string(PodStarting):
		phase = v1.PodPending
		containerStatus.State = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  "ContainerCreating",
				Message: statusMessage,
			},
		}
		falseVal := false
		containerStatus.Started = &falseVal

	case string(PodExited):
		exitCode := 0
		reason := "Completed"

		if strings.Contains(strings.ToLower(statusMessage), "error") ||
			strings.Contains(strings.ToLower(statusMessage), "fail") {
			exitCode = 1
			reason = "Error"
			phase = v1.PodFailed
		} else {
			phase = v1.PodSucceeded
		}

		containerStatus.State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode:   int32(exitCode),
				Reason:     reason,
				Message:    statusMessage,
				StartedAt:  startTime,
				FinishedAt: now,
			},
		}
		falseVal := false
		containerStatus.Started = &falseVal

	case string(PodTerminating):
		phase = v1.PodRunning
		containerStatus.State = v1.ContainerState{
			Running: &v1.ContainerStateRunning{
				StartedAt: startTime,
			},
		}
		containerStatus.Ready = true
		trueVal := true
		containerStatus.Started = &trueVal

	case string(PodTerminated):
		phase = v1.PodSucceeded
		containerStatus.State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode:   0,
				Reason:     "Terminated",
				Message:    statusMessage,
				StartedAt:  startTime,
				FinishedAt: now,
			},
		}
		falseVal := false
		containerStatus.Started = &falseVal

	case string(PodNotFound):
		phase = v1.PodFailed
		containerStatus.State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				ExitCode:   1,
				Reason:     "PodDeleted",
				Message:    "Pod was deleted from RunPod API",
				StartedAt:  startTime,
				FinishedAt: now,
			},
		}
		falseVal := false
		containerStatus.Started = &falseVal

	default:
		containerStatus.State = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  "ContainerStatusUnknown",
				Message: fmt.Sprintf("Unknown RunPod status: %s", runpodStatus),
			},
		}
		falseVal := false
		containerStatus.Started = &falseVal
	}

	// Create pod conditions
	readyCondition := v1.ConditionFalse
	if phase == v1.PodRunning && isReady {
		readyCondition = v1.ConditionTrue
	}

	podConditions := []v1.PodCondition{
		{
			Type:               v1.PodScheduled,
			Status:             v1.ConditionTrue,
			LastTransitionTime: now,
			LastProbeTime:      now,
		},
		{
			Type:               v1.PodInitialized,
			Status:             v1.ConditionTrue,
			LastTransitionTime: now,
			LastProbeTime:      now,
		},
		{
			Type:               v1.PodReady,
			Status:             readyCondition,
			LastTransitionTime: now,
			LastProbeTime:      now,
		},
		{
			Type:               v1.ContainersReady,
			Status:             readyCondition,
			LastTransitionTime: now,
			LastProbeTime:      now,
		},
	}

	// Create the pod status
	podStatus := &v1.PodStatus{
		Phase:             phase,
		Conditions:        podConditions,
		Message:           statusMessage,
		HostIP:            "10.0.0.1", // Placeholder
		PodIP:             "10.0.0.2", // Placeholder
		ContainerStatuses: []v1.ContainerStatus{containerStatus},
		StartTime:         &startTime,
		//qosClass is immutable
	}

	return podStatus
}

// RunInContainer / GetContainerLogs live in exec.go.

// syncEndpointSlices creates or updates EndpointSlices for Services that target RunPod pods.
// RunPod pods have fake cluster IPs (10.0.0.2) that are not routable, so real public IPs
// and external port mappings from RunPod are used instead.
func (p *Provider) syncEndpointSlices() {
	// Collect a snapshot of running pods with their status under read lock.
	type podSnapshot struct {
		pod     *v1.Pod
		podInfo *InstanceInfo
	}

	p.podsMutex.RLock()
	snapshots := make([]podSnapshot, 0, len(p.pods))
	for _, pod := range p.pods {
		podKey := fmt.Sprintf("%s-%s", pod.Namespace, pod.Name)
		podInfo := p.podStatus[podKey]
		if podInfo == nil {
			continue
		}
		snapshots = append(snapshots, podSnapshot{pod: pod, podInfo: podInfo})
	}
	p.podsMutex.RUnlock()

	// Group pods by namespace so we can list Services per namespace once.
	nsPods := make(map[string][]podSnapshot)
	for _, snap := range snapshots {
		ns := snap.pod.Namespace
		nsPods[ns] = append(nsPods[ns], snap)
	}

	for ns, pods := range nsPods {
		// List Services in the namespace that opt in to managed endpoints.
		svcList, err := p.clientset.CoreV1().Services(ns).List(
			context.Background(),
			metav1.ListOptions{},
		)
		if err != nil {
			p.logger.Error("syncEndpointSlices: failed to list services",
				"namespace", ns, "error", err)
			continue
		}

		for i := range svcList.Items {
			svc := &svcList.Items[i]
			if svc.Annotations["runpod.io/managed-endpoints"] != "true" {
				continue
			}

			// Build the set of label selector requirements from the annotation.
			// Using annotation instead of svc.Spec.Selector so the Service has no selector
			// and the default endpoint controller does not create conflicting EndpointSlices.
			selectorStr := svc.Annotations["runpod.io/pod-selector"]
			if selectorStr == "" {
				continue
			}
			selector := make(map[string]string)
			for _, pair := range strings.Split(selectorStr, ",") {
				kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
				if len(kv) == 2 {
					selector[kv[0]] = kv[1]
				}
			}

			// One EndpointSlice per pod — each RunPod instance has its own external port.
			// kube-proxy merges all slices for a Service and balances across them.
			managedBy := "runpod-kubelet"
			addrType := discoveryv1.AddressTypeIPv4
			isController := true

			// Построим маппинг internalPort → Service port name.
			// kube-proxy матчит EndpointSlice порты с Service портами по имени.
			svcPortNames := make(map[string]string) // "8075" → "http"
			for _, sp := range svc.Spec.Ports {
				svcPortNames[fmt.Sprintf("%d", sp.TargetPort.IntValue())] = sp.Name
			}

			// Track which EndpointSlice names we create/update so we can clean up stale ones.
			activeSliceNames := make(map[string]bool)

			for _, snap := range pods {
				pod := snap.pod
				podInfo := snap.podInfo

				// Only process RUNNING pods with ports exposed and a known public IP.
				if podInfo.Status != string(PodRunning) || !podInfo.PortsExposed || podInfo.PublicIP == "" {
					continue
				}

				// Check that all selector labels match the pod's labels.
				matched := true
				for k, v := range selector {
					if pod.Labels[k] != v {
						matched = false
						break
					}
				}
				if !matched {
					continue
				}

				isReady := p.computeIsReady(pod, podInfo)

				// Build per-pod endpoint and ports.
				var addrs []string
				var podPorts []discoveryv1.EndpointPort

				for _, requestedPort := range podInfo.RequestedPorts {
					parts := strings.SplitN(requestedPort, "/", 2)
					if len(parts) != 2 {
						continue
					}
					internalPortStr := parts[0]
					protocol := strings.ToLower(parts[1])

					if protocol == "http" || protocol == "https" {
						// HTTP порт: резолвим RunPod proxy домен.
						podID := pod.Annotations[PodIDAnnotation]
						if podID == "" {
							continue
						}
						proxyHost := fmt.Sprintf("%s-%s.proxy.runpod.net", podID, internalPortStr)
						resolved, resolveErr := net.LookupHost(proxyHost)
						if resolveErr != nil || len(resolved) == 0 {
							p.logger.Debug("syncEndpointSlices: failed to resolve proxy host",
								"host", proxyHost, "error", resolveErr)
							continue
						}
						addrs = append(addrs, resolved[0])
						proto := v1.ProtocolTCP
						portNum := int32(443)
						// Имя порта должно совпадать с именем в Service, иначе kube-proxy проигнорирует endpoint.
						// Если Service порт безымянный — оставляем пустым (K8s допускает для единственного порта).
						portName := svcPortNames[internalPortStr]
						podPorts = append(podPorts, discoveryv1.EndpointPort{
							Name: &portName, Protocol: &proto, Port: &portNum,
						})
					} else {
						// TCP порт: publicIP + external port из маппинга.
						externalPort, ok := podInfo.ExternalPortMappings[internalPortStr]
						if !ok {
							continue
						}
						addrs = append(addrs, podInfo.PublicIP)
						proto := v1.ProtocolTCP
						portNum := int32(externalPort)
						// Имя порта должно совпадать с именем в Service, иначе kube-proxy проигнорирует endpoint.
						// Если Service порт безымянный — оставляем пустым (K8s допускает для единственного порта).
						portName := svcPortNames[internalPortStr]
						podPorts = append(podPorts, discoveryv1.EndpointPort{
							Name: &portName, Protocol: &proto, Port: &portNum,
						})
					}
				}

				if len(addrs) == 0 || len(podPorts) == 0 {
					continue
				}

				// Дедупликация адресов.
				uniqueAddrs := make([]string, 0, len(addrs))
				seen := make(map[string]bool)
				for _, a := range addrs {
					if !seen[a] {
						seen[a] = true
						uniqueAddrs = append(uniqueAddrs, a)
					}
				}

				readyVal := isReady
				// EndpointSlice is scoped to a Service by the kubernetes.io/service-name
				// label, not by metadata.name. Keep the object name opaque and short:
				// it is only a Kubernetes object identity and is not used in Service DNS.
				//
				// We still create one slice per (Service, Pod) pair. The same RunPod pod
				// can be selected by multiple Services, and each Service may have its own
				// port names. EndpointSlice ports are slice-wide, while RunPod external
				// port mappings are pod-specific, so packing several RunPod pods into one
				// slice would be easy to get wrong.
				sliceName := endpointSliceName(ns, svc.Name, string(svc.UID), pod.Name, string(pod.UID))
				activeSliceNames[sliceName] = true

				desired := &discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sliceName,
						Namespace: ns,
						Labels: map[string]string{
							"kubernetes.io/service-name":             svc.Name,
							"endpointslice.kubernetes.io/managed-by": managedBy,
						},
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "v1",
								Kind:       "Service",
								Name:       svc.Name,
								UID:        svc.UID,
								Controller: &isController,
							},
						},
					},
					AddressType: addrType,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: uniqueAddrs,
							Conditions: discoveryv1.EndpointConditions{
								Ready: &readyVal,
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: pod.Namespace,
								Name:      pod.Name,
								UID:       pod.UID,
							},
						},
					},
					Ports: podPorts,
				}

				// Create or update.
				existing, getErr := p.clientset.DiscoveryV1().EndpointSlices(ns).Get(
					context.Background(), sliceName, metav1.GetOptions{},
				)
				if k8serrors.IsNotFound(getErr) {
					if _, createErr := p.clientset.DiscoveryV1().EndpointSlices(ns).Create(
						context.Background(), desired, metav1.CreateOptions{},
					); createErr != nil {
						p.logger.Error("syncEndpointSlices: failed to create",
							"slice", sliceName, "error", createErr)
					} else {
						p.logger.Info("syncEndpointSlices: created EndpointSlice",
							"slice", sliceName, "addresses", uniqueAddrs, "ports", podPorts)
					}
				} else if getErr == nil {
					existing.Endpoints = desired.Endpoints
					existing.Ports = desired.Ports
					existing.Labels = desired.Labels
					existing.OwnerReferences = desired.OwnerReferences
					if _, updateErr := p.clientset.DiscoveryV1().EndpointSlices(ns).Update(
						context.Background(), existing, metav1.UpdateOptions{},
					); updateErr != nil {
						p.logger.Error("syncEndpointSlices: failed to update",
							"slice", sliceName, "error", updateErr)
					}
				}
			}

			// Cleanup: удаляем EndpointSlice'ы для подов, которых больше нет.
			existingSlices, listErr := p.clientset.DiscoveryV1().EndpointSlices(ns).List(
				context.Background(),
				metav1.ListOptions{
					LabelSelector: fmt.Sprintf("kubernetes.io/service-name=%s,endpointslice.kubernetes.io/managed-by=%s", svc.Name, managedBy),
				},
			)
			if listErr == nil {
				for idx := range existingSlices.Items {
					name := existingSlices.Items[idx].Name
					if !activeSliceNames[name] {
						if delErr := p.clientset.DiscoveryV1().EndpointSlices(ns).Delete(
							context.Background(), name, metav1.DeleteOptions{},
						); delErr == nil {
							p.logger.Info("syncEndpointSlices: deleted stale EndpointSlice",
								"slice", name, "service", svc.Name)
						}
					}
				}
			}
		}
	}
}

const (
	endpointSliceNameMaxLength = 63
	endpointSliceNamePrefix    = "rnpd-"
	endpointSliceHashLength    = 24
)

func endpointSliceName(namespace, serviceName, serviceUID, podName, podUID string) string {
	// metadata.name is not part of Kubernetes Service discovery. kube-proxy and
	// CoreDNS associate EndpointSlices with Services through the
	// "kubernetes.io/service-name" label. The name only has to be stable and
	// unique in the namespace so reconcile can get/update/delete the same object.
	//
	// Hash both Service and Pod identity. A single pod may legitimately be
	// selected by more than one Service, and those Services must get distinct
	// EndpointSlices because the service-name label and port names are different.
	identity := fmt.Sprintf("%s/%s/%s/%s/%s", namespace, serviceName, serviceUID, podName, podUID)
	sum := sha256.Sum256([]byte(identity))
	hash := hex.EncodeToString(sum[:])[:endpointSliceHashLength]

	return endpointSliceNamePrefix + hash
}

package virtualkubelet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/command"
	"github.com/bsvogler/conduit-kubelet/pkg/config"
	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/providers/runpod"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Provider implements the virtual-kubelet provider interface for the proxy kubelet
type Provider struct {
	nodeName           string
	clientset          *kubernetes.Clientset
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int
	logger             *slog.Logger
	config             *config.Config

	// WebSocket client for backend communication
	wsClient *websocket.Client

	// Command handler
	commandHandler *command.Handler

	// Provider manager
	providerManager *providers.Manager

	// Pod tracking
	pods      map[string]*v1.Pod          // Map of podKey -> Pod
	podStatus map[string]*PodInstanceInfo // Map of podKey -> Instance info
	podsMutex sync.RWMutex                // Mutex for thread-safe access

	// Notification callback for pod status changes
	notifyFunc  func(*v1.Pod)
	notifyMutex sync.RWMutex

	// Context and cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// PodInstanceInfo stores information about a deployed pod instance
type PodInstanceInfo struct {
	PodID           string                  // Kubernetes pod UID
	ProviderPodID   string                  // Provider-specific pod ID
	Provider        string                  // Provider name (runpod, vastai, etc.)
	Status          string                  // Current status
	CostPerHour     float64                 // Cost per hour
	MachineID       string                  // Machine/instance ID
	Location        string                  // Datacenter location
	CreationTime    time.Time               // When the instance was created
	LastStatusCheck time.Time               // Last time status was checked
	StatusResult    *websocket.StatusResult // Last status result from provider

	// Set when the platform rejected the pod; the pod is reported as Failed.
	RejectCode    string
	RejectMessage string
}

// NewProvider creates a new proxy virtual kubelet provider
func NewProvider(ctx context.Context, nodeName, operatingSystem, internalIP string, daemonEndpointPort int,
	cfg *config.Config, clientset *kubernetes.Clientset, logger *slog.Logger) (*Provider, error) {

	providerCtx, cancel := context.WithCancel(ctx)

	// Create provider manager
	providerManager := providers.NewManager(logger)

	// Initialize enabled providers
	if err := initializeProviders(cfg, providerManager, logger); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to initialize providers: %w", err)
	}

	// Create command handler
	commandHandler := command.NewHandler(providerManager, logger)

	// Create WebSocket client configuration.
	// The platform identifies a kubelet by its API token, so that is the kubelet id.
	wsConfig := &websocket.ClientConfig{
		URL:               cfg.GetWebSocketURL(),
		APIToken:          cfg.BackendAPIKey,
		KubeletID:         cfg.BackendAPIKey,
		ReconnectInterval: cfg.WebSocket.ReconnectInterval,
		MaxReconnectDelay: cfg.WebSocket.MaxReconnectDelay,
		HeartbeatInterval: cfg.WebSocket.PingInterval,
		PongTimeout:       cfg.WebSocket.PongTimeout,
		WriteTimeout:      cfg.WebSocket.WriteTimeout,
		ReadTimeout:       cfg.WebSocket.ReadTimeout,
	}

	// Create WebSocket client
	wsClient := websocket.NewClient(wsConfig, logger)

	provider := &Provider{
		nodeName:           nodeName,
		clientset:          clientset,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
		logger:             logger,
		config:             cfg,
		wsClient:           wsClient,
		commandHandler:     commandHandler,
		providerManager:    providerManager,
		pods:               make(map[string]*v1.Pod),
		podStatus:          make(map[string]*PodInstanceInfo),
		ctx:                providerCtx,
		cancel:             cancel,
	}

	// Platform rejections are applied to the Kubernetes pod
	commandHandler.SetRejectHandler(provider.handleReject)

	// Register with the backend after every (re)connect
	wsClient.SetOnConnect(provider.registerWithBackend)

	// Start WebSocket client
	if err := wsClient.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start WebSocket client: %w", err)
	}

	// Start command processing goroutine
	go provider.processCommands()

	return provider, nil
}

// CreatePod creates a new pod by notifying the backend
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	podKey := p.getPodKey(pod)

	p.logger.Info("Creating pod", "pod", pod.Name, "namespace", pod.Namespace)

	// Store pod in local cache
	p.podsMutex.Lock()
	p.pods[podKey] = pod.DeepCopy()

	// Initialize pod status
	p.podStatus[podKey] = &PodInstanceInfo{
		PodID:        string(pod.UID),
		Status:       "PENDING",
		CreationTime: time.Now(),
	}
	p.podsMutex.Unlock()

	// Send pod creation event to backend. Objects from the informer carry no
	// TypeMeta, so restore it for the platform's manifest parser.
	podSpec := pod.DeepCopy()
	podSpec.APIVersion = "v1"
	podSpec.Kind = "Pod"
	event := &websocket.Event{
		Type: websocket.EventPodCreated,
		Data: &websocket.PodCreatedEvent{
			PodName:     pod.Name,
			Namespace:   pod.Namespace,
			UID:         string(pod.UID),
			PodSpec:     podSpec,
			Annotations: pod.Annotations,
		},
	}

	if err := p.wsClient.SendEvent(event); err != nil {
		p.logger.Error("Failed to send pod creation event", "error", err, "pod", pod.Name)
		// Don't fail pod creation if we can't notify backend immediately
		// The backend will discover it through reconciliation
	}

	// Update pod status to pending
	p.updatePodStatus(pod, v1.PodPending, "", "Pod creation event sent to backend")

	return nil
}

// UpdatePod updates an existing pod
func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	podKey := p.getPodKey(pod)

	p.logger.Info("Updating pod", "pod", pod.Name, "namespace", pod.Namespace)

	p.podsMutex.Lock()
	p.pods[podKey] = pod.DeepCopy()
	p.podsMutex.Unlock()

	return nil
}

// DeletePod deletes a pod by terminating the provider instance
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	podKey := p.getPodKey(pod)

	p.logger.Info("Deleting pod", "pod", pod.Name, "namespace", pod.Namespace)

	p.podsMutex.RLock()
	instanceInfo, exists := p.podStatus[podKey]
	providerPodID := ""
	if exists {
		providerPodID = instanceInfo.ProviderPodID
	}
	p.podsMutex.RUnlock()

	// Notify the backend; it decides whether to send a terminate command.
	// The event is sent even without a provider pod id so the platform can
	// close its record by pod name.
	event := &websocket.Event{
		Type: websocket.EventPodDeleted,
		Data: &websocket.PodDeletedEvent{
			PodName:       pod.Name,
			Namespace:     pod.Namespace,
			UID:           string(pod.UID),
			ProviderPodID: providerPodID,
			Reason:        "Pod deleted by Kubernetes",
		},
	}

	if err := p.wsClient.SendEvent(event); err != nil {
		p.logger.Error("Failed to send pod deletion event", "error", err, "pod", pod.Name)
	}

	// Remove from local cache
	p.podsMutex.Lock()
	delete(p.pods, podKey)
	delete(p.podStatus, podKey)
	p.podsMutex.Unlock()

	return nil
}

// GetPod retrieves a pod by namespace and name
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	podKey := namespace + "/" + name

	p.podsMutex.RLock()
	pod, exists := p.pods[podKey]
	p.podsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("pod not found")
	}

	return pod.DeepCopy(), nil
}

// GetPodStatus retrieves the status of a pod
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	podKey := namespace + "/" + name

	p.podsMutex.RLock()
	pod, podExists := p.pods[podKey]
	instanceInfo, infoExists := p.podStatus[podKey]
	p.podsMutex.RUnlock()

	if !podExists {
		return nil, fmt.Errorf("pod not found")
	}

	status := &v1.PodStatus{
		Phase: v1.PodPending,
		Conditions: []v1.PodCondition{
			{
				Type:               v1.PodScheduled,
				Status:             v1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	if infoExists && instanceInfo.RejectCode != "" {
		status.Phase = v1.PodFailed
		status.Reason = instanceInfo.RejectCode
		status.Message = instanceInfo.RejectMessage
		return status, nil
	}

	if infoExists && instanceInfo.StatusResult != nil {
		// Convert provider status to Kubernetes pod status
		switch instanceInfo.StatusResult.Phase {
		case providers.PhaseRunning:
			status.Phase = v1.PodRunning
			status.Conditions = append(status.Conditions, v1.PodCondition{
				Type:               v1.PodReady,
				Status:             v1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
			})
		case providers.PhaseSucceeded:
			status.Phase = v1.PodSucceeded
		case providers.PhaseFailed:
			status.Phase = v1.PodFailed
		}

		// Add container status
		containerStatus := v1.ContainerStatus{
			Name:    pod.Spec.Containers[0].Name,
			Ready:   status.Phase == v1.PodRunning,
			Started: &[]bool{status.Phase == v1.PodRunning}[0],
		}

		if instanceInfo.StatusResult.IsTerminated {
			containerStatus.State.Terminated = &v1.ContainerStateTerminated{
				ExitCode: int32(instanceInfo.StatusResult.ExitCode),
				Reason:   instanceInfo.StatusResult.Message,
			}
		} else if instanceInfo.StatusResult.IsRunning {
			containerStatus.State.Running = &v1.ContainerStateRunning{
				StartedAt: metav1.Time{Time: instanceInfo.CreationTime},
			}
		} else {
			containerStatus.State.Waiting = &v1.ContainerStateWaiting{
				Reason:  instanceInfo.Status,
				Message: instanceInfo.StatusResult.Message,
			}
		}

		status.ContainerStatuses = []v1.ContainerStatus{containerStatus}
	}

	return status, nil
}

// GetPods retrieves all pods managed by this provider
func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	p.podsMutex.RLock()
	defer p.podsMutex.RUnlock()

	pods := make([]*v1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod.DeepCopy())
	}

	return pods, nil
}

// Ping tests connectivity to the backend and providers
func (p *Provider) Ping(ctx context.Context) error {
	if !p.wsClient.IsConnected() {
		return fmt.Errorf("WebSocket client not connected to backend")
	}

	// Test provider connectivity
	healthStatus := p.providerManager.HealthCheck(ctx)
	for provider, healthy := range healthStatus {
		if !healthy {
			p.logger.Warn("Provider health check failed", "provider", provider)
		}
	}

	return nil
}

// GetNodeStatus returns the status of the virtual node
func (p *Provider) GetNodeStatus() *v1.NodeStatus {
	return &v1.NodeStatus{
		Capacity: v1.ResourceList{
			"cpu":            resource.MustParse("1000"),
			"memory":         resource.MustParse("1000Gi"),
			"nvidia.com/gpu": resource.MustParse("1000"),
			"pods":           resource.MustParse("1000"),
		},
		Allocatable: v1.ResourceList{
			"cpu":            resource.MustParse("1000"),
			"memory":         resource.MustParse("1000Gi"),
			"nvidia.com/gpu": resource.MustParse("1000"),
			"pods":           resource.MustParse("1000"),
		},
		Phase: v1.NodeRunning,
		Conditions: []v1.NodeCondition{
			{
				Type:               v1.NodeReady,
				Status:             v1.ConditionTrue,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletReady",
				Message:            "proxy kubelet is posting ready status",
			},
			{
				Type:               v1.NodeMemoryPressure,
				Status:             v1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletHasSufficientMemory",
				Message:            "proxy kubelet has sufficient memory available",
			},
			{
				Type:               v1.NodeDiskPressure,
				Status:             v1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletHasNoDiskPressure",
				Message:            "proxy kubelet has no disk pressure",
			},
			{
				Type:               v1.NodePIDPressure,
				Status:             v1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletHasSufficientPID",
				Message:            "proxy kubelet has sufficient PID available",
			},
		},
		NodeInfo: v1.NodeSystemInfo{
			MachineID:               "proxy-kubelet",
			SystemUUID:              "proxy-kubelet",
			BootID:                  "proxy-kubelet",
			KernelVersion:           "5.4.0",
			OSImage:                 "Proxy Kubelet",
			ContainerRuntimeVersion: "proxy://1.0.0",
			KubeletVersion:          "v1.32.0",
			KubeProxyVersion:        "v1.32.0",
			OperatingSystem:         "linux",
			Architecture:            "amd64",
		},
		DaemonEndpoints: v1.NodeDaemonEndpoints{
			KubeletEndpoint: v1.DaemonEndpoint{Port: int32(p.daemonEndpointPort)},
		},
		Addresses: []v1.NodeAddress{
			{
				Type:    v1.NodeInternalIP,
				Address: p.internalIP,
			},
		},
	}
}

// NotifyPods sets the callback function for pod status changes
func (p *Provider) NotifyPods(ctx context.Context, notifyFunc func(*v1.Pod)) {
	p.notifyMutex.Lock()
	p.notifyFunc = notifyFunc
	p.notifyMutex.Unlock()
}

// NotifyNodeStatus sets the callback function for node status changes (required by virtual-kubelet)
func (p *Provider) NotifyNodeStatus(ctx context.Context, notifyFunc func(*v1.Node)) {
	// For now, we don't need to notify about node status changes
	// This is required by the virtual-kubelet interface
}

// ConfigureNode configures the virtual node
func (p *Provider) ConfigureNode(ctx context.Context, node *v1.Node) {
	node.Status.Capacity = p.GetNodeStatus().Capacity
	node.Status.Allocatable = p.GetNodeStatus().Allocatable

	// Add taints to prevent accidental scheduling
	node.Spec.Taints = []v1.Taint{
		{
			Key:    "virtual-kubelet.io/provider",
			Value:  "proxy",
			Effect: v1.TaintEffectNoSchedule,
		},
		{
			Key:    "gpu-conduit.io/proxy",
			Value:  "true",
			Effect: v1.TaintEffectNoSchedule,
		},
	}
}

// Private helper methods

func (p *Provider) processCommands() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case env := <-p.wsClient.Commands():
			response := p.commandHandler.ExecuteCommand(p.ctx, env)

			// Handle deploy command success - update pod bookkeeping
			if response.Success() {
				if result, ok := response.Data.(*websocket.DeployResult); ok {
					var cmd websocket.Command
					var params websocket.DeployParams
					if err := env.DecodePayload(&cmd); err == nil && cmd.Type == websocket.CommandDeploy {
						if err := cmd.DecodeData(&params); err == nil {
							p.handleDeploymentSuccess(&params, result)
						}
					}
				}
			}

			// Send response back to backend
			if err := p.wsClient.SendResponse(response); err != nil {
				p.logger.Error("Failed to send command response", "error", err, "command_id", env.ID)
			}
		}
	}
}

func (p *Provider) handleDeploymentSuccess(params *websocket.DeployParams, result *websocket.DeployResult) {
	podKey := websocket.PodKey(params.Namespace, params.PodName)

	p.podsMutex.Lock()
	if instanceInfo, exists := p.podStatus[podKey]; exists {
		instanceInfo.ProviderPodID = result.ProviderPodID
		instanceInfo.Provider = result.Provider
		if instanceInfo.Provider == "" {
			instanceInfo.Provider = params.Provider
		}
		instanceInfo.Status = result.Status
		instanceInfo.CostPerHour = result.CostPerHour
		instanceInfo.MachineID = result.MachineID
		instanceInfo.Location = result.DatacenterID
		instanceInfo.LastStatusCheck = time.Now()
	} else {
		p.logger.Warn("Deploy result for unknown pod", "pod", podKey, "provider_pod_id", result.ProviderPodID)
	}
	pod, podExists := p.pods[podKey]
	p.podsMutex.Unlock()

	// Update Kubernetes pod status
	if podExists {
		p.updatePodStatus(pod, v1.PodPending, "", "Instance deployed, waiting for startup")
	}
}

// handleReject applies a platform rejection: the pod is marked Failed with
// reason=code and the platform's message.
func (p *Provider) handleReject(_ context.Context, params *websocket.RejectParams) error {
	podKey := websocket.PodKey(params.Namespace, params.PodName)

	p.podsMutex.Lock()
	pod, podExists := p.pods[podKey]
	if info, exists := p.podStatus[podKey]; exists {
		info.RejectCode = params.Code
		info.RejectMessage = params.Message
		info.Status = "REJECTED"
	}
	p.podsMutex.Unlock()

	if !podExists {
		return fmt.Errorf("pod %s not found", podKey)
	}

	p.updatePodStatus(pod, v1.PodFailed, params.Code, params.Message)
	return nil
}

// registerWithBackend sends the kubelet registration and the ready event.
// It runs after every successful connect.
func (p *Provider) registerWithBackend() {
	reg := &websocket.Registration{
		ID:           p.wsClient.KubeletID(),
		Type:         websocket.KubeletType,
		ClusterName:  p.config.ClusterName,
		NodeName:     p.nodeName,
		Namespace:    p.config.Namespace,
		Capabilities: p.commandHandler.GetAvailableProviders(),
		Metadata: websocket.RegistrationMetadata{
			Version:    websocket.ProtocolVersion,
			InternalIP: p.internalIP,
			KeyMode:    p.config.KeyMode(),
		},
	}
	if reg.Capabilities == nil {
		reg.Capabilities = []string{}
	}

	if err := p.wsClient.SendRegistration(reg); err != nil {
		p.logger.Error("Failed to register with backend", "error", err)
		return
	}

	p.logger.Info("Registered with backend",
		"cluster_name", reg.ClusterName,
		"node_name", reg.NodeName,
		"capabilities", reg.Capabilities,
		"key_mode", reg.Metadata.KeyMode)

	ready := &websocket.Event{
		Type: websocket.EventKubeletReady,
		Data: &websocket.KubeletReadyEvent{NodeName: p.nodeName},
	}
	if err := p.wsClient.SendEvent(ready); err != nil {
		p.logger.Error("Failed to send kubelet_ready event", "error", err)
	}
}

func (p *Provider) updatePodStatus(pod *v1.Pod, phase v1.PodPhase, reason, message string) {
	p.notifyMutex.RLock()
	notifyFunc := p.notifyFunc
	p.notifyMutex.RUnlock()

	if notifyFunc != nil {
		// Update pod status
		pod.Status.Phase = phase
		pod.Status.Reason = reason
		pod.Status.Message = message

		// Notify the kubelet controller
		notifyFunc(pod)
	}
}

func (p *Provider) getPodKey(pod *v1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

func initializeProviders(cfg *config.Config, manager *providers.Manager, logger *slog.Logger) error {
	for _, providerName := range cfg.Providers.EnabledProviders {
		switch providerName {
		case "runpod":
			if cfg.Providers.RunPod.APIKey != "" || cfg.IsProviderEnabled("runpod") {
				client := runpod.NewClient(logger)
				if err := manager.RegisterProvider(client); err != nil {
					return fmt.Errorf("failed to register RunPod provider: %w", err)
				}
			}
		// TODO: Add other providers (vastai, salad, aws, gcp)
		default:
			logger.Warn("Unknown provider specified", "provider", providerName)
		}
	}

	return nil
}

// Cleanup gracefully shuts down the provider
func (p *Provider) Cleanup() error {
	p.logger.Info("Shutting down proxy kubelet provider")

	// Cancel context
	if p.cancel != nil {
		p.cancel()
	}

	// Stop WebSocket client
	if p.wsClient != nil {
		return p.wsClient.Stop()
	}

	return nil
}

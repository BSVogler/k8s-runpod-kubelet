package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/config"
	virtualkubelet "github.com/bsvogler/conduit-kubelet/pkg/virtual_kubelet"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

var (
	kubeconfig        string
	configPath        string
	nodeName          string
	operatingSystem   string
	internalIP        string
	listenPort        int
	logLevel          string
	backendURL        string
	backendAPIKey     string
	healthServerAddr  string
	namespace         string
	reconcileInterval int
	enabledProviders  string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.StringVar(&configPath, "config", "", "Path to configuration file")
	flag.StringVar(&nodeName, "nodename", "virtual-proxy", "Kubernetes node name")
	flag.StringVar(&operatingSystem, "operating-system", "Linux", "Operating system (Linux, Windows)")
	flag.StringVar(&internalIP, "internal-ip", "127.0.0.1", "Internal IP address")
	flag.IntVar(&listenPort, "listen-port", 10250, "Port to listen on")
	flag.StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&backendURL, "backend-url", "", "Backend WebSocket URL")
	flag.StringVar(&backendAPIKey, "backend-api-key", "", "Backend API key")
	flag.StringVar(&healthServerAddr, "health-server-address", ":8080", "Address for health check server")
	flag.StringVar(&namespace, "namespace", "kube-system", "Kubernetes namespace")
	flag.IntVar(&reconcileInterval, "reconcile-interval", 30, "Reconcile interval in seconds")
	flag.StringVar(&enabledProviders, "enabled-providers", "runpod", "Comma-separated list of enabled providers")
}

func main() {
	flag.Parse()

	// Initialize logger
	logger := initializeLogger(logLevel)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalCh
		logger.Info("Received shutdown signal")
		cancel()
	}()

	// Load configuration
	cfg := loadConfiguration(logger)

	// Override configuration with command line flags and environment variables
	overrideConfiguration(cfg)

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		logger.Error("Configuration validation failed", "error", err)
		os.Exit(1)
	}

	logger.Info("Starting proxy kubelet",
		"node_name", cfg.NodeName,
		"backend_url", cfg.BackendURL,
		"enabled_providers", cfg.Providers.EnabledProviders)

	// Create Kubernetes client
	k8sClient, err := createKubernetesClient(kubeconfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Create the proxy kubelet provider
	provider, err := virtualkubelet.NewProvider(
		ctx,
		cfg.NodeName,
		cfg.OperatingSystem,
		cfg.InternalIP,
		cfg.ListenPort,
		cfg,
		k8sClient,
		logger,
	)
	if err != nil {
		logger.Error("Failed to create provider", "error", err)
		os.Exit(1)
	}

	// Ensure cleanup on exit
	defer func() {
		if err := provider.Cleanup(); err != nil {
			logger.Error("Error during provider cleanup", "error", err)
		}
	}()

	// Set up informers
	podInformerFactory, standardInformerFactory := setupInformers(k8sClient, cfg.NodeName, int(cfg.ReconcileInterval.Seconds()))

	// Create controllers
	podController, nodeController, err := createControllers(
		ctx, provider, k8sClient, podInformerFactory, standardInformerFactory,
		cfg.NodeName, cfg.Namespace, logger,
	)
	if err != nil {
		logger.Error("Failed to create controllers", "error", err)
		os.Exit(1)
	}

	// Start informer factories
	go podInformerFactory.Start(ctx.Done())
	go standardInformerFactory.Start(ctx.Done())

	// Create and start health server
	healthServer := virtualkubelet.NewHealthServer(
		cfg.HealthServerAddress,
		func() bool {
			return provider.Ping(context.Background()) == nil
		},
	)
	healthServer.SetLogger(logger)
	healthServer.Start()

	// Ensure health server cleanup
	defer func() {
		if err := healthServer.Stop(); err != nil {
			logger.Error("Error stopping health server", "error", err)
		}
	}()

	// Create and start API server for kubelet endpoints
	apiServer := createAPIServer(provider, cfg.InternalIP, cfg.ListenPort, logger)
	go func() {
		logger.Info("Starting kubelet API server", "address", apiServer.Addr)
		if err := apiServer.ListenAndServe(); err != nil {
			logger.Error("API server failed", "error", err)
			cancel()
		}
	}()

	// Start controllers
	if err := startControllers(ctx, podController, nodeController, logger); err != nil {
		logger.Error("Failed to start controllers", "error", err)
		os.Exit(1)
	}

	logger.Info("Proxy kubelet started successfully")

	// Wait for shutdown
	<-ctx.Done()
	logger.Info("Shutting down proxy kubelet")
}

func initializeLogger(level string) *slog.Logger {
	var logLevel slog.Level

	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "info":
		logLevel = slog.LevelInfo
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})

	return slog.New(handler)
}

func loadConfiguration(logger *slog.Logger) *config.Config {
	cfg := config.DefaultConfig()

	if configPath != "" {
		logger.Info("Loading configuration from file", "path", configPath)
		// TODO: Implement YAML configuration file loading
		// For now, we'll use the default configuration
	}

	// Load from environment variables
	cfg.LoadFromEnvironment()

	return cfg
}

func overrideConfiguration(cfg *config.Config) {
	if nodeName != "" {
		cfg.NodeName = nodeName
	}
	if operatingSystem != "" {
		cfg.OperatingSystem = operatingSystem
	}
	if internalIP != "" {
		cfg.InternalIP = internalIP
	}
	if listenPort != 0 {
		cfg.ListenPort = listenPort
	}
	if backendURL != "" {
		cfg.BackendURL = backendURL
	}
	if backendAPIKey != "" {
		cfg.BackendAPIKey = backendAPIKey
	}
	if healthServerAddr != "" {
		cfg.HealthServerAddress = healthServerAddr
	}
	if namespace != "" {
		cfg.Namespace = namespace
	}
	if reconcileInterval != 0 {
		cfg.ReconcileInterval = time.Duration(reconcileInterval) * time.Second
	}

	// Override enabled providers if specified
	if enabledProviders != "" {
		// Parse comma-separated list
		// TODO: Implement proper parsing
		cfg.Providers.EnabledProviders = []string{"runpod"} // Default for now
	}
}

func createKubernetesClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		// Try in-cluster configuration first
		config, err = rest.InClusterConfig()
		if err != nil {
			// Fall back to default kubeconfig location
			home := homeDir()
			if home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
	}

	if config == nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return clientset, nil
}

func setupInformers(k8sClient *kubernetes.Clientset, nodeName string, reconcileInterval int) (informers.SharedInformerFactory, informers.SharedInformerFactory) {
	// Create pod-specific informer factory with field selector
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient,
		time.Duration(reconcileInterval)*time.Second,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}),
	)

	// Create standard informer factory for other resources
	standardInformerFactory := informers.NewSharedInformerFactory(
		k8sClient,
		time.Duration(reconcileInterval)*time.Second,
	)

	return podInformerFactory, standardInformerFactory
}

func createControllers(ctx context.Context, provider *virtualkubelet.Provider, k8sClient *kubernetes.Clientset,
	podInformer, standardInformer informers.SharedInformerFactory, nodeName, namespace string,
	logger *slog.Logger) (*node.PodController, *node.NodeController, error) {

	// Create event recorder
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: k8sClient.CoreV1().Events(""),
	})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: nodeName})

	// Create pod controller
	podControllerConfig := node.PodControllerConfig{
		PodClient:         k8sClient.CoreV1(),
		PodInformer:       podInformer.Core().V1().Pods(),
		ConfigMapInformer: standardInformer.Core().V1().ConfigMaps(),
		SecretInformer:    standardInformer.Core().V1().Secrets(),
		ServiceInformer:   standardInformer.Core().V1().Services(),
		Provider:          provider,
		EventRecorder:     eventRecorder,
	}

	podController, err := node.NewPodController(podControllerConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create pod controller: %w", err)
	}

	// Create node controller
	nodeController, err := node.NewNodeController(
		provider,
		&v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: *provider.GetNodeStatus(),
		},
		k8sClient.CoreV1().Nodes(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create node controller: %w", err)
	}

	return podController, nodeController, nil
}

func createAPIServer(provider *virtualkubelet.Provider, internalIP string, listenPort int, logger *slog.Logger) *http.Server {
	podHandlerConfig := api.PodHandlerConfig{
		GetPods: func(ctx context.Context) ([]*v1.Pod, error) {
			return provider.GetPods(ctx)
		},
		GetPodsFromKubernetes: func(ctx context.Context) ([]*v1.Pod, error) {
			return provider.GetPods(ctx)
		},
	}

	mux := http.NewServeMux()
	api.AttachPodRoutes(podHandlerConfig, mux, false)

	return &http.Server{
		Addr:    fmt.Sprintf("%s:%d", internalIP, listenPort),
		Handler: mux,
	}
}

func startControllers(ctx context.Context, podController *node.PodController, nodeController *node.NodeController, logger *slog.Logger) error {
	// Start node controller
	go func() {
		logger.Info("Starting node controller")
		if err := nodeController.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("Node controller failed", "error", err)
		}
	}()

	// Start pod controller
	go func() {
		logger.Info("Starting pod controller")
		if err := podController.Run(ctx, 1); err != nil && ctx.Err() == nil {
			logger.Error("Pod controller failed", "error", err)
		}
	}()

	// Wait for pod controller to be ready
	select {
	case <-podController.Ready():
		logger.Info("Controllers started successfully")
		return nil
	case <-podController.Done():
		return fmt.Errorf("pod controller exited before becoming ready: %w", podController.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // Windows
}

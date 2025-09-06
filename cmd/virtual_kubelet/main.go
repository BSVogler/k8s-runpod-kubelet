package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	runpod "github.com/bsvogler/k8s-runpod-kubelet/pkg/virtual_kubelet"
	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bsvogler/k8s-runpod-kubelet/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)
import (
	authenticationv1 "k8s.io/api/authentication/v1"
)

var (
	nodeName            string
	operatingSystem     string
	kubeconfig          string
	providerConfigPath  string
	internalIP          string
	listenPort          int
	logLevel            string
	reconcileInterval   int
	maxGPUPrice         float64
	healthServerAddress string
	kubenamespace       string
	datacenterIDs       string
	heartbeatInterval   int
)

// Log handlers are defined in a separate file

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.IntVar(&reconcileInterval, "reconcile-interval", 30, "Reconcile interval in seconds")
	flag.Float64Var(&maxGPUPrice, "max-gpu-price", 0.5, "Maximum price per hour for GPU instances")
	flag.StringVar(&healthServerAddress, "health-server-address", ":8080", "Address for the health check server to listen on")
	flag.StringVar(&nodeName, "nodename", "virtual-runpod", "kubernetes node name")
	flag.StringVar(&operatingSystem, "os", "Linux", "Operating system (Linux/Windows)")
	flag.StringVar(&providerConfigPath, "provider-config", "", "path to the provider config file")
	flag.StringVar(&internalIP, "internal-ip", "127.0.0.1", "internal IP address")
	flag.IntVar(&listenPort, "listen-port", 10250, "port to listen on")
	flag.StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	flag.StringVar(&kubenamespace, "namespace", "kube-system", "kubernetes namespace")
	flag.StringVar(&datacenterIDs, "datacenter-ids", "", "Comma-separated list of RunPod datacenter IDs to launch pods in")
	flag.IntVar(&heartbeatInterval, "heartbeat-interval", 300, "Heartbeat interval in seconds (0 to disable)")
}

// LoadConfig loads configuration from a YAML file
func LoadConfig(path string) (config.Config, error) {
	var cfg config.Config

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("failed to read config file: %w", err)
	}

	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}

func logAuthInfo(k8sClient *kubernetes.Clientset, logger *slog.Logger) {
	// Try to get user info from API server
	userInfo, err := k8sClient.AuthenticationV1().SelfSubjectReviews().Create(
		context.Background(),
		&authenticationv1.SelfSubjectReview{},
		metav1.CreateOptions{},
	)

	if err != nil {
		logger.Error("Failed to get user info", "error", err)
	} else if userInfo.Status.UserInfo.Username != "" {
		logger.Info("Authenticated as",
			"username", userInfo.Status.UserInfo.Username,
			"groups", userInfo.Status.UserInfo.Groups)
		return
	}
}

// initializeLogger sets up the logger with optional Sentry integration
func initializeLogger() (*slog.Logger, func()) {
	sentryUrl := os.Getenv("SENTRY_URL")
	if sentryUrl == "" {
		return slog.New(slog.NewTextHandler(os.Stdout, nil)), func() {}
	}

	environment := os.Getenv("environment")
	if environment == "" {
		environment = "development"
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:           sentryUrl,
		EnableTracing: false,
		Environment:   environment,
	})
	if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}

	// Create both a Sentry handler and a text handler for stdout
	sentryHandler := sentryslog.Option{Level: slog.LevelInfo}.NewSentryHandler()
	stdoutHandler := slog.NewTextHandler(os.Stdout, nil)

	// Create a custom multi handler that sends logs to both
	multiHandler := newMultiHandler(sentryHandler, stdoutHandler)

	logger := slog.New(multiHandler)
	cleanup := func() {
		sentry.Flush(2 * time.Second)
	}

	return logger, cleanup
}

// setupInformers creates and configures the Kubernetes informers
func setupInformers(k8sClient *kubernetes.Clientset, nodeName string, reconcileInterval int) (informers.SharedInformerFactory, informers.SharedInformerFactory) {
	// Create a separate informer factory just for pods with field selector
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient,
		time.Duration(reconcileInterval)*time.Second,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}),
	)

	// Create a standard informer factory for other resources
	standardInformerFactory := informers.NewSharedInformerFactory(
		k8sClient,
		time.Duration(reconcileInterval)*time.Second,
	)

	return podInformerFactory, standardInformerFactory
}

// createControllers sets up the pod and node controllers
func createControllers(ctx context.Context, provider *runpod.Provider, k8sClient *kubernetes.Clientset,
	podInformer informers.SharedInformerFactory, standardInformer informers.SharedInformerFactory,
	nodeName string, kubenamespace string, logger *slog.Logger) (*node.PodController, *node.NodeController, error) {

	// Create event recorder
	eventBroadcaster := record.NewBroadcaster()
	// here we use a namespace agnostic event sink because the controller can manage pods in all namespaces
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: k8sClient.CoreV1().Events(""), // Empty string means "all namespaces"
	})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: nodeName})

	// The pod controller manages all informers to react to events
	podControllerCfg := node.PodControllerConfig{
		PodClient:                         k8sClient.CoreV1(),
		PodInformer:                       podInformer.Core().V1().Pods(),
		ConfigMapInformer:                 standardInformer.Core().V1().ConfigMaps(),
		SecretInformer:                    standardInformer.Core().V1().Secrets(),
		ServiceInformer:                   standardInformer.Core().V1().Services(),
		Provider:                          provider,
		EventRecorder:                     eventRecorder,
		SyncPodsFromKubernetesRateLimiter: nil,
	}
	podController, err := node.NewPodController(podControllerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create pod controller: %w", err)
	}

	// Create node controller
	_, err = k8sClient.Discovery().ServerResourcesForGroupVersion("coordination.k8s.io/v1")
	var nodeControllerOpts []node.NodeControllerOpt
	if err == nil {
		// Only enable leases if the API is available
		nodeControllerOpts = append(nodeControllerOpts, node.WithNodeEnableLeaseV1(k8sClient.CoordinationV1().Leases(kubenamespace), int32(30)))
	}

	nodeController, err := node.NewNodeController(
		provider,
		provider.GetNodeStatus(),
		k8sClient.CoreV1().Nodes(),
		nodeControllerOpts...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create node controller: %w", err)
	}

	return podController, nodeController, nil
}

// createAPIServer sets up the HTTP API server
func createAPIServer(provider *runpod.Provider, internalIP string, listenPort int) *http.Server {
	// Set up basic handlers for the HTTP server. This lets k8s interact with the kubelet
	podHandlerConfig := api.PodHandlerConfig{
		RunInContainer: func(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
			return fmt.Errorf("running commands in container is not supported by RunPod")
		},
		GetContainerLogs: func(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
			return nil, fmt.Errorf("container logs not supported by RunPod")
		},
		GetPods: func(ctx context.Context) ([]*v1.Pod, error) {
			return provider.GetPods(ctx)
		},
		GetPodsFromKubernetes: func(ctx context.Context) ([]*v1.Pod, error) {
			return provider.GetPods(ctx)
		},
		// These are optional - implement if needed
		//GetStatsSummary: nil,
		//GetMetricsResource: nil,
		//StreamIdleTimeout: 30 * time.Second,
		//StreamCreationTimeout: 15 * time.Second,
	}

	// Create HTTP server
	mux := http.NewServeMux()
	// Attach pod routes to the mux (multiplexer)
	api.AttachPodRoutes(podHandlerConfig, mux, false) // Set debug to false for production

	return &http.Server{
		Addr:    fmt.Sprintf("%s:%d", internalIP, listenPort),
		Handler: mux,
	}
}

// startControllers starts the pod and node controllers
func startControllers(ctx context.Context, podController *node.PodController, nodeController *node.NodeController, logger *slog.Logger) error {
	// Start node controller
	go func() {
		logger.Info("Starting node controller")
		if err := nodeController.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("Failed to run node controller", "error", err)
		}
	}()

	// Start pod controller
	go func() {
		logger.Info("Starting pod controller")
		if err := podController.Run(ctx, 1); err != nil && ctx.Err() == nil {
			logger.Error("Failed to run pod controller", "error", err)
		}
	}()

	// Wait for pod controller to be ready
	select {
	case <-podController.Ready():
		logger.Info("Pod controller is ready")
		return nil
	case <-podController.Done():
		return fmt.Errorf("pod controller exited before becoming ready: %w", podController.Err())
	}
}

// loadConfiguration loads provider config from file if specified
func loadConfiguration(logger *slog.Logger) config.Config {
	var providerConfig config.Config
	if providerConfigPath != "" {
		var err error
		providerConfig, err = LoadConfig(providerConfigPath)
		if err != nil {
			logger.Error("Failed to load provider config", "error", err)
			os.Exit(1)
		}
	}
	return providerConfig
}

// createAndValidateK8sClient creates and validates the Kubernetes client
func createAndValidateK8sClient(logger *slog.Logger) *kubernetes.Clientset {
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}
	if k8sClient != nil {
		logAuthInfo(k8sClient, logger)
	}
	return k8sClient
}

// validateEnvironment checks required environment variables
func validateEnvironment(logger *slog.Logger) {
	if os.Getenv("RUNPOD_API_KEY") == "" {
		logger.Error("RUNPOD_API_KEY environment variable is not set")
		os.Exit(1)
	}
}

// setupShutdownHandlers sets up graceful shutdown handlers
func setupShutdownHandlers(ctx context.Context, apiServer *http.Server, healthServer *runpod.HealthServer, provider *runpod.Provider, logger *slog.Logger) {
	go func() {
		<-ctx.Done()
		provider.StopHeartbeat()
		if err := healthServer.Stop(); err != nil {
			logger.Error("Health server shutdown error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}()
}

func main() {
	logger, cleanup := initializeLogger()
	defer cleanup()

	flag.Parse()

	// Create a context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle termination signals
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalCh
		logger.Info("Received termination signal")
		cancel()
	}()

	// Load configuration and validate environment
	providerConfig := loadConfiguration(logger)
	k8sClient := createAndValidateK8sClient(logger)
	validateEnvironment(logger)

	// Create the RunPod provider
	// Update config with command line flags
	providerConfig.DatacenterIDs = datacenterIDs
	provider, err := runpod.NewProvider(
		ctx,
		nodeName,
		operatingSystem,
		internalIP,
		listenPort,
		providerConfig,
		k8sClient,
		logger,
		heartbeatInterval,
	)
	if err != nil {
		logger.Error("Failed to create RunPod provider", "error", err)
		os.Exit(1)
	}

	// Setup informers
	podInformerFactory, standardInformerFactory := setupInformers(k8sClient, nodeName, reconcileInterval)

	// Create controllers
	podController, nodeController, err := createControllers(ctx, provider, k8sClient,
		podInformerFactory, standardInformerFactory, nodeName, kubenamespace, logger)
	if err != nil {
		logger.Error("Failed to create controllers", "error", err)
		os.Exit(1)
	}

	// Start informer factories
	go podInformerFactory.Start(ctx.Done())
	go standardInformerFactory.Start(ctx.Done())

	// Create and start API server
	apiServer := createAPIServer(provider, internalIP, listenPort)

	// Create and start health server
	healthServer := runpod.NewHealthServer(
		healthServerAddress, // Using the flag value you already defined
		func() bool {
			// This is the readiness check function
			// Return true if the provider is ready to accept pods
			return provider.Ping(context.Background()) == nil
		},
	)
	logger.Info("Starting health check server", "address", healthServerAddress)
	healthServer.Start()

	// Ensure proper shutdown
	setupShutdownHandlers(ctx, apiServer, healthServer, provider, logger)

	// Start controllers
	if err := startControllers(ctx, podController, nodeController, logger); err != nil {
		logger.Error("Failed to start controllers", "error", err)
		os.Exit(1)
	}

	// Start API server
	go func() {
		logger.Info("Starting API server for K8S to use", "port", listenPort)
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Failed to run API server", "error", err)
			cancel()
		}
	}()

	// Load running pods from RunPod API
	logger.Info("Loading running pods from RunPod API")
	provider.LoadRunning()

	// Wait for context cancellation
	<-ctx.Done()
	logger.Info("Shutting down virtual-kubelet")
}

func syncInformers(ctx context.Context, logger *slog.Logger, informerFactory informers.SharedInformerFactory) {
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	defer syncCancel()

	logger.Info("Waiting for informer caches to sync")
	syncResult := informerFactory.WaitForCacheSync(syncCtx.Done())

	//Check results for each informer
	for informerType, synced := range syncResult {
		if !synced {
			logger.Error("Cache failed to sync", "informerType", fmt.Sprintf("%v", informerType))
		} else {
			logger.Info("Cache synced successfully", "informerType", fmt.Sprintf("%v", informerType))
		}
	}

	//a simple check to see if all caches synced
	allSynced := true
	for _, synced := range syncResult {
		if !synced {
			allSynced = false
			break
		}
	}
	if !allSynced {
		logger.Error("Not all caches synced")
		os.Exit(1)
	}
}

// createK8sClient creates a Kubernetes client
func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var clusterConfig *rest.Config
	var err error

	if kubeconfig == "" {
		// Try loading in-cluster clusterConfig
		clusterConfig, err = rest.InClusterConfig()
		if err != nil {
			// If no in-cluster clusterConfig, look for kubeconfig in default location
			home := homeDir()
			if home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
	}

	if clusterConfig == nil {
		clusterConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return clientset, nil
}

// homeDir returns the user's home directory
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // Windows
}

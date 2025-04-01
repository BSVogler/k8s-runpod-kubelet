package main

import (
	"context"
	"flag"
	"fmt"
	runpod "github.com/bsvogler/k8s-runpod-kubelet/pkg/virtual_kubelet"
	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
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

func main() {
	sentryUrl := os.Getenv("SENTRY_URL")
	var logger *slog.Logger
	if sentryUrl != "" {
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

		// Configure `slog` to use the multi handler
		logger = slog.New(multiHandler)
		//logger = logger.With("release", "v1.0.1")
		defer sentry.Flush(2 * time.Second) //send errors after a crash
	} else { // Use a default logger (stdout) when Sentry is not initialized
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}

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

	// Load provider config
	var providerConfig config.Config
	if providerConfigPath != "" {
		var err error
		providerConfig, err = LoadConfig(providerConfigPath)
		if err != nil {
			logger.Error("Failed to load provider config", "error", err)
			os.Exit(1)
		}
	}

	// Create Kubernetes client
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Create the RunPod provider
	provider, err := runpod.NewProvider(
		ctx,
		nodeName,
		operatingSystem,
		internalIP,
		listenPort,
		providerConfig,
		k8sClient,
		logger,
	)
	if err != nil {
		logger.Error("Failed to create RunPod provider", "error", err)
		os.Exit(1)
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: k8sClient.CoreV1().Events(kubenamespace),
	})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: nodeName})

	// Create informer factory for the default namespace or specified namespace
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		k8sClient,
		30*time.Second, // Resync period
	)

	// Start the informer factory
	informerFactory.Start(ctx.Done())

	// Wait for caches to sync
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()

	_, err = k8sClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		logger.Error("Failed to connect to Kubernetes API", "error", err)
		os.Exit(1)
	}
	logger.Info("Successfully connected to Kubernetes API")

	// Create pod controller with the informer
	podInformer := informerFactory.Core().V1().Pods()
	configMapInformer := informerFactory.Core().V1().ConfigMaps()
	secretInformer := informerFactory.Core().V1().Secrets()
	serviceInformer := informerFactory.Core().V1().Services()

	// Wait for caches to sync with a timeout
	logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(syncCtx.Done(), podInformer.Informer().HasSynced) {
		logger.Error("Failed to sync pod informer cache within timeout")
		// Consider continuing anyway or exiting
		// os.Exit(1)

		// For debugging, log a warning but continue
		logger.Warn("Continuing without fully synced cache")
	} else {
		logger.Info("Informer caches synced successfully")
	}
	// Then when creating the pod controller, include all informers:
	podController, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                         k8sClient.CoreV1(),
		PodInformer:                       podInformer,
		ConfigMapInformer:                 configMapInformer,
		SecretInformer:                    secretInformer,
		ServiceInformer:                   serviceInformer,
		Provider:                          provider,
		EventRecorder:                     eventRecorder,
		SyncPodsFromKubernetesRateLimiter: nil,
	})
	if err != nil {
		logger.Error("Failed to create pod controller", "error", err)
		os.Exit(1)
	}

	// Create node controller
	nodeController, err := node.NewNodeController(
		provider,
		provider.GetNodeStatus(),
		k8sClient.CoreV1().Nodes(),
		node.WithNodeEnableLeaseV1(k8sClient.CoordinationV1().Leases(kubenamespace), int32(30)),
	)
	if err != nil {
		logger.Error("Failed to create node controller", "error", err)
		os.Exit(1)
	}

	// Set up basic handlers
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
	// Attach pod routes to the mux
	api.AttachPodRoutes(podHandlerConfig, mux, false) // Set debug to false for production

	// Create HTTP server
	apiServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", internalIP, listenPort),
		Handler: mux,
	}
	healthServer := runpod.NewHealthServer(
		healthServerAddress, // Using the flag value you already defined
		func() bool {
			// This is the readiness check function
			// Return true if the provider is ready to accept pods
			return provider.Ping(context.Background()) == nil
		},
	)

	// Start the health check server
	logger.Info("Starting health check server", "address", healthServerAddress)
	healthServer.Start()

	// Ensure shutdown on context cancellation
	go func() {
		<-ctx.Done()
		if err := healthServer.Stop(); err != nil {
			logger.Error("Health server shutdown error", "error", err)
		}
	}()

	// Start controllers and API server
	go func() {
		logger.Info("Starting node controller")
		if err := nodeController.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("Failed to run node controller", "error", err)
			cancel()
		}
	}()

	go func() {
		logger.Info("Starting pod controller")
		// The second parameter (threadiness) is the number of workers
		if err := podController.Run(ctx, 1); err != nil && ctx.Err() == nil {
			logger.Error("Failed to run pod controller", "error", err)
			cancel()
		}
	}()

	go func() {
		logger.Info("Starting API server", "port", listenPort)
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Failed to run API server", "error", err)
			cancel()
		}
	}()

	// Ensure server shutdown on context cancellation
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("API server shutdown error", "error", err)
		}
	}()

	// Load running pods from RunPod API
	logger.Info("Loading running pods from RunPod API")
	provider.LoadRunning()

	// Wait for context cancellation
	<-ctx.Done()
	logger.Info("Shutting down virtual-kubelet")
}

// createK8sClient creates a Kubernetes client
func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		// Try loading in-cluster config
		config, err = rest.InClusterConfig()
		if err != nil {
			// If no in-cluster config, look for kubeconfig in default location
			home := homeDir()
			if home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
	}

	if config == nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
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

package main

import (
	"context"
	"flag"
	"fmt"
	runpod "github.com/bsvogler/k8s-runpod-controller/pkg/runpod_controller"
	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.IntVar(&reconcileInterval, "reconcile-interval", 30, "Reconcile interval in seconds")
	flag.Float64Var(&maxGPUPrice, "max-gpu-price", 0.5, "Maximum price per hour for GPU instances")
	flag.StringVar(&healthServerAddress, "health-server-address", ":8080", "Address for the health check server to listen on")
	flag.StringVar(&nodeName, "nodename", os.Getenv("NODENAME"), "kubernetes node name")
	flag.StringVar(&operatingSystem, "os", "Linux", "Operating system (Linux/Windows)")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file")
	flag.StringVar(&providerConfigPath, "provider-config", "", "path to the provider config file")
	flag.StringVar(&internalIP, "internal-ip", "127.0.0.1", "internal IP address")
	flag.IntVar(&listenPort, "listen-port", 10250, "port to listen on")
	flag.StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
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
		providerConfig, err = config.LoadConfig(providerConfigPath)
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

	// Create pod controller
	podController, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                         k8sClient.CoreV1(),
		PodInformer:                       nil, // Pod informer is optional
		Provider:                          provider,
		EventRecorder:                     nil, // You can create an event recorder if needed
		Recorder:                          nil, // You can create a metrics recorder if needed
		ConfigNamespace:                   kubenamespace,
		ConfigMaps:                        nil, // You can specify config maps to watch
		Secrets:                           nil, // You can specify secrets to watch
		SyncPodsFromKubernetesRateLimiter: nil, // Optional rate limiter
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
		node.WithNodeEnableLeaseV1(k8sClient.CoordinationV1().Leases(kubenamespace), 30*time.Second),
	)
	if err != nil {
		logger.Error("Failed to create node controller", "error", err)
		os.Exit(1)
	}

	// Create API server for kubectl logs/exec
	apiServerConfig := api.ServerConfig{
		KubeletNamespace:      kubenamespace,
		KubeConfigPath:        kubeconfig,
		StreamIdleTimeout:     30 * time.Minute,
		StreamCreationTimeout: 10 * time.Second,
		Provider:              provider,
		NodeName:              nodeName,
		InternalIP:            internalIP,
		DaemonPort:            int(listenPort),
		DisableTaintNode:      false,
	}

	apiServer, err := api.NewServerWithConfig(apiServerConfig)
	if err != nil {
		logger.Error("Failed to create API server", "error", err)
		os.Exit(1)
	}

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
		if err := apiServer.Run(ctx); err != nil && ctx.Err() == nil {
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

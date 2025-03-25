package main

import (
	"flag"
	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	"github.com/bsvogler/k8s-runpod-controller/pkg/runpod_controller"
	"github.com/getsentry/sentry-go"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	sentryslog "github.com/getsentry/sentry-go/slog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	sentryUrl := os.Getenv("SENTRY_URL")
	var logger *slog.Logger
	if sentryUrl != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn: sentryUrl,
		})
		if err != nil {
			log.Fatalf("sentry.Init: %s", err)
		}
		// Configure `slog` to use Sentry as a handler
		logger = slog.New(sentryslog.Option{Level: slog.LevelDebug}.NewSentryHandler())
		logger = logger.With("release", "v1.0.1")
		defer sentry.Flush(2 * time.Second) //send errors after a crash
	} else { // Use a default logger (stdout) when Sentry is not initialized
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	// Parse command line flags
	var kubeconfig string
	var reconcileInterval int
	var pendingJobThreshold int
	var maxPendingTime int
	var maxGPUPrice float64
	var healthServerAddress string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.IntVar(&reconcileInterval, "reconcile-interval", 30, "Reconcile interval in seconds")
	flag.IntVar(&pendingJobThreshold, "pending-job-threshold", 5, "Number of pending jobs that triggers automatic offloading")
	flag.IntVar(&maxPendingTime, "max-pending-time", 5, "Number of pending jobs that triggers automatic offloading")
	flag.Float64Var(&maxGPUPrice, "max-gpu-price", 0.5, "Maximum price per hour for GPU instances")
	flag.StringVar(&healthServerAddress, "health-server-address", ":8080", "Address for the health check server to listen on")
	flag.Parse()

	// Create Kubernetes client
	var k8sConfig *rest.Config
	var err error
	if kubeconfig == "" {
		// Use in-cluster config
		k8sConfig, err = rest.InClusterConfig()
		if err != nil {
			logger.Error("Failed to create in-cluster config", "err", err)
			os.Exit(1)
		}
	} else {
		// Use kubeconfig file
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			logger.Error("Failed to create config from kubeconfig file", "kubeconfig", kubeconfig, "err", err)
			os.Exit(1)
		}
	}

	// Create clientset
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "err", err)
		os.Exit(1)
	}

	// Create controller config
	controllerConfig := config.Config{
		ReconcileInterval:   time.Duration(reconcileInterval) * time.Second,
		PendingJobThreshold: pendingJobThreshold,
		MaxPendingTime:      maxPendingTime,
		MaxGPUPrice:         maxGPUPrice,
		HealthServerAddress: healthServerAddress,
	}

	// Create and start controller
	jobController := controller.NewJobController(clientset, logger, controllerConfig)

	// Start controller in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- jobController.Start()
	}()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for either an error or a signal
	select {
	case err := <-errChan:
		logger.Error("Controller error", "err", err)
		os.Exit(1)
	case sig := <-sigChan:
		logger.Info("Received signal, shutting down", "signal", sig)
		os.Exit(0)
	}
}

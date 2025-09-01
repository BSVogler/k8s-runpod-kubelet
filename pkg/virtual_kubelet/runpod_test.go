package runpod_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/bsvogler/k8s-runpod-kubelet/pkg/config"
	runpod "github.com/bsvogler/k8s-runpod-kubelet/pkg/virtual_kubelet"
)

type testContext struct {
	t          *testing.T
	logger     *slog.Logger
	clientset  *kubernetes.Clientset
	client     *runpod.Client
	namespace  *v1.Namespace
	testFailed bool
}

// setupTestEnvironment sets up the test environment and returns a test context
func setupTestEnvironment(t *testing.T) *testContext {
	// Skip if this is a short test run
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Check if RUNPOD_API_KEY is set
	if os.Getenv("RUNPOD_API_KEY") == "" {
		t.Skip("RUNPOD_API_KEY environment variable not set")
	}

	// Setup logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Setup Kubernetes clientset
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG environment variable not set")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err, "Failed to build config from flags")

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err, "Failed to create Kubernetes clientset")

	// Create RunPod client
	testConfig := &config.Config{}
	client := runpod.NewRunPodClient(logger, clientset, testConfig)

	// Create a test namespace
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("runpod-test-%d", time.Now().Unix()),
		},
	}

	// Create namespace in Kubernetes
	ns, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create test namespace")

	return &testContext{
		t:         t,
		logger:    logger,
		clientset: clientset,
		client:    client,
		namespace: ns,
	}
}

// createTestPod creates a test pod configuration
func createTestPod() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("runpod-test-pod-%d", time.Now().Unix()),
			Annotations: map[string]string{
				runpod.CloudTypeAnnotation:  "STANDARD",
				runpod.GpuMemoryAnnotation:  "2",
				runpod.TemplateIdAnnotation: "8pjdlrsfmx",
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Name:    "test-container",
					Image:   "nvidia/cuda:11.7.1-base-ubuntu22.04",
					Command: []string{"nvidia-smi", "-L"},
					Env: []v1.EnvVar{
						{
							Name:  "TEST_ENV_VAR",
							Value: "test_value",
						},
					},
				},
			},
		},
	}
}

// waitForPodStatus waits for the pod to reach the expected status
func waitForPodStatus(tc *testContext, runpodID string) (*runpod.DetailedStatus, bool) {
	maxWaitTime := 5 * time.Minute
	interval := 10 * time.Second
	deadline := time.Now().Add(maxWaitTime)

	tc.t.Logf("Waiting up to %s for pod to start...", maxWaitTime)

	for time.Now().Before(deadline) {
		// check status using REST API
		restStatus, err := tc.client.GetPodStatusREST(runpodID)
		if err == nil {
			tc.t.Logf("Pod status (REST): %s", restStatus)
		} else {
			tc.t.Logf("Error getting pod status via REST: %v", err)
		}

		// Get detailed status
		detailedStatus, err := tc.client.GetDetailedPodStatus(runpodID)
		if err == nil {
			tc.t.Logf("Detailed pod status: currentStatus=%s, desiredStatus=%s",
				detailedStatus.CurrentStatus, detailedStatus.DesiredStatus)

			// If we have runtime info, check for completion
			if detailedStatus.Runtime != nil {
				tc.t.Logf("Runtime info: exitCode=%d, message=%s, completionStatus=%s",
					detailedStatus.Runtime.Container.ExitCode,
					detailedStatus.Runtime.Container.Message,
					detailedStatus.Runtime.PodCompletionStatus)

				// Check if pod has finished successfully
				if runpod.IsSuccessfulCompletion(detailedStatus) {
					tc.t.Log("Pod has completed successfully")
					return detailedStatus, true
				}
			}
		} else {
			tc.t.Logf("Error getting detailed pod status: %v", err)
		}

		time.Sleep(interval)
	}

	return nil, false
}

// verifyPodTermination verifies that a pod has been terminated
func verifyPodTermination(tc *testContext, terminatedID string) bool {
	deadline := time.Now().Add(2 * time.Minute)
	interval := 10 * time.Second

	for time.Now().Before(deadline) {
		status, err := tc.client.GetPodStatusREST(terminatedID)
		if err != nil {
			tc.t.Logf("Error getting pod status during termination: %v", err)
		} else {
			tc.t.Logf("Pod termination status: %s", status)

			if status == runpod.PodTerminated || status == runpod.PodNotFound {
				return true
			}
		}

		time.Sleep(interval)
	}

	return false
}

func TestRunPodIntegration(t *testing.T) {
	tc := setupTestEnvironment(t)

	// Cleanup namespace when test is done
	defer func() {
		err := tc.clientset.CoreV1().Namespaces().Delete(context.Background(), tc.namespace.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
	}()

	pod := createTestPod()

	// Variables to track created RunPod instance
	var runpodID string
	var costPerHr float64
	var params map[string]interface{}
	var createdPod *v1.Pod
	var terminatedID string

	// Ensure resources are cleaned up at the end
	defer func() {
		if createdPod != nil {
			t.Logf("Cleaning up Kubernetes pod: %s", pod.Name)
			err := tc.clientset.CoreV1().Pods(tc.namespace.Name).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			if err != nil {
				t.Logf("Failed to delete pod: %v", err)
			}
		}

		if runpodID != "" {
			t.Logf("Cleaning up RunPod instance: %s", runpodID)
			err := tc.client.TerminatePod(runpodID)
			if err != nil {
				t.Logf("Failed to terminate RunPod instance: %v", err)
			}
		}
	}()

	// Step 1: Create and verify pod creation
	t.Run("1. CreatePod", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		createdPod, err = tc.clientset.CoreV1().Pods(tc.namespace.Name).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to create test pod: %v", err)
		}

		if createdPod == nil || createdPod.Name != pod.Name {
			tc.testFailed = true
			t.Fatalf("Created pod verification failed")
		}

		t.Logf("Pod created successfully: %s", createdPod.Name)
	})

	// Step 2: Prepare RunPod parameters
	t.Run("2. PrepareParameters", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		createdPod, err = tc.clientset.CoreV1().Pods(tc.namespace.Name).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to get created pod: %v", err)
		}

		params, err = tc.client.PrepareRunPodParameters(createdPod, false)
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to prepare RunPod parameters: %v", err)
		}

		if params == nil {
			tc.testFailed = true
			t.Fatalf("RunPod parameters should not be nil")
		}

		t.Log("RunPod parameters prepared successfully")
	})

	// Step 3: Deploy pod to RunPod
	t.Run("3. DeployToRunPod", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		t.Log("Deploying pod to RunPod...")
		runpodID, costPerHr, err = tc.client.DeployPodREST(params)
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to deploy pod to RunPod: %v", err)
		}

		if runpodID == "" {
			tc.testFailed = true
			t.Fatalf("RunPod ID should not be empty")
		}

		if costPerHr <= 0.0 {
			tc.testFailed = true
			t.Fatalf("Cost per hour should be greater than 0, got: %f", costPerHr)
		}

		t.Logf("Pod deployed to RunPod with ID: %s, cost: $%.4f/hr", runpodID, costPerHr)
	})

	// Step 4: Update pod with RunPod ID
	t.Run("4. UpdatePodAnnotations", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s","%s":"%g"}}}`,
			runpod.PodIDAnnotation, runpodID,
			runpod.CostAnnotation, costPerHr)

		_, err := tc.clientset.CoreV1().Pods(tc.namespace.Name).Patch(
			context.Background(),
			pod.Name,
			"application/strategic-merge-patch+json",
			[]byte(patch),
			metav1.PatchOptions{},
		)
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to update pod with RunPod ID: %v", err)
		}

		t.Log("Pod annotations updated successfully")
	})

	// Step 5: Wait for pod to start and check status
	t.Run("5. WaitForPodStartAndCheckStatus", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		detailedStatus, podReady := waitForPodStatus(tc, runpodID)

		if !podReady {
			tc.testFailed = true
			t.Fatalf("Pod did not reach running or exited state within timeout period")
		}

		lastStatus := runpod.PodStatus(detailedStatus.DesiredStatus)
		if !contains([]runpod.PodStatus{runpod.PodRunning, runpod.PodExited}, lastStatus) {
			tc.testFailed = true
			t.Fatalf("Pod should reach running or exited state, got: %s", lastStatus)
		}

		t.Logf("Pod reached expected status: %s", lastStatus)
	})

	// Step 6: Terminate pod
	t.Run("6. TerminatePod", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		t.Log("Terminating pod...")
		err := tc.client.TerminatePod(runpodID)
		if err != nil {
			tc.testFailed = true
			t.Fatalf("Failed to terminate pod: %v", err)
		}

		// Store the ID and clear runpodID so we don't try to terminate it again in the defer block
		terminatedID = runpodID
		runpodID = ""

		t.Log("Pod termination initiated")
	})

	// Step 7: Verify pod termination
	t.Run("7. VerifyPodTermination", func(t *testing.T) {
		if tc.testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		if terminatedID == "" {
			tc.testFailed = true
			t.Fatalf("No terminated pod ID to verify")
		}

		t.Log("Verifying pod termination...")

		terminated := verifyPodTermination(tc, terminatedID)
		if !terminated {
			tc.testFailed = true
			t.Fatalf("Pod was not confirmed terminated within timeout period")
		}

		t.Log("Pod termination verified successfully")
	})

	if tc.testFailed {
		t.Fatalf("Test failed - see subtest failures for details")
	} else {
		t.Log("All tests completed successfully")
	}
}

// Helper function to check if a slice contains a value
func contains(slice []runpod.PodStatus, value runpod.PodStatus) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

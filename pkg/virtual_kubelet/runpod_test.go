package runpod_test

import (
	"context"
	"fmt"
	runpod "github.com/bsvogler/k8s-runpod-kubelet/pkg/virtual_kubelet"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRunPodIntegration(t *testing.T) {
	// Skip if this is a short test run
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Check if RUNPOD_KEY is set
	apiKey := os.Getenv("RUNPOD_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_KEY environment variable not set")
	}

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
	client := runpod.NewRunPodClient(logger, clientset)

	// Create a test namespace
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("runpod-test-%d", time.Now().Unix()),
		},
	}

	// Create namespace in Kubernetes
	ns, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create test namespace")

	// Cleanup namespace when test is done
	defer func() {
		err := clientset.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("Failed to delete namespace: %v", err)
		}
	}()

	// Create a test pod configuration
	pod := &v1.Pod{
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

	// Variables to track created RunPod instance
	var runpodID string
	var costPerHr float64
	var params map[string]interface{}
	var createdPod *v1.Pod
	var lastStatus runpod.PodStatus
	var detailedStatus *runpod.DetailedStatus
	var terminatedID string

	// Ensure resources are cleaned up at the end
	defer func() {
		// Clean up the Kubernetes pod if it was created
		if createdPod != nil {
			t.Logf("Cleaning up Kubernetes pod: %s", pod.Name)
			err := clientset.CoreV1().Pods(ns.Name).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			if err != nil {
				t.Logf("Failed to delete pod: %v", err)
			}
		}

		// If we created a RunPod instance, make sure to terminate it
		if runpodID != "" {
			t.Logf("Cleaning up RunPod instance: %s", runpodID)
			err := client.TerminatePod(runpodID)
			if err != nil {
				t.Logf("Failed to terminate RunPod instance: %v", err)
			}
		}
	}()

	// Use t.Run with a boolean success flag to control progression
	var testFailed bool

	// Step 1: Create and verify pod creation
	t.Run("1. CreatePod", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		createdPod, err = clientset.CoreV1().Pods(ns.Name).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to create test pod: %v", err)
		}

		if createdPod == nil || createdPod.Name != pod.Name {
			testFailed = true
			t.Fatalf("Created pod verification failed")
		}

		t.Logf("Pod created successfully: %s", createdPod.Name)
	})

	// Step 2: Prepare RunPod parameters
	t.Run("2. PrepareParameters", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		createdPod, err = clientset.CoreV1().Pods(ns.Name).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to get created pod: %v", err)
		}

		params, err = client.PrepareRunPodParameters(createdPod, false) // false for REST API
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to prepare RunPod parameters: %v", err)
		}

		if params == nil {
			testFailed = true
			t.Fatalf("RunPod parameters should not be nil")
		}

		t.Log("RunPod parameters prepared successfully")
	})

	// Step 3: Deploy pod to RunPod
	t.Run("3. DeployToRunPod", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		var err error
		t.Log("Deploying pod to RunPod...")
		runpodID, costPerHr, err = client.DeployPodREST(params)
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to deploy pod to RunPod: %v", err)
		}

		if runpodID == "" {
			testFailed = true
			t.Fatalf("RunPod ID should not be empty")
		}

		if costPerHr <= 0.0 {
			testFailed = true
			t.Fatalf("Cost per hour should be greater than 0, got: %f", costPerHr)
		}

		t.Logf("Pod deployed to RunPod with ID: %s, cost: $%.4f/hr", runpodID, costPerHr)
	})

	// Step 4: Update pod with RunPod ID
	t.Run("4. UpdatePodAnnotations", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s","%s":"%g"}}}`,
			runpod.PodIDAnnotation, runpodID,
			runpod.CostAnnotation, costPerHr)

		_, err = clientset.CoreV1().Pods(ns.Name).Patch(
			context.Background(),
			pod.Name,
			"application/strategic-merge-patch+json",
			[]byte(patch),
			metav1.PatchOptions{},
		)
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to update pod with RunPod ID: %v", err)
		}

		t.Log("Pod annotations updated successfully")
	})

	// Step 5: Wait for pod to start and check status
	t.Run("5. WaitForPodStartAndCheckStatus", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		maxWaitTime := 5 * time.Minute
		interval := 10 * time.Second
		deadline := time.Now().Add(maxWaitTime)

		t.Logf("Waiting up to %s for pod to start...", maxWaitTime)
		podReady := false

		for time.Now().Before(deadline) && !podReady {
			// check status using REST API
			restStatus, err := client.GetPodStatusREST(runpodID)
			if err == nil {
				t.Logf("Pod status (REST): %s", restStatus)
			} else {
				t.Logf("Error getting pod status via REST: %v", err)
			}

			// Get detailed status
			detailedStatus, err = client.GetDetailedPodStatus(runpodID)
			if err == nil {
				t.Logf("Detailed pod status: currentStatus=%s, desiredStatus=%s",
					detailedStatus.CurrentStatus, detailedStatus.DesiredStatus)

				// If we have runtime info, check for completion
				if detailedStatus.Runtime != nil {
					t.Logf("Runtime info: exitCode=%d, message=%s, completionStatus=%s",
						detailedStatus.Runtime.Container.ExitCode,
						detailedStatus.Runtime.Container.Message,
						detailedStatus.Runtime.PodCompletionStatus)

					// Check if pod has finished successfully
					if runpod.IsSuccessfulCompletion(detailedStatus) {
						t.Log("Pod has completed successfully")
						podReady = true
						break
					}
				}
			} else {
				t.Logf("Error getting detailed pod status: %v", err)
			}

			time.Sleep(interval)
		}

		// Assert that pod reached running or exited state
		if !podReady {
			testFailed = true
			t.Fatalf("Pod did not reach running or exited state within timeout period")
		}

		if !contains([]runpod.PodStatus{runpod.PodRunning, runpod.PodExited}, lastStatus) {
			testFailed = true
			t.Fatalf("Pod should reach running or exited state, got: %s", lastStatus)
		}

		t.Logf("Pod reached expected status: %s", lastStatus)
	})

	// Step 6: Terminate pod
	t.Run("6. TerminatePod", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		t.Log("Terminating pod...")
		err = client.TerminatePod(runpodID)
		if err != nil {
			testFailed = true
			t.Fatalf("Failed to terminate pod: %v", err)
		}

		// Store the ID and clear runpodID so we don't try to terminate it again in the defer block
		terminatedID = runpodID
		runpodID = ""

		t.Log("Pod termination initiated")
	})

	// Step 7: Verify pod termination
	t.Run("7. VerifyPodTermination", func(t *testing.T) {
		if testFailed {
			t.Skip("Skipping due to previous test failure")
		}

		if terminatedID == "" {
			testFailed = true
			t.Fatalf("No terminated pod ID to verify")
		}

		t.Log("Verifying pod termination...")

		// Wait for pod to be terminated
		deadline := time.Now().Add(2 * time.Minute)
		interval := 10 * time.Second
		terminated := false

		for time.Now().Before(deadline) && !terminated {
			status, err := client.GetPodStatusREST(terminatedID)
			if err != nil {
				t.Logf("Error getting pod status during termination: %v", err)
			} else {
				t.Logf("Pod termination status: %s", status)

				if status == runpod.PodTerminated || status == runpod.PodNotFound {
					terminated = true
					break
				}
			}

			time.Sleep(interval)
		}

		if !terminated {
			testFailed = true
			t.Fatalf("Pod was not confirmed terminated within timeout period")
		}

		t.Log("Pod termination verified successfully")
	})

	if testFailed {
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

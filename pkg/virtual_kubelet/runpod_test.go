package runpod_test

import (
	"context"
	"fmt"
	runpod "github.com/bsvogler/k8s-runpod-controller/pkg/virtual_kubelet"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRunPodCreateCheckAndTerminate(t *testing.T) {
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

	// Create a test pod configuration, the runpod needs to have some annotations like the template to even work with the API
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("runpod-test-pod-%d", time.Now().Unix()),
			Annotations: map[string]string{
				runpod.RunpodCloudTypeAnnotation:  "STANDARD", // Use community cloud for lower costs during testing
				runpod.GpuMemoryAnnotation:        "2",        // Request minimum 16GB GPU memory
				runpod.RunpodTemplateIdAnnotation: "8pjdlrsfmx",
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Name:    "test-container",
					Image:   "nvidia/cuda:11.7.1-base-ubuntu22.04", // Use a simple CUDA image
					Command: []string{"nvidia-smi", "-L"},          // List GPUs and exit
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

	// 1. Create the pod in Kubernetes
	createdPod, err := clientset.CoreV1().Pods(ns.Name).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create test pod")

	// Variables to track created RunPod instance
	var runpodID string
	var costPerHr float64

	// Ensure pod is cleaned up at the end
	defer func() {
		// Clean up the Kubernetes pod
		err := clientset.CoreV1().Pods(ns.Name).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Logf("Failed to delete pod: %v", err)
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

	// 2. Prepare RunPod parameters
	params, err := client.PrepareRunPodParameters(createdPod, false) // false for REST API
	require.NoError(t, err, "Failed to prepare RunPod parameters")

	// 3. Deploy pod to RunPod using REST API
	t.Log("Deploying pod to RunPod...")
	runpodID, costPerHr, err = client.DeployPodREST(params)
	require.NoError(t, err, "Failed to deploy pod to RunPod")
	require.NotEmpty(t, runpodID, "RunPod ID should not be empty")
	require.Greater(t, costPerHr, 0.0, "Cost per hour should be greater than 0")

	t.Logf("Pod deployed to RunPod with ID: %s, cost: $%.4f/hr", runpodID, costPerHr)

	// 4. Check pod status multiple ways
	t.Log("Checking pod status...")

	// Update the pod in Kubernetes with the RunPod ID
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s","%s":"%g"}}}`,
		runpod.RunpodPodIDAnnotation, runpodID,
		runpod.RunpodCostAnnotation, costPerHr)

	_, err = clientset.CoreV1().Pods(ns.Name).Patch(
		context.Background(),
		pod.Name,
		"application/strategic-merge-patch+json",
		[]byte(patch),
		metav1.PatchOptions{},
	)
	require.NoError(t, err, "Failed to update pod with RunPod ID")

	// 5. Wait for pod to start and check status with GraphQL API
	maxWaitTime := 5 * time.Minute
	interval := 10 * time.Second
	deadline := time.Now().Add(maxWaitTime)

	var lastStatus runpod.PodStatus
	var detailedStatus *runpod.RunPodDetailedStatus

	t.Logf("Waiting up to %s for pod to start...", maxWaitTime)

	for time.Now().Before(deadline) {
		// Check status using GraphQL API
		status, err := client.GetPodStatus(runpodID)
		if err == nil {
			lastStatus = status
			t.Logf("Pod status (GraphQL): %s", status)

			// If pod is running or has completed, we can proceed
			if status == runpod.PodRunning || status == runpod.PodExited {
				break
			}
		} else {
			t.Logf("Error getting pod status via GraphQL: %v", err)
		}

		// Also check status using REST API
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
					break
				}
			}
		} else {
			t.Logf("Error getting detailed pod status: %v", err)
		}

		time.Sleep(interval)
	}

	// Assert that pod reached running or exited state
	require.Contains(t, []runpod.PodStatus{runpod.PodRunning, runpod.PodExited}, lastStatus,
		"Pod should reach running or exited state")

	// 6. Terminate pod and verify termination
	t.Log("Terminating pod...")
	err = client.TerminatePod(runpodID)
	require.NoError(t, err, "Failed to terminate pod")

	// Set runpodID to empty so we don't try to terminate it again in the defer block
	terminatedID := runpodID
	runpodID = ""

	// 7. Verify pod termination
	t.Log("Verifying pod termination...")

	// Wait for pod to be terminated
	deadline = time.Now().Add(2 * time.Minute)
	terminated := false

	for time.Now().Before(deadline) {
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

	assert.True(t, terminated, "Pod should be terminated")

	t.Log("Test completed successfully")
}

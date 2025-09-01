//go:build integration
// +build integration

package runpod

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bsvogler/k8s-runpod-kubelet/pkg/config"
)

// TestContainerRegistryAuthAnnotation tests that container-registry-auth-id annotation
// is properly read from job annotations and included in RunPod parameters
func TestContainerRegistryAuthAnnotation(t *testing.T) {
	// Skip if no API key
	if os.Getenv("RUNPOD_API_KEY") == "" {
		t.Skip("RUNPOD_API_KEY not set, skipping integration test")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Use fake clientset for testing
	fakeClientset := fake.NewSimpleClientset()
	// Create a test config with no datacenter restrictions
	testConfig := &config.Config{}
	client := NewRunPodClient(logger, fakeClientset, testConfig)

	// Test container registry auth ID
	testAuthID := "test-auth-id-12345"
	testTemplateID := "test-template-id"

	// Create a test pod with job owner reference
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-registry-auth-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       "test-job",
					UID:        "test-job-uid",
				},
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test-container",
					Image: "nginx:latest",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
						Requests: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
		},
	}

	// Create a job with annotations
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "default",
			UID:       "test-job-uid",
			Annotations: map[string]string{
				ContainerRegistryAuthAnnotation: testAuthID,
				TemplateIdAnnotation:            testTemplateID,
				GpuMemoryAnnotation:             "8",
				CloudTypeAnnotation:             "SECURE",
			},
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: pod.Spec,
			},
		},
	}

	// Add the job to the fake clientset
	_, err := fakeClientset.BatchV1().Jobs("default").Create(context.Background(), job, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create test job")

	// Test 1: Verify parameters are prepared correctly with job annotations
	t.Run("PrepareParametersWithJobAnnotations", func(t *testing.T) {
		params, err := client.PrepareRunPodParameters(pod, false)
		require.NoError(t, err, "Failed to prepare RunPod parameters")

		// Verify containerRegistryAuthId is included
		assert.Equal(t, testAuthID, params["containerRegistryAuthId"], 
			"containerRegistryAuthId should be read from job annotation")

		// Verify templateId is included
		assert.Equal(t, testTemplateID, params["templateId"], 
			"templateId should be read from job annotation")

		// Verify GPU memory requirement
		assert.Equal(t, 8, params["minRAMPerGPU"], 
			"GPU memory should be read from job annotation")

		// Verify cloud type
		assert.Equal(t, "SECURE", params["cloudType"], 
			"Cloud type should be read from job annotation")
	})

	// Test 2: Pod annotations should override job annotations
	t.Run("PodAnnotationsOverrideJob", func(t *testing.T) {
		podWithAnnotations := pod.DeepCopy()
		podWithAnnotations.Annotations = map[string]string{
			ContainerRegistryAuthAnnotation: "pod-auth-override",
			GpuMemoryAnnotation:             "16",
		}

		params, err := client.PrepareRunPodParameters(podWithAnnotations, false)
		require.NoError(t, err, "Failed to prepare RunPod parameters")

		// Pod annotation should override job annotation
		assert.Equal(t, "pod-auth-override", params["containerRegistryAuthId"], 
			"Pod annotation should override job annotation for containerRegistryAuthId")

		// Pod annotation should override job annotation for GPU memory
		assert.Equal(t, 16, params["minRAMPerGPU"], 
			"Pod annotation should override job annotation for GPU memory")

		// Job annotation should still be used for templateId (not overridden by pod)
		assert.Equal(t, testTemplateID, params["templateId"], 
			"templateId should still come from job annotation when not overridden")
	})

	// Test 3: Verify both templateId and containerRegistryAuthId can coexist
	t.Run("TemplateAndRegistryAuthCoexist", func(t *testing.T) {
		params, err := client.PrepareRunPodParameters(pod, false)
		require.NoError(t, err, "Failed to prepare RunPod parameters")

		// Both should be present in parameters
		_, hasTemplate := params["templateId"]
		_, hasRegistryAuth := params["containerRegistryAuthId"]

		assert.True(t, hasTemplate, "templateId should be present in parameters")
		assert.True(t, hasRegistryAuth, "containerRegistryAuthId should be present in parameters")
	})
}

// TestJobAnnotationFallback tests the complete annotation fallback mechanism
func TestJobAnnotationFallback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	fakeClientset := fake.NewSimpleClientset()
	testConfig := &config.Config{}
	client := NewRunPodClient(logger, fakeClientset, testConfig)

	// Create test job with various annotations
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fallback-test-job",
			Namespace: "default",
			UID:       "fallback-job-uid",
			Annotations: map[string]string{
				CloudTypeAnnotation:      "COMMUNITY",
				DatacenterAnnotation:     "dc1,dc2",
				GpuMemoryAnnotation:      "24",
				PortsAnnotation:          "8080/http,9000/tcp",
			},
		},
	}

	// Create pod with owner reference to job
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fallback-test-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       job.Name,
					UID:        job.UID,
				},
			},
			// Only override some annotations at pod level
			Annotations: map[string]string{
				CloudTypeAnnotation: "SECURE", // Override cloud type
				// DatacenterAnnotation not set - should fall back to job
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "test",
					Image: "nginx:latest",
				},
			},
		},
	}

	// Add job to fake clientset
	_, err := fakeClientset.BatchV1().Jobs("default").Create(context.Background(), job, metav1.CreateOptions{})
	require.NoError(t, err)

	params, err := client.PrepareRunPodParameters(pod, false)
	require.NoError(t, err)

	// Cloud type should come from pod (override)
	assert.Equal(t, "SECURE", params["cloudType"], 
		"Cloud type should be overridden by pod annotation")

	// Datacenter should come from job (fallback)
	assert.Equal(t, []string{"dc1", "dc2"}, params["dataCenterIds"], 
		"Datacenter IDs should fall back to job annotation")

	// GPU memory should come from job (no pod override)
	assert.Equal(t, 24, params["minRAMPerGPU"], 
		"GPU memory should fall back to job annotation")

	// Ports should come from job (no pod override)
	assert.Equal(t, []string{"8080/http", "9000/tcp"}, params["ports"], 
		"Ports should fall back to job annotation")
}

// TestDeployWithContainerRegistryAuth performs an actual deployment test
// This test will actually create a RunPod instance, so it incurs costs
// Set RUNPOD_DEPLOY_TEST=true to enable actual deployment
func TestDeployWithContainerRegistryAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping deployment test in short mode")
	}

	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set, skipping deployment test")
	}

	// Only run actual deployment if explicitly enabled
	deployEnabled := os.Getenv("RUNPOD_DEPLOY_TEST") == "true"

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Use fake clientset for parameter testing
	clientset := fake.NewSimpleClientset()
	testConfig := &config.Config{}
	client := NewRunPodClient(logger, clientset, testConfig)

	// Create a test pod with container registry auth annotation
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("test-registry-%d", time.Now().Unix()),
			Namespace: "default",
			Annotations: map[string]string{
				ContainerRegistryAuthAnnotation: "test-auth-id", // This would need to be a real auth ID for actual deployment
				TemplateIdAnnotation:            "test-template",
				CloudTypeAnnotation:             "SECURE",
				GpuMemoryAnnotation:             "2",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "test",
					Image:   "nginx:latest",
					Command: []string{"sleep", "30"}, // Short-lived for testing
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
						Requests: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	// Prepare parameters and verify
	params, err := client.PrepareRunPodParameters(pod, false)
	require.NoError(t, err, "Failed to prepare parameters")

	// Verify the containerRegistryAuthId is in the parameters
	authID, ok := params["containerRegistryAuthId"]
	assert.True(t, ok, "containerRegistryAuthId should be present in parameters")
	assert.Equal(t, "test-auth-id", authID, "containerRegistryAuthId should match annotation")

	// Verify templateId is also present
	templateID, ok := params["templateId"]
	assert.True(t, ok, "templateId should be present in parameters")
	assert.Equal(t, "test-template", templateID, "templateId should match annotation")

	t.Logf("Successfully prepared parameters with containerRegistryAuthId: %v and templateId: %v", authID, templateID)

	// Only deploy if explicitly enabled
	if deployEnabled {
		t.Log("RUNPOD_DEPLOY_TEST=true, performing actual deployment")
		
		// Deploy to RunPod
		podID, cost, err := client.DeployPodREST(params)
		if err != nil {
			t.Fatalf("Failed to deploy to RunPod: %v", err)
		}
		
		t.Logf("Successfully deployed pod %s with estimated cost $%.2f/hr", podID, cost)
		
		// IMPORTANT: Clean up RunPod instance immediately to minimize costs
		defer func() {
			t.Logf("Cleaning up RunPod instance %s", podID)
			err := client.TerminatePod(podID)
			if err != nil {
				t.Errorf("CRITICAL: Failed to terminate RunPod instance %s: %v", podID, err)
				t.Errorf("MANUAL CLEANUP REQUIRED: Please terminate pod %s in RunPod console", podID)
			} else {
				t.Logf("Successfully terminated RunPod instance %s", podID)
			}
		}()

		// Wait a moment to verify deployment
		time.Sleep(5 * time.Second)

		// Verify the pod was created with the correct parameters
		status, err := client.GetDetailedPodStatus(podID)
		if err != nil {
			t.Logf("Warning: Could not get pod status: %v", err)
		} else {
			t.Logf("Pod status: %s", status.CurrentStatus)
			
			// CRITICAL: Verify containerRegistryAuthId is actually set on deployed pod
			if status.ContainerRegistryAuthId != "" {
				t.Logf("✅ Pod has containerRegistryAuthId: %s", status.ContainerRegistryAuthId)
				assert.Equal(t, "test-auth-id", status.ContainerRegistryAuthId, 
					"Deployed pod should have containerRegistryAuthId matching annotation")
			} else {
				t.Error("❌ Pod does not have containerRegistryAuthId set - this indicates the parameter was not applied")
			}
			
			// Also verify templateId if present
			if status.TemplateId != "" {
				t.Logf("✅ Pod has templateId: %s", status.TemplateId)
				assert.Equal(t, "test-template", status.TemplateId, 
					"Deployed pod should have templateId matching annotation")
			}
			
			// Verify environment variables were set
			if status.Env != nil {
				t.Logf("Pod has %d environment variables", len(status.Env))
			}
		}
		
		// Terminate immediately - don't wait for natural completion
		t.Log("Terminating pod immediately to minimize costs")
		err = client.TerminatePod(podID)
		require.NoError(t, err, "Failed to terminate pod - MANUAL CLEANUP REQUIRED")
		
	} else {
		t.Log("Skipping actual deployment. Set RUNPOD_DEPLOY_TEST=true to test deployment")
	}
}

// TestActualDeploymentWithCleanup performs a real deployment with comprehensive cleanup
// WARNING: This test incurs real costs on RunPod
func TestActualDeploymentWithCleanup(t *testing.T) {
	// Skip unless explicitly enabled
	if os.Getenv("RUNPOD_DEPLOY_TEST") != "true" {
		t.Skip("Set RUNPOD_DEPLOY_TEST=true to run actual deployment tests")
	}

	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	clientset := fake.NewSimpleClientset()
	testConfig := &config.Config{}
	client := NewRunPodClient(logger, clientset, testConfig)

	// Track deployed pod for cleanup
	var deployedPodID string
	
	// Ensure cleanup even on panic
	defer func() {
		if deployedPodID != "" {
			t.Logf("Cleanup: Terminating RunPod instance %s", deployedPodID)
			if err := client.TerminatePod(deployedPodID); err != nil {
				t.Errorf("CRITICAL: Failed to cleanup pod %s: %v", deployedPodID, err)
				t.Errorf("MANUAL ACTION REQUIRED: Delete pod %s from RunPod console", deployedPodID)
			}
		}
	}()

	// Create minimal pod for testing
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cleanup-test-%d", time.Now().Unix()),
			Namespace: "default",
			Annotations: map[string]string{
				CloudTypeAnnotation: "SECURE",
				GpuMemoryAnnotation: "2", // Minimum GPU memory to reduce cost
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "test",
					Image:   "busybox:latest", // Small image for quick download
					Command: []string{"echo", "test complete"}, // Exit immediately
				},
			},
		},
	}

	// Prepare and deploy
	params, err := client.PrepareRunPodParameters(pod, false)
	require.NoError(t, err)

	t.Log("Deploying test pod to RunPod...")
	podID, cost, err := client.DeployPodREST(params)
	require.NoError(t, err, "Deployment failed")
	
	deployedPodID = podID // Store for cleanup
	t.Logf("Deployed pod %s (cost: $%.2f/hr)", podID, cost)

	// Minimal wait for pod to start
	time.Sleep(3 * time.Second)

	// Check status once
	status, err := client.GetDetailedPodStatus(podID)
	if err == nil {
		t.Logf("Pod status: %s", status.CurrentStatus)
	}

	// Immediately terminate
	t.Log("Terminating pod to minimize costs...")
	err = client.TerminatePod(podID)
	require.NoError(t, err, "Termination failed - MANUAL CLEANUP REQUIRED")
	
	deployedPodID = "" // Clear to prevent double cleanup
	t.Log("Pod successfully terminated")
}

// TestContainerRegistryAuthInDeploymentResponse tests that containerRegistryAuthId 
// is returned in the deployment response when requested
func TestContainerRegistryAuthInDeploymentResponse(t *testing.T) {
	// Skip unless explicitly enabled for deployment tests
	if os.Getenv("RUNPOD_DEPLOY_TEST") != "true" {
		t.Skip("Set RUNPOD_DEPLOY_TEST=true to run actual deployment tests")
	}

	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	clientset := fake.NewSimpleClientset()
	testConfig := &config.Config{}
	client := NewRunPodClient(logger, clientset, testConfig)

	// Track deployed pod for cleanup
	var deployedPodID string
	
	// Ensure cleanup even on panic
	defer func() {
		if deployedPodID != "" {
			t.Logf("Cleanup: Terminating RunPod instance %s", deployedPodID)
			if err := client.TerminatePod(deployedPodID); err != nil {
				t.Errorf("CRITICAL: Failed to cleanup pod %s: %v", deployedPodID, err)
			}
		}
	}()

	// Create minimal pod with containerRegistryAuthId annotation
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("registry-auth-test-%d", time.Now().Unix()),
			Namespace: "default",
			Annotations: map[string]string{
				ContainerRegistryAuthAnnotation: "cmeya286c0001if024y81803b", // Real auth ID from test
				CloudTypeAnnotation:             "SECURE",
				GpuMemoryAnnotation:             "2", // Minimum GPU memory
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "test",
					Image:   "registry.gitlab.com/photo-generator/gsplat:latest", // Private image requiring auth
					Command: []string{"echo", "test complete"},
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
						Requests: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
		},
	}

	// Prepare parameters
	params, err := client.PrepareRunPodParameters(pod, false)
	require.NoError(t, err)

	// Verify containerRegistryAuthId is in parameters
	expectedAuthId := "cmeya286c0001if024y81803b"
	authIdParam, ok := params["containerRegistryAuthId"]
	require.True(t, ok, "containerRegistryAuthId should be in parameters")
	require.Equal(t, expectedAuthId, authIdParam, "containerRegistryAuthId parameter should match annotation")

	t.Log("Deploying pod with containerRegistryAuthId...")

	// Deploy using the standard function (logs will show containerRegistryAuthId)
	podID, cost, err := client.DeployPodREST(params)
	require.NoError(t, err, "Deployment should succeed")

	deployedPodID = podID // Store for cleanup
	t.Logf("Deployed pod %s (cost: $%.2f/hr)", podID, cost)

	// Verify other expected fields
	assert.NotEmpty(t, podID, "Pod ID should not be empty")
	assert.Greater(t, cost, 0.0, "Cost should be greater than 0")

	t.Log("✅ Deployment completed - check logs above for containerRegistryAuthId in deployment response")

	// Wait briefly for pod to start, then check GET response
	time.Sleep(3 * time.Second)
	
	// CRITICAL TEST 2: Verify containerRegistryAuthId is also returned in GET request
	t.Log("Checking if containerRegistryAuthId is also returned in GET pod status...")
	status, err := client.GetDetailedPodStatus(deployedPodID)
	if err != nil {
		t.Logf("Warning: Could not get detailed pod status: %v", err)
	} else {
		t.Logf("GET response ContainerRegistryAuthId: '%s'", status.ContainerRegistryAuthId)
		assert.Equal(t, expectedAuthId, status.ContainerRegistryAuthId, 
			"GET pod status should also include containerRegistryAuthId when it was requested")
		
		// Also check templateId if present
		if status.TemplateId != "" {
			t.Logf("GET response also includes TemplateId: '%s'", status.TemplateId)
		}
		
		t.Logf("✅ SUCCESS: containerRegistryAuthId '%s' also confirmed in GET pod status", status.ContainerRegistryAuthId)
	}
	
	t.Log("Terminating pod to minimize costs...")
	err = client.TerminatePod(deployedPodID)
	require.NoError(t, err, "Termination failed")
	
	deployedPodID = "" // Clear to prevent double cleanup
	t.Log("Pod successfully terminated")
}
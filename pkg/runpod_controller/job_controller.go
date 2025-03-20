package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	"github.com/go-logr/logr"
	"io"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Constants for RunPod integration
	RunpodManagedAnnotation   = "runpod.io/managed"
	RunpodOffloadAnnotation   = "runpod.io/offload"
	RunpodOffloadedAnnotation = "runpod.io/offloaded"
	RunpodPodIDAnnotation     = "runpod.io/pod-id"
	RunpodCostAnnotation      = "runpod.io/cost-per-hr"
	RunpodManagedLabel        = "runpod.io/managed"

	// Annotation for GPU memory requirements
	GpuMemoryAnnotation = "runpod.io/required-gpu-memory"

	// Default max price for GPU
	DefaultMaxPrice = 0.5
)

// RunPodEnv represents the environment variable structure for RunPod API
type RunPodEnv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// GPUType represents the GPU type from RunPod API
type GPUType struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"displayName"`
	MemoryInGb  int     `json:"memoryInGb"`
	SecureCloud bool    `json:"secureCloud"`
	SecurePrice float64 `json:"securePrice"`
}

// RunPodResponse represents the response from RunPod API
type RunPodResponse struct {
	Data struct {
		GPUTypes []GPUType `json:"gpuTypes"`
	} `json:"data"`
}

// DeploymentResponse represents the response from RunPod deployment API
type DeploymentResponse struct {
	Data struct {
		PodFindAndDeployOnDemand struct {
			ID        string  `json:"id"`
			ImageName string  `json:"imageName"`
			MachineID string  `json:"machineId"`
			CostPerHr float64 `json:"costPerHr"`
			Machine   struct {
				PodHostID string `json:"podHostId"`
			} `json:"machine"`
		} `json:"podFindAndDeployOnDemand"`
	} `json:"data"`
}

// JobController manages Kubernetes jobs and offloads them to RunPod when necessary
type JobController struct {
	clientset        *kubernetes.Clientset
	logger           logr.Logger
	config           config.Config
	httpClient       *http.Client
	runpodKey        string
	maxPrice         float64
	deletedJobs      map[string]string // Maps job name to runpod ID for cleanup
	deletedJobsMutex sync.Mutex
	runpodAvailable  bool // Tracks if RunPod API is available
	healthServer     *HealthServer
}

// NewJobController creates a new JobController instance
func NewJobController(clientset *kubernetes.Clientset, logger logr.Logger, cfg config.Config) *JobController {
	runpodKey := os.Getenv("RUNPOD_KEY")
	if runpodKey == "" {
		logger.Error(nil, "RUNPOD_KEY environment variable is not set")
	}

	maxPrice := DefaultMaxPrice
	if cfg.MaxGPUPrice > 0 {
		maxPrice = cfg.MaxGPUPrice
	}

	controller := &JobController{
		clientset:       clientset,
		logger:          logger,
		config:          cfg,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		runpodKey:       runpodKey,
		maxPrice:        maxPrice,
		deletedJobs:     make(map[string]string),
		runpodAvailable: true, // Initially assume RunPod is available
	}

	// Create health server
	controller.healthServer = NewHealthServer(cfg.HealthServerAddress, controller.isReady)

	return controller
}

// cleanupTerminatingPods checks for pods in Terminating state on the RunPod virtual node
// and ensures they are properly terminated both in K8s and on RunPod
func (c *JobController) cleanupTerminatingPods() error {
	c.logger.Info("Checking for terminating pods on RunPod virtual node")

	// Check for any pods that might be terminating but stuck
	allPods, err := c.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: "runpod.io/managed=true",
		},
	)
	if err != nil {
		return fmt.Errorf("failed to list all RunPod managed pods: %w", err)
	}

	terminatingCount := 0
	for _, pod := range allPods.Items {
		// Check if pod is terminating (has a deletion timestamp but still exists)
		if pod.DeletionTimestamp != nil {
			terminatingCount++
			deletionTime := pod.DeletionTimestamp.Time
			terminatingDuration := time.Since(deletionTime)

			c.logger.Info("Found terminating pod",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"deletionTimestamp", pod.DeletionTimestamp,
				"terminatingFor", terminatingDuration.String())

			// If pod has been terminating for more than 15 minutes, force delete it regardless of RunPod status
			forceDeletionThreshold := 15 * time.Minute
			if terminatingDuration > forceDeletionThreshold {
				c.logger.Info("Pod has been terminating for too long, force deleting",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"terminatingFor", terminatingDuration.String())

				if err := c.forceDeletePod(pod.Namespace, pod.Name); err != nil {
					c.logger.Error(err, "Failed to force delete long-terminating pod",
						"pod", pod.Name,
						"namespace", pod.Namespace)
				}
				continue
			}

			// Get RunPod ID from pod annotations
			runpodID, exists := pod.Annotations[RunpodPodIDAnnotation]
			if !exists {
				c.logger.Info("Terminating pod missing RunPod ID annotation, will force delete",
					"pod", pod.Name,
					"namespace", pod.Namespace)

				// Force delete the pod as it has no RunPod ID
				if err := c.forceDeletePod(pod.Namespace, pod.Name); err != nil {
					c.logger.Error(err, "Failed to force delete pod without RunPod ID",
						"pod", pod.Name,
						"namespace", pod.Namespace)
				}
				continue
			}

			// Check if the RunPod instance is still running
			podStatus, err := c.checkRunPodInstanceStatus(runpodID)
			if err != nil {
				c.logger.Error(err, "Failed to check RunPod instance status",
					"runpodID", runpodID,
					"pod", pod.Name)

				// If we get a GraphQL validation error or any other API error,
				// assume the pod doesn't exist on RunPod anymore and force delete it
				if strings.Contains(err.Error(), "GRAPHQL_VALIDATION_FAILED") ||
					strings.Contains(err.Error(), "Something went wrong") {
					c.logger.Info("RunPod API returned error, assuming instance no longer exists. Force deleting K8s pod",
						"runpodID", runpodID,
						"pod", pod.Name)

					if err := c.forceDeletePod(pod.Namespace, pod.Name); err != nil {
						c.logger.Error(err, "Failed to force delete pod after RunPod API error",
							"pod", pod.Name,
							"namespace", pod.Namespace)
					}
				}
				continue
			}

			// If RunPod instance is still active, terminate it
			if podStatus == "RUNNING" || podStatus == "STARTING" {
				c.logger.Info("RunPod instance is still active, terminating it",
					"runpodID", runpodID,
					"pod", pod.Name,
					"status", podStatus)

				if err := c.terminateRunPodInstance(runpodID); err != nil {
					c.logger.Error(err, "Failed to terminate RunPod instance",
						"runpodID", runpodID,
						"pod", pod.Name)
					continue
				}

				// Wait briefly for termination to be processed
				time.Sleep(2 * time.Second)
			} else if podStatus == "TERMINATED" || podStatus == "TERMINATING" || podStatus == "NOT_FOUND" {
				// If pod is already terminated or not found, just force delete it from K8s
				c.logger.Info("RunPod instance is already terminated or not found, cleaning up K8s pod",
					"runpodID", runpodID,
					"pod", pod.Name,
					"status", podStatus)

				if err := c.forceDeletePod(pod.Namespace, pod.Name); err != nil {
					c.logger.Error(err, "Failed to force delete pod",
						"pod", pod.Name,
						"namespace", pod.Namespace)
				}
			}
		}
	}

	c.logger.Info("Terminating pod cleanup completed", "processed", terminatingCount)
	return nil
}

// checkRunPodInstanceStatus checks the status of a RunPod instance
func (c *JobController) checkRunPodInstanceStatus(podID string) (string, error) {
	url := fmt.Sprintf("https://api.runpod.io/graphql?api_key=%s", c.runpodKey)

	// Using the format from the RunPod documentation
	query := `
		query pod($input: PodFilter) {
			pod(input: $input) {
				id
				desiredStatus
				currentStatus
			}
		}
	`

	// Create variables for the GraphQL query
	variables := map[string]interface{}{
		"input": map[string]string{
			"podId": podID,
		},
	}

	// Create the request body with both query and variables
	reqBody, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("RunPod API returned error: %d %s", resp.StatusCode, string(body))
	}

	// Parse response to get pod status
	var response struct {
		Data struct {
			Pod struct {
				ID            string `json:"id"`
				DesiredStatus string `json:"desiredStatus"`
				CurrentStatus string `json:"currentStatus"`
			} `json:"pod"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", err
	}

	// Check for API errors
	if len(response.Errors) > 0 {
		// If pod not found error, return a special status
		if strings.Contains(strings.ToLower(response.Errors[0].Message), "not found") {
			return "NOT_FOUND", nil
		}
		return "", fmt.Errorf("RunPod API error: %s", response.Errors[0].Message)
	}

	// Check if pod data is missing
	if response.Data.Pod.ID == "" {
		return "NOT_FOUND", nil
	}

	// Return current status if available, otherwise desired status
	if response.Data.Pod.CurrentStatus != "" {
		return response.Data.Pod.CurrentStatus, nil
	}
	return response.Data.Pod.DesiredStatus, nil
}

// forceDeletePod forcefully removes a pod from the Kubernetes API
func (c *JobController) forceDeletePod(namespace, name string) error {
	// Create zero grace period for immediate deletion
	gracePeriod := int64(0)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &[]metav1.DeletionPropagation{metav1.DeletePropagationBackground}[0],
	}

	err := c.clientset.CoreV1().Pods(namespace).Delete(
		context.Background(),
		name,
		deleteOptions,
	)

	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	c.logger.Info("Successfully force deleted pod", "pod", name, "namespace", namespace)
	return nil
}

// Start begins the controller's reconciliation loop
func (c *JobController) Start() error {
	reconcileTicker := time.NewTicker(c.config.ReconcileInterval)
	cleanupTicker := time.NewTicker(5 * time.Minute)        // Check for cleanup every 5 minutes
	terminatingPodTicker := time.NewTicker(1 * time.Minute) // Check for terminating pods every minute
	healthCheckTicker := time.NewTicker(1 * time.Minute)    // Check RunPod API health every minute
	defer reconcileTicker.Stop()
	defer cleanupTicker.Stop()
	defer terminatingPodTicker.Stop()
	defer healthCheckTicker.Stop()

	// Start health server
	c.healthServer.Start()

	// Set up channels for graceful shutdown
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		for {
			select {
			case <-reconcileTicker.C:
				if err := c.reconcile(); err != nil {
					c.logger.Error(err, "reconciliation failed")
				}
			case <-cleanupTicker.C:
				if err := c.cleanupDeletedJobs(); err != nil {
					c.logger.Error(err, "cleanup failed")
				}
			case <-terminatingPodTicker.C:
				if err := c.cleanupTerminatingPods(); err != nil {
					c.logger.Error(err, "terminating pod cleanup failed")
				}
			case <-healthCheckTicker.C:
				c.checkRunPodHealth()
			case <-stopCh:
				return
			}
		}
	}()

	<-stopCh
	c.logger.Info("Stopping controller")

	// Stop health server
	if err := c.healthServer.Stop(); err != nil {
		c.logger.Error(err, "failed to stop health server")
	}

	// Wait for cleanup to finish
	select {
	case <-doneCh:
		c.logger.Info("Controller stopped gracefully")
	case <-time.After(30 * time.Second):
		c.logger.Info("Controller stop timed out")
	}

	return nil
}

// reconcile checks for jobs that need to be offloaded to RunPod
func (c *JobController) reconcile() error {
	// List all jobs with the runpod.io/managed annotation
	jobs, err := c.clientset.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	pendingCount := 0
	activeJobs := make(map[string]bool)

	for _, job := range jobs.Items {
		// Skip jobs that don't have the runpod.io/managed annotation
		if !isRunPodJob(job) {
			continue
		}

		// Track all active job names
		jobKey := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
		activeJobs[jobKey] = true

		// Check if job needs labeling
		if !hasRunPodLabel(job) {
			if err := c.labelAsRunPodJob(&job); err != nil {
				c.logger.Error(err, "failed to label RunPod job", "job", job.Name)
				continue
			}
		}

		// Check if job should be offloaded to RunPod
		if isPending(job) {
			pendingCount++
			if shouldOffloadToRunPod(job, pendingCount, c.config) {
				if err := c.offloadJobToRunPod(job); err != nil {
					c.logger.Error(err, "failed to offload job to RunPod", "job", job.Name)
				}
			}
		}
	}

	// Check for deleted jobs and update the tracking map
	c.deletedJobsMutex.Lock()
	for jobKey := range c.deletedJobs {
		if !activeJobs[jobKey] {
			// Job is no longer in the API server but still in our map
			// Keep it in the map for cleanup to handle
			c.logger.Info("Job marked for cleanup", "job", jobKey)
		}
	}
	c.deletedJobsMutex.Unlock()

	return nil
}

// isRunPodJob checks if a job should be managed by RunPod
func isRunPodJob(job batchv1.Job) bool {
	_, hasRunPodAnnotation := job.Annotations[RunpodManagedAnnotation]
	return hasRunPodAnnotation
}

// hasRunPodLabel checks if a job has the RunPod managed label
func hasRunPodLabel(job batchv1.Job) bool {
	_, hasLabel := job.Labels[RunpodManagedLabel]
	return hasLabel
}

// shouldOffloadToRunPod determines if a job should be offloaded to RunPod
func shouldOffloadToRunPod(job batchv1.Job, pendingCounter int, cfg config.Config) bool {
	// Already offloaded
	if _, hasOffloaded := job.Annotations[RunpodOffloadedAnnotation]; hasOffloaded {
		return false
	}

	// Check for explicit offload annotation
	value, exists := job.Annotations[RunpodOffloadAnnotation]
	if exists && value == "true" {
		return true
	}

	// Check if job has been pending for too long
	creationTime := job.CreationTimestamp.Time
	pendingTime := time.Since(creationTime)

	return pendingTime > time.Duration(cfg.MaxPendingTime)*time.Second || pendingCounter > cfg.PendingJobThreshold
}

// labelAsRunPodJob adds the RunPod managed label to a job
func (c *JobController) labelAsRunPodJob(job *batchv1.Job) error {
	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}
	job.Labels[RunpodManagedLabel] = "true"

	_, err := c.clientset.BatchV1().Jobs(job.Namespace).Update(
		context.Background(),
		job,
		metav1.UpdateOptions{},
	)
	return err
}

// isPending checks if a job is in a pending state
func isPending(job batchv1.Job) bool {
	return job.Status.Active > 0 && job.Status.Succeeded == 0 && job.Status.Failed == 0
}

// getPossibleGPUs gets available GPU types from RunPod API that match requirements
func (c *JobController) getPossibleGPUs(minMemoryInGb int) (string, error) {
	url := fmt.Sprintf("https://api.runpod.io/graphql?api_key=%s", c.runpodKey)

	query := `
		query GpuTypes {
			gpuTypes {
				id
				displayName
				memoryInGb
				secureCloud
				securePrice
			}
		}
	`

	reqBody, err := json.Marshal(map[string]string{
		"query": query,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var response RunPodResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", err
	}

	var filteredGPUs []GPUType
	for _, gpu := range response.Data.GPUTypes {
		if gpu.SecureCloud &&
			gpu.SecurePrice > 0 &&
			gpu.SecurePrice < c.maxPrice &&
			gpu.MemoryInGb >= minMemoryInGb {
			filteredGPUs = append(filteredGPUs, gpu)
		}
	}

	// Sort by price ascending
	sort.Slice(filteredGPUs, func(i, j int) bool {
		return filteredGPUs[i].SecurePrice < filteredGPUs[j].SecurePrice
	})

	// Take up to 5 GPUs
	var gpuIDs []string
	for i, gpu := range filteredGPUs {
		if i >= 5 {
			break
		}
		gpuIDs = append(gpuIDs, gpu.ID)
	}

	// Format as GraphQL array
	var gpuIDsStr []string
	for _, id := range gpuIDs {
		gpuIDsStr = append(gpuIDsStr, fmt.Sprintf(`"%s"`, id))
	}

	return "[" + strings.Join(gpuIDsStr, ", ") + "]", nil
}

// extractEnvVars extracts environment variables from the Kubernetes job
func (c *JobController) extractEnvVars(job batchv1.Job) ([]RunPodEnv, error) {
	var envVars []RunPodEnv

	// Extract all environment variables from the job containers
	if len(job.Spec.Template.Spec.Containers) > 0 {
		container := job.Spec.Template.Spec.Containers[0]
		for _, env := range container.Env {
			// Skip empty values and secret refs (these will be handled by volume mounts)
			if env.Value == "" || env.ValueFrom != nil {
				continue
			}

			// Add the environment variable
			envVars = append(envVars, RunPodEnv{
				Key:   env.Name,
				Value: env.Value,
			})
		}
	}

	// Handle environment variables from secrets that should be included
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			// Get the secret
			secret, err := c.clientset.CoreV1().Secrets(job.Namespace).Get(
				context.Background(),
				volume.Secret.SecretName,
				metav1.GetOptions{},
			)
			if err != nil {
				c.logger.Error(err, "failed to get secret",
					"namespace", job.Namespace,
					"secret", volume.Secret.SecretName)
				continue
			}

			// Check if this secret should be included as environment variables
			if items := volume.Secret.Items; len(items) > 0 {
				for _, item := range items {
					if secretValue, ok := secret.Data[item.Key]; ok {
						// Add it as an environment variable
						envVars = append(envVars, RunPodEnv{
							Key:   item.Key,
							Value: strings.ReplaceAll(string(secretValue), "\n", "\\n"),
						})
					}
				}
			}
		}
	}

	return envVars, nil
}

// formatEnvVarsForGraphQL formats environment variables for GraphQL query
func formatEnvVarsForGraphQL(envVars []RunPodEnv) string {
	var envVarsStr []string
	for _, env := range envVars {
		envVarsStr = append(envVarsStr, fmt.Sprintf(`{key: "%s", value: "%s"}`, env.Key, env.Value))
	}
	return strings.Join(envVarsStr, ",\n")
}

// updateJobWithRetry updates a job with retry logic to handle concurrent modifications
func (c *JobController) updateJobWithRetry(job *batchv1.Job) error {
	// Maximum number of retries
	maxRetries := 5
	retryCount := 0

	for retryCount < maxRetries {
		// Get the latest version of the job
		latestJob, err := c.clientset.BatchV1().Jobs(job.Namespace).Get(
			context.Background(),
			job.Name,
			metav1.GetOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to get latest job version: %w", err)
		}

		// Apply our annotations to the latest job version
		if latestJob.Annotations == nil {
			latestJob.Annotations = make(map[string]string)
		}
		// Copy over the annotations we want to set
		for k, v := range job.Annotations {
			if strings.HasPrefix(k, "runpod.io/") {
				latestJob.Annotations[k] = v
			}
		}

		// Update the job with the latest version
		_, err = c.clientset.BatchV1().Jobs(latestJob.Namespace).Update(
			context.Background(),
			latestJob,
			metav1.UpdateOptions{},
		)
		if err == nil {
			// Update successful
			return nil
		}

		// Check if it's a conflict error
		if !k8serrors.IsConflict(err) {
			// If it's not a conflict error, return immediately
			return err
		}

		// Increment retry count
		retryCount++

		// Wait a short time before retrying
		time.Sleep(100 * time.Millisecond)

		c.logger.Info("Retrying job update after conflict",
			"job", job.Name,
			"namespace", job.Namespace,
			"retry", retryCount)
	}

	return fmt.Errorf("failed to update job after %d retries: %s/%s", maxRetries, job.Namespace, job.Name)
}

// offloadJobToRunPod sends the job to RunPod and creates a corresponding Pod representation
func (c *JobController) offloadJobToRunPod(job batchv1.Job) error {
	// Determine minimum GPU memory required
	minMemoryInGb := 16 // Default minimum memory
	if memStr, exists := job.Annotations[GpuMemoryAnnotation]; exists {
		if mem, err := strconv.Atoi(memStr); err == nil {
			minMemoryInGb = mem
		}
	}

	// Get GPU types
	gpuTypes, err := c.getPossibleGPUs(minMemoryInGb)
	if err != nil {
		return err
	}

	// Extract environment variables from job
	envVars, err := c.extractEnvVars(job)
	if err != nil {
		return err
	}

	// Format environment variables for GraphQL
	formattedEnvVars := formatEnvVarsForGraphQL(envVars)

	// Determine image name from job
	var imageName string
	if len(job.Spec.Template.Spec.Containers) > 0 {
		imageName = job.Spec.Template.Spec.Containers[0].Image
	} else {
		return fmt.Errorf("job has no containers")
	}

	// Get volume size (if specified)
	volumeInGb := 0
	containerDiskInGb := 15 // Default disk size

	// Add namespace to the job name to ensure uniqueness
	runpodJobName := fmt.Sprintf("%s-%s", job.Namespace, job.Name)

	// Prepare RunPod GraphQL query
	query := fmt.Sprintf(`
		mutation {
			podFindAndDeployOnDemand(
				input: {
					cloudType: SECURE,
					gpuCount: 1,
					volumeInGb: %d,
					containerDiskInGb: %d,
					minVcpuCount: 2,
					minMemoryInGb: %d,
					gpuTypeIdList: %s,
					name: "%s",
					imageName: "%s",
					env: [%s]
				}
			) {
				id
				imageName
				machineId
				costPerHr
				machine {
					podHostId
				}
			}
		}
	`, volumeInGb, containerDiskInGb, minMemoryInGb, gpuTypes, runpodJobName, imageName, formattedEnvVars)

	// Call RunPod API
	url := fmt.Sprintf("https://api.runpod.io/graphql?api_key=%s", c.runpodKey)

	reqBody, err := json.Marshal(map[string]string{
		"query": query,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("RunPod API returned error: %d %s", resp.StatusCode, string(body))
	}

	var response DeploymentResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}

	runpodID := response.Data.PodFindAndDeployOnDemand.ID
	costPerHr := response.Data.PodFindAndDeployOnDemand.CostPerHr

	// Update job annotations
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}

	job.Annotations[RunpodPodIDAnnotation] = runpodID
	job.Annotations[RunpodOffloadedAnnotation] = "true"
	job.Annotations[RunpodCostAnnotation] = fmt.Sprintf("%f", costPerHr)

	// Add to tracking map for potential cleanup
	jobKey := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
	c.deletedJobsMutex.Lock()
	c.deletedJobs[jobKey] = runpodID
	c.deletedJobsMutex.Unlock()

	// Create a Pod representation of the RunPod instance
	podName := fmt.Sprintf("%s-runpod", job.Name)

	// Create labels to link Pod to Job
	podLabels := make(map[string]string)
	for k, v := range job.Labels {
		podLabels[k] = v
	}
	podLabels["job-name"] = job.Name
	podLabels["runpod.io/managed"] = "true"
	podLabels["runpod.io/pod-id"] = runpodID

	// Create annotations for the Pod
	podAnnotations := make(map[string]string)
	podAnnotations[RunpodPodIDAnnotation] = runpodID
	podAnnotations[RunpodCostAnnotation] = fmt.Sprintf("%f", costPerHr)
	podAnnotations["runpod.io/job-name"] = job.Name
	podAnnotations["runpod.io/external"] = "true"

	// Add owner references
	ownerReferences := []metav1.OwnerReference{
		{
			APIVersion: "batch/v1",
			Kind:       "Job",
			Name:       job.Name,
			UID:        job.UID,
			Controller: boolPtr(true),
		},
	}

	// Create Pod object
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       job.Namespace,
			Labels:          podLabels,
			Annotations:     podAnnotations,
			OwnerReferences: ownerReferences,
		},
		Spec: corev1.PodSpec{
			// Use the job's pod template but with modified node selector
			Containers: []corev1.Container{
				{
					Name:  "runpod-proxy",
					Image: imageName,
					// No resources as this is a virtual pod
					Command: []string{"/bin/sh", "-c", "echo 'This pod represents a RunPod instance'; sleep infinity"},
				},
			},
			RestartPolicy: "Never",
			NodeName:      "runpod-virtual-node", // This prevents scheduling on real nodes
			NodeSelector: map[string]string{
				"runpod.io/virtual": "true", // This ensures it won't be scheduled on a real node
			},
			Tolerations: []corev1.Toleration{
				{
					Key:      "runpod.io/virtual",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	// Create the Pod
	_, err = c.clientset.CoreV1().Pods(job.Namespace).Create(
		context.Background(),
		pod,
		metav1.CreateOptions{},
	)
	if err != nil {
		c.logger.Error(err, "Failed to create virtual pod for RunPod instance",
			"pod", podName, "runpodID", runpodID)
		// Continue even if pod creation fails, as the job is still offloaded to RunPod
	}

	// Update Job status to mark as offloaded
	jobCopy := job.DeepCopy()

	// Mark Job as "active" by creating a "Running" pod
	if jobCopy.Status.Active == 0 {
		jobCopy.Status.Active = 1
		jobCopy.Status.StartTime = &metav1.Time{Time: time.Now()}

		// Update job status in Kubernetes
		_, err = c.clientset.BatchV1().Jobs(jobCopy.Namespace).UpdateStatus(
			context.Background(),
			jobCopy,
			metav1.UpdateOptions{},
		)
		if err != nil {
			c.logger.Error(err, "Failed to update job status",
				"job", jobCopy.Name, "namespace", jobCopy.Namespace)
		}
	}

	// Update the job in Kubernetes with annotations
	if err := c.updateJobWithRetry(&job); err != nil {
		return fmt.Errorf("failed to update job annotations: %w", err)
	}

	c.logger.Info("Successfully offloaded job to RunPod",
		"job", job.Name,
		"namespace", job.Namespace,
		"runpodID", runpodID,
		"costPerHour", costPerHr,
		"virtualPod", podName)

	return nil
}

// cleanupDeletedJobs checks for jobs that have been deleted from Kubernetes
// and terminates the corresponding RunPod instances
func (c *JobController) cleanupDeletedJobs() error {
	c.logger.Info("Starting RunPod cleanup")

	c.deletedJobsMutex.Lock()
	defer c.deletedJobsMutex.Unlock()

	// Get current jobs to confirm which are deleted
	currentJobs, err := c.clientset.BatchV1().Jobs("").List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: "runpod.io/managed=true",
		},
	)
	if err != nil {
		return fmt.Errorf("failed to list jobs for cleanup: %w", err)
	}

	// Build a map of existing jobs
	existingJobs := make(map[string]bool)
	for _, job := range currentJobs.Items {
		jobKey := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
		existingJobs[jobKey] = true
	}

	// Track jobs to remove from map
	var jobsToRemove []string

	// Check each tracked job
	for jobKey, runpodID := range c.deletedJobs {
		if !existingJobs[jobKey] {
			// Job no longer exists in K8s, terminate the RunPod instance
			c.logger.Info("Terminating RunPod for deleted job", "job", jobKey, "runpodID", runpodID)

			// Call RunPod API to terminate the pod
			if err := c.terminateRunPodInstance(runpodID); err != nil {
				c.logger.Error(err, "Failed to terminate RunPod instance", "runpodID", runpodID)
				continue
			}

			// Mark for removal from tracking map
			jobsToRemove = append(jobsToRemove, jobKey)

			// Clean up virtual pod if it still exists
			parts := strings.Split(jobKey, "/")
			if len(parts) == 2 {
				namespace := parts[0]
				jobName := parts[1]
				podName := fmt.Sprintf("%s-runpod", jobName)

				err := c.clientset.CoreV1().Pods(namespace).Delete(
					context.Background(),
					podName,
					metav1.DeleteOptions{},
				)
				if err != nil {
					c.logger.Info("Virtual pod already deleted or not found", "pod", podName)
				} else {
					c.logger.Info("Deleted virtual pod", "pod", podName)
				}
			}
		}
	}

	// Remove terminated jobs from tracking
	for _, jobKey := range jobsToRemove {
		delete(c.deletedJobs, jobKey)
		c.logger.Info("Cleanup complete, removed job from tracking", "job", jobKey)
	}

	c.logger.Info("RunPod cleanup completed", "processed", len(jobsToRemove))
	return nil
}

// terminateRunPodInstance terminates a RunPod instance by ID
func (c *JobController) terminateRunPodInstance(podID string) error {
	url := fmt.Sprintf("https://api.runpod.io/graphql?api_key=%s", c.runpodKey)

	query := fmt.Sprintf(`
		mutation {
			podTerminate(input: {
				podId: "%s"
			}) {
				id
				desiredStatus
			}
		}
	`, podID)

	reqBody, err := json.Marshal(map[string]string{
		"query": query,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("RunPod API returned error: %d %s", resp.StatusCode, string(body))
	}

	// Parse response to confirm termination
	var response struct {
		Data struct {
			PodTerminate struct {
				ID            string `json:"id"`
				DesiredStatus string `json:"desiredStatus"`
			} `json:"podTerminate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}

	// Check for API errors
	if len(response.Errors) > 0 {
		return fmt.Errorf("RunPod API error: %s", response.Errors[0].Message)
	}

	// Check termination status
	if response.Data.PodTerminate.DesiredStatus != "TERMINATED" {
		return fmt.Errorf("failed to terminate RunPod instance, status: %s",
			response.Data.PodTerminate.DesiredStatus)
	}

	return nil
}

// Helper function to create boolean pointer
func boolPtr(b bool) *bool {
	return &b
}

// isReady checks if the controller is ready to serve requests
func (c *JobController) isReady() bool {
	return c.runpodAvailable && c.runpodKey != ""
}

// checkRunPodHealth checks if the RunPod API is healthy
func (c *JobController) checkRunPodHealth() {
	// Skip check if no API key
	if c.runpodKey == "" {
		c.runpodAvailable = false
		return
	}

	url := fmt.Sprintf("https://api.runpod.io/graphql?api_key=%s", c.runpodKey)
	query := `
		query {
			myself {
				id
			}
		}
	`

	reqBody, err := json.Marshal(map[string]string{
		"query": query,
	})
	if err != nil {
		c.logger.Error(err, "failed to marshal RunPod health check query")
		c.runpodAvailable = false
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		c.logger.Error(err, "failed to create RunPod health check request")
		c.runpodAvailable = false
		return
	}

	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error(err, "RunPod API health check failed")
		c.runpodAvailable = false
		return
	}
	defer resp.Body.Close()

	// Update health status based on response
	wasAvailable := c.runpodAvailable
	c.runpodAvailable = resp.StatusCode >= 200 && resp.StatusCode < 300

	// Log changes in availability
	if wasAvailable && !c.runpodAvailable {
		c.logger.Info("RunPod API is now unavailable", "statusCode", resp.StatusCode)
	} else if !wasAvailable && c.runpodAvailable {
		c.logger.Info("RunPod API is now available")
	}
}

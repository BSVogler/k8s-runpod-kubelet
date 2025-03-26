package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsvogler/k8s-runpod-controller/pkg/config"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Constants for RunPod integration
const (
	RunpodManagedAnnotation               = "runpod.io/managed"
	RunpodOffloadAnnotation               = "runpod.io/offload"
	RunpodOffloadedAnnotation             = "runpod.io/offloaded"
	RunpodPodIDAnnotation                 = "runpod.io/pod-id"
	RunpodCostAnnotation                  = "runpod.io/cost-per-hr"
	RunpodManagedLabel                    = "runpod.io/managed"
	RunpodRetryAnnotation                 = "runpod.io/retry-after"
	RunpodCloudTypeAnnotation             = "runpod.io/cloud-type"
	RunpodTemplateIdAnnotation            = "runpod.io/templateId"
	GpuMemoryAnnotation                   = "runpod.io/required-gpu-memory"
	RunpodContainerRegistryAuthAnnotation = "runpod.io/container-registry-auth-id"

	// DefaultMaxPrice for GPU
	DefaultMaxPrice = 0.5

	// DefaultAPITimeout API and timeout defaults
	DefaultAPITimeout = 30 * time.Second
	DefaultRetryCount = 5
	DefaultRetryDelay = 100 * time.Millisecond
)

// PodStatus represents the status of a RunPod instance
type PodStatus string

const (
	PodRunning     PodStatus = "RUNNING"
	PodStarting    PodStatus = "STARTING"
	PodTerminated  PodStatus = "TERMINATED"
	PodTerminating PodStatus = "TERMINATING"
	PodNotFound    PodStatus = "NOT_FOUND"
)

// RunPodEnv represents the environment variable structure for RunPod API
type RunPodEnv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// GPUType represents the GPU type from RunPod API
type GPUType struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"displayName"`
	MemoryInGb     int     `json:"memoryInGb"`
	SecureCloud    bool    `json:"secureCloud"`
	SecurePrice    float64 `json:"securePrice"`
	CommunityCloud bool    `json:"communityCloud"`
	CommunityPrice float64 `json:"communityPrice"`

	// These fields are not from the API but are used internally
	IsSecure bool
	Price    float64
}

// RunPodClient handles all interactions with the RunPod API
type RunPodClient struct {
	httpClient     *http.Client
	apiKey         string
	baseGraphqlURL string
	baseRESTURL    string
	logger         *slog.Logger
}

// NewRunPodClient creates a new RunPod API client
func NewRunPodClient(logger *slog.Logger) *RunPodClient {
	apiKey := os.Getenv("RUNPOD_KEY")
	if apiKey == "" {
		logger.Error("RUNPOD_KEY environment variable is not set")
	}

	return &RunPodClient{
		httpClient:     &http.Client{Timeout: DefaultAPITimeout},
		apiKey:         apiKey,
		baseGraphqlURL: "https://api.runpod.io/graphql",
		baseRESTURL:    "https://rest.runpod.io/v1/",
		logger:         logger,
	}
}

// ExecuteGraphQL executes a GraphQL query with proper error handling
func (c *RunPodClient) ExecuteGraphQL(query string, variables map[string]interface{}, response interface{}) error {
	reqBody, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseGraphqlURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(response)
}

// GetPodStatus checks the status of a RunPod instance
func (c *RunPodClient) GetPodStatus(podID string) (PodStatus, error) {
	//in tests this did not work
	query := `
        query pod($input: PodFilter!) {
            pod(input: $input) {
                id
                desiredStatus
                currentStatus
            }
        }
    `

	variables := map[string]interface{}{
		"input": map[string]string{
			"podId": podID,
		},
	}

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

	err := c.ExecuteGraphQL(query, variables, &response)

	// Add retries for temporary errors
	retryCount := 0
	maxRetries := 3
	for retryCount < maxRetries && err != nil && strings.Contains(err.Error(), "GRAPHQL_VALIDATION_FAILED") {
		retryCount++
		c.logger.Info("Retrying pod status check after validation error",
			"podID", podID,
			"retry", retryCount)
		time.Sleep(time.Duration(retryCount) * 500 * time.Millisecond)
		err = c.ExecuteGraphQL(query, variables, &response)
	}

	if err != nil {
		return PodNotFound, err
	}

	if len(response.Errors) > 0 {
		if strings.Contains(strings.ToLower(response.Errors[0].Message), "not found") {
			return PodNotFound, nil
		}
		return PodNotFound, fmt.Errorf("RunPod API error: %s", response.Errors[0].Message)
	}

	if response.Data.Pod.ID == "" {
		return PodNotFound, nil
	}

	if response.Data.Pod.CurrentStatus != "" {
		return PodStatus(response.Data.Pod.CurrentStatus), nil
	}

	return PodStatus(response.Data.Pod.DesiredStatus), nil
}

func (c *RunPodClient) GetPodStatusREST(podID string) (PodStatus, error) {
	endpoint := fmt.Sprintf("pods/%s", podID)

	// Add retries for temporary errors
	var resp *http.Response
	retryCount := 0
	maxRetries := 3
	var lastError error
	var lastStatusCode int
	var lastResponseBody string

	for retryCount < maxRetries {
		var err error
		resp, err = c.makeRESTRequest("GET", endpoint, nil)

		if err == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound) {
			break
		}

		// Store the error details for better reporting
		lastError = err
		if resp != nil {
			lastStatusCode = resp.StatusCode
			// Try to read the response body for error details
			if resp.Body != nil {
				bodyBytes, readErr := io.ReadAll(resp.Body)
				if readErr == nil {
					lastResponseBody = string(bodyBytes)
				}
				c.logger.Error("Failed getting pod status", "error", lastResponseBody)
				bodyErr := resp.Body.Close()
				if bodyErr != nil {
					c.logger.Warn("Error closing response body", "error", bodyErr)
				}
			}
		}

		retryCount++

		c.logger.Info("Retrying pod status check after error",
			"podID", podID,
			"retry", retryCount,
			"error", err,
			"statusCode", lastStatusCode)

		time.Sleep(time.Duration(retryCount) * 500 * time.Millisecond)
	}

	if resp == nil {
		errorMsg := fmt.Sprintf("API request failed after %d attempts", maxRetries)
		if lastError != nil {
			errorMsg = fmt.Sprintf("%s, last error: %v", errorMsg, lastError)
		}
		if lastStatusCode > 0 {
			errorMsg = fmt.Sprintf("%s, last status code: %d", errorMsg, lastStatusCode)
		}
		if lastResponseBody != "" {
			// Truncate very long response bodies
			if len(lastResponseBody) > 200 {
				lastResponseBody = lastResponseBody[:200] + "..."
			}
			errorMsg = fmt.Sprintf("%s, last response: %s", errorMsg, lastResponseBody)
		}
		return PodNotFound, fmt.Errorf(errorMsg)
	}

	defer func() {
		bodyErr := resp.Body.Close()
		if bodyErr != nil {
			c.logger.Warn("Error closing response body", "error", bodyErr)
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return PodNotFound, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return PodNotFound, fmt.Errorf("RunPod API error with status %d and error reading body: %w",
				resp.StatusCode, readErr)
		}
		return PodNotFound, fmt.Errorf("RunPod API error: status %d, body: %s",
			resp.StatusCode, string(body))
	}

	var response struct {
		ID            string `json:"id"`
		DesiredStatus string `json:"desiredStatus"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return PodNotFound, fmt.Errorf("error decoding response: %w", err)
	}

	if response.ID == "" {
		return PodNotFound, nil
	}

	return PodStatus(response.DesiredStatus), nil
}

// GetGPUTypes gets available GPU types from RunPod API
// Update GetGPUTypes to return the selected GPU details and formatted GPU type list
func (c *RunPodClient) GetGPUTypes(minRAMPerGPU int, maxPrice float64, cloudType string) ([]string, error) {
	//https://graphql-spec.runpod.io/#query-gpuTypes
	query := `
        query GpuTypes {
            gpuTypes {
                id
                displayName
                memoryInGb
                secureCloud
                securePrice
                communityCloud
                communityPrice
            }
        }
    `

	var response struct {
		Data struct {
			GPUTypes []GPUType `json:"gpuTypes"`
		} `json:"data"`
	}

	if err := c.ExecuteGraphQL(query, nil, &response); err != nil {
		return []string{}, err
	}

	// Filter GPUs based on criteria AND cloud type
	var filteredGPUs []struct {
		ID          string
		DisplayName string
		MemoryInGb  int
		Price       float64
	}

	for _, gpu := range response.Data.GPUTypes {
		var price float64
		var cloudCheck bool

		if cloudType == "SECURE" {
			price = gpu.SecurePrice
			cloudCheck = gpu.SecureCloud
		} else if cloudType == "COMMUNITY" {
			price = gpu.CommunityPrice
			cloudCheck = gpu.CommunityCloud
		}

		// If the cloud condition is met and the GPU meets the price and memory requirements
		if cloudCheck && price > 0 && price < maxPrice && gpu.MemoryInGb >= minRAMPerGPU {
			filteredGPUs = append(filteredGPUs, struct {
				ID          string
				DisplayName string
				MemoryInGb  int
				Price       float64
			}{
				ID:          gpu.ID,
				DisplayName: gpu.DisplayName,
				MemoryInGb:  gpu.MemoryInGb,
				Price:       price,
			})
			c.logger.Debug("Found eligible "+cloudType+" GPU type",
				"id", gpu.ID,
				"displayName", gpu.DisplayName,
				"price", price)
		}
	}

	// Sort by price ascending
	sort.Slice(filteredGPUs, func(i, j int) bool {
		return filteredGPUs[i].Price < filteredGPUs[j].Price
	})

	// Take up to 5 GPUs
	var gpuIDs []string
	for i, gpu := range filteredGPUs {
		if i >= 5 {
			break
		}
		gpuIDs = append(gpuIDs, gpu.ID) // No formatting with quotes here
	}

	if len(gpuIDs) == 0 {
		c.logger.Info("No eligible GPU types found",
			"cloudType", cloudType,
			"minRAMPerGPU", minRAMPerGPU,
			"maxPrice", maxPrice)
		return []string{}, nil
	}

	return gpuIDs, nil
}

func (c *RunPodClient) DeployPodREST(params map[string]interface{}) (string, float64, error) {
	//https://rest.runpod.io/v1/docs#tag/pods/POST/pods
	reqBody, err := json.Marshal(params)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.makeRESTRequest("POST", "pods", bytes.NewBuffer(reqBody))
	if err != nil {
		c.logger.Error("REST API request failed when deploying pod", "err", err)
		return "", 0, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.logger.Error("RunPod REST API returned error",
			"statusCode", resp.StatusCode,
			"response", string(body))
		return "", 0, fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		ID        string  `json:"id"`
		CostPerHr float64 `json:"costPerHr"`
		MachineID string  `json:"machineId"`
		Name      string  `json:"name"`
		Machine   struct {
			DataCenterID string `json:"dataCenterId"`
			GpuTypeID    string `json:"gpuTypeId"`
			Location     string `json:"location"`
			SecureCloud  bool   `json:"secureCloud"`
		} `json:"machine"`
		DesiredStatus string `json:"desiredStatus"`
		ImageName     string `json:"imageName"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.ID == "" {
		return "", 0, fmt.Errorf("pod deployment failed: %s", string(body))
	}

	c.logger.Info("Pod deployed successfully",
		"podId", response.ID,
		"costPerHr", response.CostPerHr,
		"machineId", response.MachineID,
		"gpuType", response.Machine.GpuTypeID,
		"location", response.Machine.Location,
		"dataCenter", response.Machine.DataCenterID)

	return response.ID, response.CostPerHr, nil
}

// DeployPod deploys a pod to RunPod
func (c *RunPodClient) DeployPod(params map[string]interface{}) (string, float64, error) {
	//https://graphql-spec.runpod.io/#definition-PodFindAndDeployOnDemandInput
	query := `
        mutation podFindAndDeployOnDemand($input: PodFindAndDeployOnDemandInput!) {
            podFindAndDeployOnDemand(input: $input) {
                id
                costPerHr
                machineId
                imageName
                machine {
                    podHostId
                }
            }
        }
    `

	variables := map[string]interface{}{
		"input": params,
	}

	var response struct {
		Data struct {
			PodFindAndDeployOnDemand struct {
				ID        string  `json:"id"`
				CostPerHr float64 `json:"costPerHr"`
				MachineID string  `json:"machineId"`
				ImageName string  `json:"imageName"`
				Machine   struct {
					PodHostID string `json:"podHostId"`
				} `json:"machine"`
			} `json:"podFindAndDeployOnDemand"`
		} `json:"data"`
		Errors []struct {
			Message    string                 `json:"message"`
			Path       []string               `json:"path"`
			Extensions map[string]interface{} `json:"extensions"`
		} `json:"errors"`
	}

	if err := c.ExecuteGraphQL(query, variables, &response); err != nil {
		c.logger.Error("API request failed when deploying pod", "err", err)
		return "", 0, err
	}

	// Check for API errors
	if len(response.Errors) > 0 {
		errorDetails := map[string]interface{}{
			"message": response.Errors[0].Message,
		}

		if len(response.Errors[0].Path) > 0 {
			errorDetails["path"] = strings.Join(response.Errors[0].Path, ".")
		}

		for k, v := range response.Errors[0].Extensions {
			errorDetails[k] = v
		}

		errorDetailsJSON, _ := json.Marshal(errorDetails)
		c.logger.Error("RunPod API returned error",
			"errorDetails", string(errorDetailsJSON))

		return "", 0, fmt.Errorf("RunPod API error: %s", response.Errors[0].Message)
	}

	// Validate response
	if response.Data.PodFindAndDeployOnDemand.ID == "" || response.Data.PodFindAndDeployOnDemand.CostPerHr <= 0 {
		return "", 0, fmt.Errorf("RunPod deployment failed: empty pod ID or zero cost")
	}

	return response.Data.PodFindAndDeployOnDemand.ID, response.Data.PodFindAndDeployOnDemand.CostPerHr, nil
}

// TerminatePod terminates a RunPod instance by ID
func (c *RunPodClient) TerminatePod(podID string) error {
	query := `
		mutation podTerminate($input: PodTerminateInput!) {
			podTerminate(input: $input) {
				id
				desiredStatus
			}
		}
	`

	variables := map[string]interface{}{
		"input": map[string]string{
			"podId": podID,
		},
	}

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

	if err := c.ExecuteGraphQL(query, variables, &response); err != nil {
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

// makeRESTRequest is a helper function to make REST API requests to RunPod
func (c *RunPodClient) makeRESTRequest(method, endpoint string, body io.Reader) (*http.Response, error) {
	url := fmt.Sprintf("%s%s", c.baseRESTURL, endpoint)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	ctx, cancel := context.WithTimeout(context.Background(), DefaultAPITimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	// Check for 404 Not Found and return a standardized error
	if resp.StatusCode == http.StatusNotFound {
		// Read and close the body to prevent resource leaks
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("not found: %s", string(body))
	}

	return resp, nil
}

// LoadRunning loads from the runpod api the running pods and if missing adds them to the list of virtual pods in the cluster
func (c *JobController) LoadRunning() {
	// Skip check if no API key
	if c.runpodClient.apiKey == "" {
		c.runpodAvailable = false
		c.logger.Warn("RunPod API key not set, skipping LoadRunning")
		return
	}

	// Make a request to the RunPod API to get all running pods
	resp, err := c.runpodClient.makeRESTRequest("GET", "pods?desiredStatus=RUNNING", nil)
	if err != nil {
		c.logger.Error("Failed to get running pods from RunPod API", "err", err)
		c.runpodAvailable = false
		return
	}

	// Ensure we always close the response body
	if resp != nil && resp.Body != nil {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				c.logger.Error("Failed to close response body", "err", err)
			}
		}()
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			c.logger.Error("Failed to read error response body",
				"statusCode", resp.StatusCode,
				"readErr", readErr)
		} else {
			c.logger.Error("RunPod API returned error",
				"statusCode", resp.StatusCode,
				"response", string(body))
		}
		c.runpodAvailable = false
		return
	}

	// API is available
	c.runpodAvailable = true

	// Parse the response - the API returns an array directly, not a struct with a pods field
	var pods []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		CostPerHr float64 `json:"costPerHr"`
		ImageName string  `json:"imageName"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&pods); err != nil {
		c.logger.Error("Failed to decode RunPod API response", "err", err)
		return
	}

	// Get existing pods in the cluster
	existingPods, err := c.clientset.CoreV1().Pods("").List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: "runpod.io/managed=true",
		},
	)
	if err != nil {
		c.logger.Error("Failed to list existing pods", "err", err)
		return
	}

	// Create a map of existing RunPod IDs
	existingRunPodIDs := make(map[string]bool)
	for _, pod := range existingPods.Items {
		if podID, exists := pod.Annotations[RunpodPodIDAnnotation]; exists {
			existingRunPodIDs[podID] = true
		}
	}

	// Process each running pod from RunPod API
	for _, runpodInstance := range pods {
		// Skip if this RunPod instance is already represented in the cluster
		if existingRunPodIDs[runpodInstance.ID] {
			continue
		}

		// Use the name directly without splitting
		// The name should be the job name, and we'll need to determine the namespace
		// For now, use the default namespace
		namespace := "default"
		jobName := runpodInstance.Name

		// Skip jobs with invalid names
		if len(jobName) == 0 {
			c.logger.Info("Skipping RunPod instance with empty name",
				"podID", runpodInstance.ID)
			continue
		}

		c.logger.Info("Processing RunPod instance",
			"podID", runpodInstance.ID,
			"name", runpodInstance.Name,
			"namespace", namespace,
			"jobName", jobName)

		// Check if the job exists
		job, err := c.clientset.BatchV1().Jobs(namespace).Get(
			context.Background(),
			jobName,
			metav1.GetOptions{},
		)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				c.logger.Info("Job not found for RunPod instance, skipping",
					"podID", runpodInstance.ID,
					"namespace", namespace,
					"jobName", jobName)
				continue
			}
			c.logger.Error("Failed to get job for RunPod instance",
				"podID", runpodInstance.ID,
				"namespace", namespace,
				"jobName", jobName,
				"err", err)
			continue
		}

		// Create a virtual pod for this RunPod instance
		c.logger.Info("Creating virtual pod for existing RunPod instance",
			"podID", runpodInstance.ID,
			"namespace", namespace,
			"jobName", jobName)

		// Update job annotations if needed
		jobCopy := job.DeepCopy()
		if jobCopy.Annotations == nil {
			jobCopy.Annotations = make(map[string]string)
		}

		// Only update if not already set
		updateJob := false
		if jobCopy.Annotations[RunpodPodIDAnnotation] != runpodInstance.ID {
			jobCopy.Annotations[RunpodPodIDAnnotation] = runpodInstance.ID
			updateJob = true
		}
		if jobCopy.Annotations[RunpodOffloadedAnnotation] != "true" {
			jobCopy.Annotations[RunpodOffloadedAnnotation] = "true"
			updateJob = true
		}
		if jobCopy.Annotations[RunpodCostAnnotation] != fmt.Sprintf("%f", runpodInstance.CostPerHr) {
			jobCopy.Annotations[RunpodCostAnnotation] = fmt.Sprintf("%f", runpodInstance.CostPerHr)
			updateJob = true
		}

		if updateJob {
			if err := c.UpdateJobWithRetry(jobCopy); err != nil {
				c.logger.Error("Failed to update job annotations for RunPod instance",
					"podID", runpodInstance.ID,
					"namespace", namespace,
					"jobName", jobName,
					"err", err)
				continue
			}
		}

		// Create the virtual pod
		if err := c.CreateVirtualPod(*job, runpodInstance.ID, runpodInstance.CostPerHr); err != nil {
			c.logger.Error("Failed to create virtual pod for RunPod instance",
				"podID", runpodInstance.ID,
				"namespace", namespace,
				"jobName", jobName,
				"err", err)
			continue
		}

		c.logger.Info("Successfully created virtual pod for RunPod instance",
			"podID", runpodInstance.ID,
			"namespace", namespace,
			"jobName", jobName)
	}
}

// JobController manages Kubernetes jobs and offloads them to RunPod when necessary
type JobController struct {
	clientset        *kubernetes.Clientset
	logger           *slog.Logger
	config           config.Config
	runpodClient     *RunPodClient
	maxPrice         float64
	deletedJobs      map[string]string // Maps job name to runpod ID for cleanup
	deletedJobsMutex sync.Mutex
	runpodAvailable  bool // Tracks if RunPod API is available
	healthServer     *HealthServer
}

// NewJobController creates a new JobController instance
func NewJobController(clientset *kubernetes.Clientset, logger *slog.Logger, cfg config.Config) *JobController {

	maxPrice := DefaultMaxPrice
	if cfg.MaxGPUPrice > 0 {
		maxPrice = cfg.MaxGPUPrice
	}

	runpodClient := NewRunPodClient(logger)

	controller := &JobController{
		clientset:       clientset,
		logger:          logger,
		config:          cfg,
		runpodClient:    runpodClient,
		maxPrice:        maxPrice,
		deletedJobs:     make(map[string]string),
		runpodAvailable: true, // Initially assume RunPod is available
	}

	// Create health server
	controller.healthServer = NewHealthServer(cfg.HealthServerAddress, controller.isReady)

	return controller
}

// UpdateJobWithRetry updates a job with retry logic to handle concurrent modifications
func (c *JobController) UpdateJobWithRetry(job *batchv1.Job) error {
	retryCount := 0
	maxRetries := DefaultRetryCount

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
		time.Sleep(DefaultRetryDelay)

		c.logger.Info("Retrying job update after conflict",
			"job", job.Name,
			"namespace", job.Namespace,
			"retry", retryCount)
	}

	return fmt.Errorf("failed to update job after %d retries: %s/%s", maxRetries, job.Namespace, job.Name)
}

// ForceDeletePod forcefully removes a pod from the Kubernetes API
func (c *JobController) ForceDeletePod(namespace, name string) error {
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

// IsRunPodJob checks if a job should be managed by RunPod
func IsRunPodJob(job batchv1.Job) bool {
	_, hasRunPodAnnotation := job.Annotations[RunpodManagedAnnotation]
	return hasRunPodAnnotation
}

// HasRunPodLabel checks if a job has the RunPod managed label
func HasRunPodLabel(job batchv1.Job) bool {
	_, hasLabel := job.Labels[RunpodManagedLabel]
	return hasLabel
}

// IsPending checks if a job is in a pending state
func IsPending(job batchv1.Job, clientset *kubernetes.Clientset) (bool, error) {
	// Check if the job is still actively running but hasn't completed yet
	if job.Status.Active > 0 && job.Status.Succeeded == 0 &&
		job.Status.Failed < *job.Spec.BackoffLimit && job.Status.CompletionTime == nil {

		// Check if the active pod is actually pending
		pods, err := clientset.CoreV1().Pods(job.Namespace).List(
			context.Background(),
			metav1.ListOptions{
				LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
			},
		)

		if err != nil {
			return false, fmt.Errorf("failed to list pods for job: %w", err)
		}

		// Count actual pending pods
		pendingPods := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodPending {
				// Check if the pod is unschedulable
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
						if condition.Reason == "Unschedulable" {
							// This is an unschedulable pod - means the job is truly pending
							return true, nil
						}
					}
				}
				pendingPods++
			}
		}

		// If all active pods are pending, the job is truly pending
		if pendingPods == int(job.Status.Active) {
			return true, nil
		}

		// Otherwise, the job has at least one non-pending pod
		return false, nil
	}

	return false, nil
}

// ShouldOffloadToRunPod determines if a job should be offloaded to RunPod
func ShouldOffloadToRunPod(job batchv1.Job, pendingCounter int, cfg config.Config) bool {
	// Already offloaded
	if _, hasOffloaded := job.Annotations[RunpodOffloadedAnnotation]; hasOffloaded {
		return false
	}

	// Check for explicit offload annotation
	value, exists := job.Annotations[RunpodOffloadAnnotation]
	if exists && value == "true" {
		return true
	}

	// Check if job is completing or already completed
	if job.Status.Succeeded > 0 || job.Status.CompletionTime != nil {
		return false
	}

	// Check if job has been pending for too long
	creationTime := job.CreationTimestamp.Time
	pendingTime := time.Since(creationTime)

	// Time-based decision
	timeBasedOffload := pendingTime > time.Duration(cfg.MaxPendingTime)*time.Second
	// Count-based decision
	countBasedOffload := pendingCounter > cfg.PendingJobThreshold

	return timeBasedOffload || countBasedOffload
}

// LabelAsRunPodJob adds the RunPod managed label to a job
func (c *JobController) LabelAsRunPodJob(job *batchv1.Job) error {
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

// HasRunningOrScheduledPods checks if a job already has non-pending pods
func (c *JobController) HasRunningOrScheduledPods(job batchv1.Job) (bool, error) {
	// List pods associated with this job
	pods, err := c.clientset.CoreV1().Pods(job.Namespace).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
		},
	)
	if err != nil {
		return false, err
	}

	for _, pod := range pods.Items {
		// Skip pods that are managed by RunPod
		if _, isRunPodManaged := pod.Labels[RunpodManagedLabel]; isRunPodManaged {
			continue
		}

		// Check if any pod is running or about to run
		if pod.Status.Phase == corev1.PodRunning ||
			pod.Status.Phase == corev1.PodSucceeded ||
			pod.Spec.NodeName != "" { // Pod is assigned to a node
			return true, nil
		}
	}

	return false, nil
}

// ExtractEnvVars extracts environment variables from the Kubernetes job
func (c *JobController) ExtractEnvVars(job batchv1.Job) ([]RunPodEnv, error) {
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
				c.logger.Error("failed to get secret",
					"namespace", job.Namespace,
					"secret", volume.Secret.SecretName, "err", err)
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

// FormatEnvVarsForGraphQL formats environment variables for GraphQL query
func FormatEnvVarsForGraphQL(envVars []RunPodEnv) []map[string]string {
	formattedEnvVars := make([]map[string]string, 0, len(envVars))
	for _, env := range envVars {
		formattedEnvVars = append(formattedEnvVars, map[string]string{
			"key":   env.Key,
			"value": env.Value,
		})
	}
	return formattedEnvVars
}

func FormatEnvVarsForREST(envVars []RunPodEnv) map[string]string {
	envMap := make(map[string]string)
	for _, env := range envVars {
		envMap[env.Key] = env.Value
	}
	return envMap
}

// PrepareRunPodParameters prepares parameters for RunPod deployment
func (c *JobController) PrepareRunPodParameters(job batchv1.Job, graphql bool) (map[string]interface{}, error) {
	// Determine cloud type - default to COMMUNITY but allow override via annotation
	cloudType := "SECURE"
	if cloudTypeVal, exists := job.Annotations[RunpodCloudTypeAnnotation]; exists {
		// Validate and normalize the cloud type value
		cloudTypeUpperCase := strings.ToUpper(cloudTypeVal)
		if cloudTypeUpperCase == "SECURE" || cloudTypeUpperCase == "COMMUNITY" {
			cloudType = cloudTypeUpperCase
		} else {
			c.logger.Warn("Invalid cloud type specified, using default",
				"job", job.Name,
				"namespace", job.Namespace,
				"specifiedValue", cloudTypeVal,
				"defaultValue", cloudType)
		}
	}

	// Extract container registry auth ID if provided
	containerRegistryAuthId := ""
	if authID, exists := job.Annotations[RunpodContainerRegistryAuthAnnotation]; exists && authID != "" {
		containerRegistryAuthId = authID
	}

	// Extract template ID if provided
	templateId := ""
	if tplID, exists := job.Annotations[RunpodTemplateIdAnnotation]; exists && tplID != "" {
		templateId = tplID
	}

	// Determine minimum GPU memory required
	minRAMPerGPU := 16 // Default minimum memory
	if memStr, exists := job.Annotations[GpuMemoryAnnotation]; exists {
		if mem, err := strconv.Atoi(memStr); err == nil {
			minRAMPerGPU = mem
		}
	}

	// Get GPU types - pass the cloud type to filter correctly
	gpuTypes, err := c.runpodClient.GetGPUTypes(minRAMPerGPU, c.maxPrice, cloudType)
	if err != nil {
		return nil, fmt.Errorf("failed to get GPU types: %w", err)
	}

	// Extract environment variables from job
	envVars, err := c.ExtractEnvVars(job)
	if err != nil {
		return nil, fmt.Errorf("failed to extract environment variables: %w", err)
	}

	var formattedEnvVars interface{}
	if graphql {
		formattedEnvVars = FormatEnvVarsForGraphQL(envVars)
	} else {
		formattedEnvVars = FormatEnvVarsForREST(envVars)
	}
	// Determine image name from job
	var imageName string
	if len(job.Spec.Template.Spec.Containers) > 0 {
		imageName = job.Spec.Template.Spec.Containers[0].Image
	} else {
		return nil, fmt.Errorf("job has no containers")
	}

	// Use the job name directly without namespace prefix
	runpodJobName := job.Name

	// Default values
	volumeInGb := 0
	containerDiskInGb := 15

	// Create deployment parameters - use the same cloudType as used for filtering https://graphql-spec.runpod.io/#definition-PodFindAndDeployOnDemandInput
	params := map[string]interface{}{
		"cloudType":         cloudType,
		"volumeInGb":        volumeInGb,
		"containerDiskInGb": containerDiskInGb,
		"minRAMPerGPU":      minRAMPerGPU,
		"gpuTypeIdList":     gpuTypes, // Use the array directly, don't stringify it
		"name":              runpodJobName,
		"imageName":         imageName,
		"templateId":        templateId,
		"env":               formattedEnvVars,
	}

	// Add templateId to params if it exists
	if templateId != "" {
		params["templateId"] = templateId
	}

	if containerRegistryAuthId != "" {
		params["containerRegistryAuthId"] = containerRegistryAuthId
	}

	return params, nil
}

// Add this function to JobController to clean up pending pods for an offloaded job
func (c *JobController) CleanupPendingPodsForJob(job batchv1.Job) error {
	// List all pods associated with this job
	pods, err := c.clientset.CoreV1().Pods(job.Namespace).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
		},
	)

	if err != nil {
		return fmt.Errorf("failed to list pods for job cleanup: %w", err)
	}

	c.logger.Info("Cleaning up pending pods for offloaded job",
		"job", job.Name,
		"namespace", job.Namespace,
		"podCount", len(pods.Items))

	for _, pod := range pods.Items {
		// Skip pods that are managed by RunPod (our virtual pod)
		if _, isRunPodManaged := pod.Labels[RunpodManagedLabel]; isRunPodManaged {
			continue
		}

		// Check if the pod is in Pending state
		if pod.Status.Phase == corev1.PodPending {
			c.logger.Info("Force deleting pending pod for offloaded job",
				"pod", pod.Name,
				"namespace", pod.Namespace)

			if err := c.ForceDeletePod(pod.Namespace, pod.Name); err != nil {
				c.logger.Error("Failed to force delete pending pod",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"err", err)
			}
		}
	}

	return nil
}

// createVirtualPodObject creates a Pod object representing a RunPod instance
func createVirtualPodObject(job batchv1.Job, runpodID string, costPerHr float64) *corev1.Pod {
	// Use consistent naming format for virtual pods
	podName := fmt.Sprintf("runpod-%s", runpodID)

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

	// Get image name from job
	var imageName string
	if len(job.Spec.Template.Spec.Containers) > 0 {
		imageName = job.Spec.Template.Spec.Containers[0].Image
	} else {
		imageName = "placeholder:latest"
	}

	// Create Pod object
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       job.Namespace,
			Labels:          podLabels,
			Annotations:     podAnnotations,
			OwnerReferences: ownerReferences,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "runpod-proxy",
					Image:   imageName,
					Command: []string{"/bin/sh", "-c", "echo 'This pod represents a RunPod instance'; sleep infinity"},
				},
			},
			RestartPolicy: "Never",
			NodeName:      "runpod-virtual-node",
			NodeSelector: map[string]string{
				"runpod.io/virtual": "true",
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
}

// CreateVirtualPod creates a virtual Pod representation of a RunPod instance
func (c *JobController) CreateVirtualPod(job batchv1.Job, runpodID string, costPerHr float64) error {
	pod := createVirtualPodObject(job, runpodID, costPerHr)

	// Create the Pod
	_, err := c.clientset.CoreV1().Pods(job.Namespace).Create(
		context.Background(),
		pod,
		metav1.CreateOptions{},
	)
	if err != nil {
		c.logger.Error("Failed to create virtual pod for RunPod instance",
			"pod", pod.Name, "runpodID", runpodID, "err", err)
		return fmt.Errorf("failed to create virtual pod: %w", err)
	}

	return nil
}

// UpdateJobAnnotations updates a job's annotations with RunPod information
func (c *JobController) UpdateJobAnnotations(job batchv1.Job, runpodID string, costPerHr float64) error {
	jobCopy := job.DeepCopy()

	if jobCopy.Annotations == nil {
		jobCopy.Annotations = make(map[string]string)
	}

	jobCopy.Annotations[RunpodPodIDAnnotation] = runpodID
	jobCopy.Annotations[RunpodOffloadedAnnotation] = "true"
	jobCopy.Annotations[RunpodCostAnnotation] = fmt.Sprintf("%f", costPerHr)

	return c.UpdateJobWithRetry(jobCopy)
}

// UpdateJobStatus updates a job's status to mark it as active
func (c *JobController) UpdateJobStatus(job batchv1.Job) error {
	jobCopy := job.DeepCopy()

	// Mark Job as "active" by creating a "Running" pod
	if jobCopy.Status.Active == 0 {
		jobCopy.Status.Active = 1
		jobCopy.Status.StartTime = &metav1.Time{Time: time.Now()}

		// Update job status in Kubernetes
		_, err := c.clientset.BatchV1().Jobs(jobCopy.Namespace).UpdateStatus(
			context.Background(),
			jobCopy,
			metav1.UpdateOptions{},
		)
		if err != nil {
			c.logger.Error("Failed to update job status",
				"job", jobCopy.Name, "namespace", jobCopy.Namespace, "err", err)
			return fmt.Errorf("failed to update job status: %w", err)
		}
	}

	return nil
}

func sanitizeParameters(params map[string]interface{}) string {
	//log params minus the env vars for security
	logParams := make(map[string]interface{})
	for key, value := range params {
		// Skip the "env" key
		if key != "env" {
			logParams[key] = value
		}
	}
	// Log the request parameters (envs dropped for any secrets and not needed for debugging of the controller)
	paramsJSON, _ := json.MarshalIndent(logParams, "", "  ")
	return string(paramsJSON)
}

// OffloadJobToRunPod sends a job to RunPod and creates a K8s representation
func (c *JobController) OffloadJobToRunPod(job batchv1.Job) error {
	// Step 1: Prepare parameters for RunPod deployment
	params, err := c.PrepareRunPodParameters(job, false)
	if err != nil {
		return fmt.Errorf("failed to prepare RunPod parameters: %w", err)
	}

	c.logger.Info("Requesting RunPod deployment",
		"job", job.Name,
		"namespace", job.Namespace,
		"parameters", sanitizeParameters(params))

	// Step 2: Deploy to RunPod
	runpodID, costPerHr, err := c.runpodClient.DeployPodREST(params)
	if err != nil {
		// Check if this is a "no instances available" error that we should retry
		if strings.Contains(err.Error(), "no instances currently available") {
			// Mark this job for retry by adding a specific annotation
			jobCopy := job.DeepCopy()
			if jobCopy.Annotations == nil {
				jobCopy.Annotations = make(map[string]string)
			}
			jobCopy.Annotations[RunpodRetryAnnotation] = time.Now().Add(5 * time.Minute).Format(time.RFC3339)

			updateErr := c.UpdateJobWithRetry(jobCopy)
			if updateErr != nil {
				c.logger.Error("Failed to mark job for retry",
					"job", job.Name, "namespace", job.Namespace, "updateErr", updateErr)
			} else {
				c.logger.Info("Marked job for retry due to no available instances",
					"job", job.Name, "namespace", job.Namespace,
					"retryAfter", jobCopy.Annotations[RunpodRetryAnnotation])
			}
		}
		return fmt.Errorf("failed to deploy to RunPod: %w", err)
	}

	c.logger.Info("Successfully deployed to RunPod",
		"job", job.Name,
		"namespace", job.Namespace,
		"runpodID", runpodID,
		"costPerHour", costPerHr)

	// Step 3: Track the job for potential cleanup
	jobKey := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
	c.deletedJobsMutex.Lock()
	c.deletedJobs[jobKey] = runpodID
	c.deletedJobsMutex.Unlock()

	// Step 4: Create K8s resources
	if err := c.UpdateJobAnnotations(job, runpodID, costPerHr); err != nil {
		// Clean up RunPod instance on failure
		c.runpodClient.TerminatePod(runpodID)
		return fmt.Errorf("failed to update job annotations: %w", err)
	}

	if err := c.CreateVirtualPod(job, runpodID, costPerHr); err != nil {
		// Clean up RunPod instance on failure
		c.runpodClient.TerminatePod(runpodID)
		return fmt.Errorf("failed to create virtual pod: %w", err)
	}

	if err := c.UpdateJobStatus(job); err != nil {
		// Note: We don't clean up here as the pod and job annotations are already created
		return fmt.Errorf("failed to update job status: %w", err)
	}

	// Step 5: Clean up original pending pods that are no longer needed
	if err := c.CleanupPendingPodsForJob(job); err != nil {
		c.logger.Error("Failed to clean up pending pods for offloaded job",
			"job", job.Name,
			"namespace", job.Namespace,
			"err", err)
		// Continue execution even if cleanup fails
	}

	c.logger.Info("Successfully offloaded job to RunPod",
		"job", job.Name,
		"namespace", job.Namespace,
		"runpodID", runpodID,
		"costPerHour", costPerHr)

	return nil
}

// CleanupPod handles termination of a RunPod instance and K8s resources
func (c *JobController) CleanupPod(namespace, jobName, runpodID string) error {
	// First terminate the RunPod instance
	if err := c.runpodClient.TerminatePod(runpodID); err != nil {
		c.logger.Error("Failed to terminate RunPod instance",
			"runpodID", runpodID,
			"job", jobName, "err", err)
		// Continue with cleanup even if termination fails
	}

	// Then clean up the K8s pod - use consistent naming format
	podName := fmt.Sprintf("runpod-%s", runpodID)
	err := c.clientset.CoreV1().Pods(namespace).Delete(
		context.Background(),
		podName,
		metav1.DeleteOptions{},
	)

	if err != nil && !k8serrors.IsNotFound(err) {
		c.logger.Error("Failed to delete virtual pod",
			"pod", podName,
			"namespace", namespace, "err", err)
		return err
	}

	c.logger.Info("Cleanup completed", "job", jobName, "runpodID", runpodID)
	return nil
}

// CleanupTerminatingPods checks for pods in Terminating state on the RunPod virtual node
// and ensures they are properly terminated both in K8s and on RunPod
func (c *JobController) CleanupTerminatingPods() error {
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
			c.handleTerminatingPod(pod)
		}
	}

	c.logger.Info("Terminating pod cleanup completed", "processed", terminatingCount)
	return nil
}

// shouldForceDeleteTerminatingPod determines if a pod should be force deleted based on its termination duration
func shouldForceDeleteTerminatingPod(pod corev1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}

	deletionTime := pod.DeletionTimestamp.Time
	terminatingDuration := time.Since(deletionTime)
	forceDeletionThreshold := 15 * time.Minute

	return terminatingDuration > forceDeletionThreshold
}

// handleTerminatingPod processes a single terminating pod
func (c *JobController) handleTerminatingPod(pod corev1.Pod) {
	deletionTime := pod.DeletionTimestamp.Time
	terminatingDuration := time.Since(deletionTime)

	c.logger.Info("Found terminating pod",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"deletionTimestamp", pod.DeletionTimestamp,
		"terminatingFor", terminatingDuration.String())

	// If pod has been terminating for more than 15 minutes, force delete it
	if shouldForceDeleteTerminatingPod(pod) {
		c.logger.Info("Pod has been terminating for too long, force deleting",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"terminatingFor", terminatingDuration.String())

		if err := c.ForceDeletePod(pod.Namespace, pod.Name); err != nil {
			c.logger.Error("Failed to force delete long-terminating pod",
				"pod", pod.Name,
				"namespace", pod.Namespace, "err", err)
		}
		return
	}

	// Get RunPod ID from pod annotations
	runpodID, exists := pod.Annotations[RunpodPodIDAnnotation]
	if !exists {
		c.logger.Info("Terminating pod missing RunPod ID annotation, will force delete",
			"pod", pod.Name,
			"namespace", pod.Namespace)

		// Force delete the pod as it has no RunPod ID
		if err := c.ForceDeletePod(pod.Namespace, pod.Name); err != nil {
			c.logger.Error("Failed to force delete pod without RunPod ID",
				"pod", pod.Name,
				"namespace", pod.Namespace, "err", err)
		}
		return
	}

	// Check if the RunPod instance is still running
	podStatus, err := c.runpodClient.GetPodStatusREST(runpodID)
	if err != nil {
		c.logger.Error("Failed to check RunPod instance status",
			"runpodID", runpodID,
			"pod", pod.Name, "err", err)

		// If we get an API error, assume the pod doesn't exist on RunPod anymore and force delete it
		// This handles both GraphQL validation errors and REST API errors
		if strings.Contains(err.Error(), "GRAPHQL_VALIDATION_FAILED") ||
			strings.Contains(err.Error(), "Something went wrong") ||
			strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "404") {
			c.logger.Info("RunPod API returned error, assuming instance no longer exists. Force deleting K8s pod",
				"runpodID", runpodID,
				"pod", pod.Name)

			if err := c.ForceDeletePod(pod.Namespace, pod.Name); err != nil {
				c.logger.Error("Failed to force delete pod after RunPod API error",
					"pod", pod.Name,
					"namespace", pod.Namespace, "err", err)
			}
		}
		return
	}

	// Handle based on RunPod instance status
	switch podStatus {
	case PodRunning, PodStarting:
		c.logger.Info("RunPod instance is still active, terminating it",
			"runpodID", runpodID,
			"pod", pod.Name,
			"status", podStatus)

		if err := c.runpodClient.TerminatePod(runpodID); err != nil {
			c.logger.Error("Failed to terminate RunPod instance",
				"runpodID", runpodID,
				"pod", pod.Name, "err", err)
			return
		}

		// Wait briefly for termination to be processed
		time.Sleep(2 * time.Second)

	case PodTerminated, PodTerminating, PodNotFound:
		// If pod is already terminated or not found, just force delete it from K8s
		c.logger.Info("RunPod instance is already terminated or not found, cleaning up K8s pod",
			"runpodID", runpodID,
			"pod", pod.Name,
			"status", podStatus)

		if err := c.ForceDeletePod(pod.Namespace, pod.Name); err != nil {
			c.logger.Error("Failed to force delete pod",
				"pod", pod.Name,
				"namespace", pod.Namespace, "err", err)
		}
	}
}

// CleanupDeletedJobs checks for jobs that have been deleted from Kubernetes
// and terminates the corresponding RunPod instances
func (c *JobController) CleanupDeletedJobs() error {
	// Create a local copy of the jobs to clean up to minimize lock contention
	jobsToCheck := make(map[string]string)

	c.deletedJobsMutex.Lock()
	for jobKey, runpodID := range c.deletedJobs {
		jobsToCheck[jobKey] = runpodID
	}
	c.deletedJobsMutex.Unlock()

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
	for jobKey, runpodID := range jobsToCheck {
		if !existingJobs[jobKey] {
			// Job no longer exists in K8s, terminate the RunPod instance
			c.logger.Info("Terminating RunPod for deleted job", "job", jobKey, "runpodID", runpodID)

			// Extract namespace and job name
			parts := strings.Split(jobKey, "/")
			if len(parts) != 2 {
				c.logger.Error("Invalid job key format", "jobKey", jobKey)
				continue
			}

			namespace := parts[0]
			jobName := parts[1]

			// Clean up the resources
			if err := c.CleanupPod(namespace, jobName, runpodID); err != nil {
				c.logger.Error("Failed to cleanup pod resources",
					"job", jobKey,
					"runpodID", runpodID, "err", err)
				continue
			}

			// Mark for removal from tracking map
			jobsToRemove = append(jobsToRemove, jobKey)
		}
	}

	// Remove terminated jobs from tracking - reacquire lock
	if len(jobsToRemove) > 0 {
		c.deletedJobsMutex.Lock()
		for _, jobKey := range jobsToRemove {
			delete(c.deletedJobs, jobKey)
			c.logger.Info("Cleanup complete, removed job from tracking", "job", jobKey)
		}
		c.deletedJobsMutex.Unlock()
	}

	c.logger.Info("RunPod cleanup completed", "processed", len(jobsToRemove))
	return nil
}

// Reconcile checks for jobs that need to be offloaded to RunPod
// getJobState determines the current state of a job
func (c *JobController) getJobState(job batchv1.Job) string {
	if job.Status.Succeeded > 0 || job.Status.CompletionTime != nil {
		return "COMPLETED"
	}

	if job.Spec.BackoffLimit != nil && job.Status.Failed >= *job.Spec.BackoffLimit {
		return "FAILED"
	}

	if _, offloaded := job.Annotations[RunpodOffloadedAnnotation]; offloaded {
		if podID := job.Annotations[RunpodPodIDAnnotation]; podID != "" {
			return "OFFLOADED"
		}
		return "OFFLOADING_INCOMPLETE" // Incomplete offload
	}

	// Use the new IsPending function that requires clientset
	isPending, err := IsPending(job, c.clientset)
	if err != nil {
		c.logger.Error("Failed to check if job is pending",
			"job", job.Name, "namespace", job.Namespace, "err", err)
		// Default to a safe state if we can't determine pending status
		return "UNKNOWN"
	}

	if isPending {
		return "PENDING"
	}

	return "NEW"
}

// resetJobOffloadState resets a job's offload state when it's inconsistent
func (c *JobController) resetJobOffloadState(job batchv1.Job) error {
	c.logger.Info("Resetting inconsistent job offload state",
		"job", job.Name,
		"namespace", job.Namespace)

	jobCopy := job.DeepCopy()
	if jobCopy.Annotations == nil {
		jobCopy.Annotations = make(map[string]string)
	}

	delete(jobCopy.Annotations, RunpodOffloadedAnnotation)
	delete(jobCopy.Annotations, RunpodPodIDAnnotation)

	return c.UpdateJobWithRetry(jobCopy)
}

// normalizeJobAnnotations converts and standardizes job annotations
func (c *JobController) normalizeJobAnnotations(job batchv1.Job) error {
	jobCopy := job.DeepCopy()
	changed := false

	// Initialize annotations if they don't exist
	if jobCopy.Annotations == nil {
		jobCopy.Annotations = make(map[string]string)
	}

	// Validate that a job that's marked as offloaded has a pod ID
	if jobCopy.Annotations[RunpodOffloadedAnnotation] == "true" {
		podID := jobCopy.Annotations[RunpodPodIDAnnotation]
		if podID == "" {
			delete(jobCopy.Annotations, RunpodOffloadedAnnotation)
			delete(jobCopy.Annotations, RunpodPodIDAnnotation)
			c.logger.Info("Removed invalid offload state",
				"job", job.Name,
				"namespace", job.Namespace)
			changed = true
		}
	}

	if changed {
		return c.UpdateJobWithRetry(jobCopy)
	}
	return nil
}

// verifyRunPodInstance checks if a RunPod instance still exists and is valid
func (c *JobController) verifyRunPodInstance(job batchv1.Job) error {
	podID := job.Annotations[RunpodPodIDAnnotation]
	if podID == "" {
		return nil // Nothing to verify
	}

	status, err := c.runpodClient.GetPodStatusREST(podID)
	if err != nil {
		c.logger.Error("Error checking RunPod instance status",
			"job", job.Name,
			"namespace", job.Namespace,
			"podID", podID,
			"error", err)

		// Reset if we're sure the pod doesn't exist or if there's an API error
		// This handles both explicit "not found" errors and other API failures
		if strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "404") ||
			strings.Contains(err.Error(), "GRAPHQL_VALIDATION_FAILED") {
			c.logger.Info("RunPod instance appears to be gone, resetting job state",
				"job", job.Name,
				"namespace", job.Namespace,
				"podID", podID,
				"error", err)
			return c.resetJobOffloadState(job)
		}

		// For other errors, log but don't reset - might be temporary API issues
		return nil
	}

	if status == PodNotFound || status == PodTerminated {
		c.logger.Info("RunPod instance not found or terminated, resetting job state",
			"job", job.Name,
			"namespace", job.Namespace,
			"podID", podID,
			"status", status)
		return c.resetJobOffloadState(job)
	}

	// Instance exists, but let's ensure there are no lingering pending pods
	if err := c.CleanupPendingPodsForJob(job); err != nil {
		c.logger.Error("Failed to clean up pending pods during verification",
			"job", job.Name,
			"namespace", job.Namespace,
			"err", err)
		// Continue execution even if cleanup fails
	}

	c.logger.Debug("Verified RunPod instance",
		"job", job.Name,
		"namespace", job.Namespace,
		"podID", podID,
		"status", status)
	return nil
}

// Reconcile checks for jobs that need to be offloaded to RunPod
func (c *JobController) Reconcile() error {
	// List all jobs with the runpod.io/managed annotation
	jobs, err := c.clientset.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	pendingCount := 0
	activeJobs := make(map[string]bool)

	for _, job := range jobs.Items {
		// Skip jobs that don't have the runpod.io/managed annotation
		if !IsRunPodJob(job) {
			continue
		}

		// Track all active job names
		jobKey := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
		activeJobs[jobKey] = true

		// First, normalize annotations to ensure consistent format
		if err := c.normalizeJobAnnotations(job); err != nil {
			c.logger.Error("Failed to normalize job annotations",
				"job", job.Name,
				"namespace", job.Namespace, "err", err)
			continue
		}

		// Check if job needs labeling
		if !HasRunPodLabel(job) {
			if err := c.LabelAsRunPodJob(&job); err != nil {
				c.logger.Error("Failed to label RunPod job", "job", job.Name, "err", err)
				continue
			}
		}

		// Determine job state and handle accordingly
		jobState := c.getJobState(job)

		switch jobState {
		case "OFFLOADING_INCOMPLETE":
			// Job is in an inconsistent state, reset it
			if err := c.resetJobOffloadState(job); err != nil {
				c.logger.Error("Failed to reset job in inconsistent state",
					"job", job.Name,
					"namespace", job.Namespace, "err", err)
			}
			continue

		case "OFFLOADED":
			// Verify that the RunPod instance still exists
			if err := c.verifyRunPodInstance(job); err != nil {
				c.logger.Error("Failed to verify RunPod instance",
					"job", job.Name,
					"namespace", job.Namespace, "err", err)
			}
			continue

		case "COMPLETED", "FAILED":
			// No action needed for completed or failed jobs
			continue
		}

		// Process pending jobs for potential offloading
		isPending, err := IsPending(job, c.clientset)
		if err != nil {
			c.logger.Error("Failed to check if job is pending",
				"job", job.Name, "namespace", job.Namespace, "err", err)
			continue
		}

		if isPending {
			pendingCount++

			// Check if the job already has pods running or scheduled on cluster nodes
			hasNonPendingPods, err := c.HasRunningOrScheduledPods(job)
			if err != nil {
				c.logger.Error("Failed to check if job has running pods", "job", job.Name, "err", err)
				continue
			}

			c.logger.Info("Pending job details",
				"job", job.Name,
				"namespace", job.Namespace,
				"hasNonPendingPods", hasNonPendingPods,
				"activeCount", job.Status.Active,
				"succeededCount", job.Status.Succeeded,
				"failedCount", job.Status.Failed)

			if !hasNonPendingPods && ShouldOffloadToRunPod(job, pendingCount, c.config) {
				c.logger.Info("Attempting to offload job to RunPod",
					"job", job.Name,
					"namespace", job.Namespace)

				if err := c.OffloadJobToRunPod(job); err != nil {
					c.logger.Error("Failed to offload job to RunPod",
						"job", job.Name,
						"namespace", job.Namespace, "err", err)
				}
			}
		}
	}

	// Update tracking map for deleted jobs
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
				if err := c.Reconcile(); err != nil {
					c.logger.Error("reconciliation failed", "err", err)
				}
			case <-cleanupTicker.C:
				if err := c.CleanupDeletedJobs(); err != nil {
					c.logger.Error("cleanup failed", "err", err)
				}
			case <-terminatingPodTicker.C:
				if err := c.CleanupTerminatingPods(); err != nil {
					c.logger.Error("terminating pod cleanup failed", "err", err)
				}
			case <-healthCheckTicker.C:
				c.LoadRunning()
			case <-stopCh:
				return
			}
		}
	}()

	<-stopCh
	c.logger.Info("Stopping controller")

	// Stop health server
	if err := c.healthServer.Stop(); err != nil {
		c.logger.Error("failed to stop health server", "err", err)
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

// isReady checks if the controller is ready to serve requests
func (c *JobController) isReady() bool {
	return c.runpodAvailable && c.runpodClient.apiKey != ""
}

// Helper function to create boolean pointer
func boolPtr(b bool) *bool {
	return &b
}

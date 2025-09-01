package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client handles all interactions with the RunPod API
type Client struct {
	httpClient     *http.Client
	apiKey         string
	baseGraphqlURL string
	baseRESTURL    string
	logger         *slog.Logger
	clientset      kubernetes.Interface // Add clientset field
}

// Constants for RunPod integration
const (
	PodIDAnnotation = "runpod.io/pod-id"
	CostAnnotation  = "runpod.io/cost-per-hr"
	//CloudTypeAnnotation should in THEORY support COMMUNITY and STANDARD but in tests only STANDARD lead to results when using the API. Therefore, defaults to STANDARD.
	CloudTypeAnnotation             = "runpod.io/cloud-type"
	TemplateIdAnnotation            = "runpod.io/templateId"
	GpuMemoryAnnotation             = "runpod.io/required-gpu-memory"
	ContainerRegistryAuthAnnotation = "runpod.io/container-registry-auth-id"
	DatacenterAnnotation            = "runpod.io/datacenter-ids" // Comma-separated list preferred
	PortsAnnotation                 = "runpod.io/ports"          // Manual override for port specifications
	InterruptibleAnnotation         = "runpod.io/interruptible"  // Manual override for interruptible setting
	// DefaultMaxPrice for GPU
	DefaultMaxPrice = 0.5

	// DefaultAPITimeout API and timeout defaults
	DefaultAPITimeout = 30 * time.Second
)

// PodStatus represents the status of a RunPod instance
type PodStatus string

const (
	PodRunning     PodStatus = "RUNNING" //used by runpod
	PodStarting    PodStatus = "STARTING"
	PodTerminating PodStatus = "TERMINATING"
	PodTerminated  PodStatus = "TERMINATED" //used by runpod
	PodNotFound    PodStatus = "NOT_FOUND"  //not used by runpod according to docs
	PodExited      PodStatus = "EXITED"     //used by runpod
)

// RunPodEnv represents the environment variable structure for RunPod API
type RunPodEnv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RunPodInstance represents a pod instance from the RunPod API
type RunPodInstance struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	CostPerHr     float64 `json:"costPerHr"`
	ImageName     string  `json:"imageName"`
	CurrentStatus string  `json:"currentStatus"`
	DesiredStatus string  `json:"desiredStatus"`
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

// InstanceInfo stores information about a RunPod instance in the cluster
type InstanceInfo struct {
	ID            string
	CostPerHr     float64
	PodName       string
	Namespace     string
	Status        string
	StatusMessage string
	ExitCode      int
	CreationTime  time.Time
	RequestedPorts []string  // Ports that were requested for this pod
	PortsExposed  bool      // Tracks whether requested ports are currently exposed
}

type DetailedStatus struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	DesiredStatus          string            `json:"desiredStatus"`
	CurrentStatus          string            `json:"currentStatus,omitempty"`
	CostPerHr              float64           `json:"costPerHr"`
	Image                  string            `json:"image"`
	Env                    map[string]string `json:"env"`
	MachineID              string            `json:"machineId"`
	PortMappings           map[string]int    `json:"portMappings"`
	Runtime                *RuntimeInfo      `json:"runtime,omitempty"`
	Machine                *MachineInfo      `json:"machine,omitempty"`
	LastError              string            `json:"lastError,omitempty"`
	ContainerRegistryAuthId string           `json:"containerRegistryAuthId,omitempty"`
	TemplateId             string            `json:"templateId,omitempty"`
}

type RuntimeInfo struct {
	Container struct {
		ExitCode int    `json:"exitCode,omitempty"`
		Message  string `json:"message,omitempty"`
	} `json:"container,omitempty"`
	PodCompletionStatus string `json:"podCompletionStatus,omitempty"`
}

type MachineInfo struct {
	GPUTypeID    string `json:"gpuTypeId"`
	Location     string `json:"location"`
	DataCenterID string `json:"dataCenterId"`
}

// NewRunPodClient creates a new RunPod API client
func NewRunPodClient(logger *slog.Logger, clientset kubernetes.Interface) *Client {
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		logger.Error("RUNPOD_API_KEY environment variable is not set")
	}

	return &Client{
		httpClient:     &http.Client{Timeout: DefaultAPITimeout},
		apiKey:         apiKey,
		baseGraphqlURL: "https://api.runpod.io/graphql",
		baseRESTURL:    "https://rest.runpod.io/v1/",
		logger:         logger,
		clientset:      clientset, // Store the clientset
	}
}

// ExecuteGraphQL executes a GraphQL query with proper error handling
func (c *Client) ExecuteGraphQL(query string, variables map[string]interface{}, response interface{}) error {
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
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Error closing response body", "error", err)
		}
	}()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(response)
}

// GetPodStatus checks the status of a RunPod instance, experimental as it was not working
func (c *Client) GetPodStatusGraphql(podID string) (PodStatus, error) {
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

type retryState struct {
	lastError        error
	lastStatusCode   int
	lastResponseBody string
}

// performRetryableRequest performs a request with retries
func (c *Client) performRetryableRequest(endpoint string, podID string) (*http.Response, *retryState, error) {
	// Add retries for temporary errors
	maxRetries := 3
	state := &retryState{}

	for retryCount := 0; retryCount < maxRetries; retryCount++ {
		resp, err := c.makeRESTRequest("GET", endpoint, nil)

		// Both 200 OK and 404 Not Found are considered valid responses for our purpose
		if err == nil && resp != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound) {
			return resp, state, nil
		}

		// Store the error details for better reporting
		state.lastError = err
		if resp != nil {
			state.lastStatusCode = resp.StatusCode
			// Try to read the response body for error details
			c.captureResponseBody(resp, state)
		}

		if retryCount < maxRetries-1 {
			c.logger.Info("Retrying pod status check after error",
				"podID", podID,
				"retry", retryCount+1,
				"error", err,
				"statusCode", state.lastStatusCode)
			time.Sleep(time.Duration(retryCount+1) * 500 * time.Millisecond)
		}
	}

	return nil, state, fmt.Errorf("failed after %d attempts", maxRetries)
}

// captureResponseBody reads and stores response body for error reporting
func (c *Client) captureResponseBody(resp *http.Response, state *retryState) {
	if resp.Body == nil {
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err == nil {
		state.lastResponseBody = string(bodyBytes)
		c.logger.Error("Failed getting pod status", "error", state.lastResponseBody)
	}

	if err := resp.Body.Close(); err != nil {
		c.logger.Warn("Error closing response body", "error", err)
	}
}

// buildErrorMessage builds a detailed error message from retry state
func buildErrorMessage(baseMsg string, state *retryState) string {
	if state.lastError != nil {
		baseMsg = fmt.Sprintf("%s, last error: %v", baseMsg, state.lastError)
	}
	if state.lastStatusCode > 0 {
		baseMsg = fmt.Sprintf("%s, last status code: %d", baseMsg, state.lastStatusCode)
	}
	if state.lastResponseBody != "" {
		body := state.lastResponseBody
		// Truncate very long response bodies
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		baseMsg = fmt.Sprintf("%s, last response: %s", baseMsg, body)
	}
	return baseMsg
}

// handle404Response processes 404 responses
func (c *Client) handle404Response(resp *http.Response, podID string) (PodStatus, error) {
	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PodNotFound, fmt.Errorf("error reading 404 response body: %w", err)
	}

	// Check if this is the specific "pod not found" message we expect
	var errorResponse struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}

	if err := json.Unmarshal(body, &errorResponse); err == nil {
		if errorResponse.Error == "pod not found" && errorResponse.Status == 404 {
			// This is the expected "pod not found" response, return PodNotFound status
			return PodNotFound, nil
		}
	}

	// If we can't parse the JSON, or it doesn't match our expected error format,
	// log it but still return PodNotFound since a 404 generally indicates the pod doesn't exist
	c.logger.Warn("Received unexpected 404 response format",
		"podID", podID,
		"body", string(body))
	return PodNotFound, nil
}

// handleNonOKResponse processes non-200 responses
func handleNonOKResponse(resp *http.Response) (PodStatus, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PodNotFound, fmt.Errorf("RunPod API error with status %d and error reading body: %w",
			resp.StatusCode, err)
	}
	return PodNotFound, fmt.Errorf("RunPod API error: status %d, body: %s",
		resp.StatusCode, string(body))
}

// GetPodStatusREST gets pod status from RunPod REST API
func (c *Client) GetPodStatusREST(podID string) (PodStatus, error) {
	endpoint := fmt.Sprintf("pods/%s", podID)

	resp, state, err := c.performRetryableRequest(endpoint, podID)
	if err != nil {
		errorMsg := buildErrorMessage("API request failed after 3 attempts", state)
		return PodNotFound, fmt.Errorf("%s", errorMsg)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Error closing response body", "error", err)
		}
	}()

	// Handle different response codes
	switch resp.StatusCode {
	case http.StatusNotFound:
		// Handle 404 Not Found responses
		return c.handle404Response(resp, podID)
	case http.StatusOK:
		// Continue to decode response
	default:
		return handleNonOKResponse(resp)
	}

	// Decode successful response
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
func (c *Client) GetGPUTypes(minRAMPerGPU int, maxPrice float64, cloudType string) ([]string, error) {
	//https://graphql-spec.io/#query-gpuTypes
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

func (c *Client) DeployPodREST(params map[string]interface{}) (string, float64, error) {
	// https://rest.runpod.io/v1/docs#tag/pods/POST/pods
	reqBody, err := json.Marshal(params)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.makeRESTRequest("POST", "pods", bytes.NewBuffer(reqBody))
	if err != nil {
		c.logger.Error("REST API request failed when deploying pod", "err", err)
		return "", 0, fmt.Errorf("API request failed: %w", err)
	}

	if resp == nil {
		return "", 0, fmt.Errorf("received nil response from makeRESTRequest")
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Error closing response body", "error", err)
		}
	}()

	// Read the full response body
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		c.logger.Error("Failed to read response body",
			"error", readErr,
			"statusCode", resp.StatusCode)
		return "", 0, fmt.Errorf("failed to read response body: %w", readErr)
	}

	// Log the raw response for debugging (truncated if too large)
	responseString := string(body)
	if len(responseString) > 500 {
		c.logger.Debug("Received response from RunPod API (truncated)",
			"statusCode", resp.StatusCode,
			"bodyLength", len(responseString),
			"responseStart", responseString[:500])
	} else {
		c.logger.Debug("Received response from RunPod API",
			"statusCode", resp.StatusCode,
			"response", responseString)
	}

	if resp.StatusCode >= 400 {
		c.logger.Error("RunPod REST API returned error",
			"statusCode", resp.StatusCode,
			"response", string(body))
		return "", 0, fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	// Check if response is empty
	if len(body) == 0 {
		c.logger.Error("RunPod API returned empty response body",
			"statusCode", resp.StatusCode)
		return "", 0, fmt.Errorf("API returned empty response body with status %d", resp.StatusCode)
	}

	var response struct {
		ID                     string  `json:"id"`
		CostPerHr              float64 `json:"costPerHr"` // JSON returns this as a string
		MachineID              string  `json:"machineId"`
		Name                   string  `json:"name"`
		DesiredStatus          string  `json:"desiredStatus"`
		Image                  string  `json:"image"` // Change from ImageName to match JSON
		ContainerRegistryAuthId string `json:"containerRegistryAuthId,omitempty"`
		TemplateId             string  `json:"templateId,omitempty"`

		Machine struct {
			DataCenterID string `json:"dataCenterId"`
			GpuTypeID    string `json:"gpuTypeId"`
			Location     string `json:"location"`
			SecureCloud  bool   `json:"secureCloud"`
		} `json:"machine"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		c.logger.Error("Failed to parse JSON response",
			"error", err,
			"response", string(body),
			"responseLength", len(body))
		return "", 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.ID == "" {
		return "", 0, fmt.Errorf("pod deployment failed: %s", string(body))
	}

	// Log deployment success with all relevant fields
	logFields := []interface{}{
		"podId", response.ID,
		"costPerHr", response.CostPerHr,
		"machineId", response.MachineID,
		"gpuType", response.Machine.GpuTypeID,
		"location", response.Machine.Location,
		"dataCenter", response.Machine.DataCenterID,
	}
	
	// Add containerRegistryAuthId to log if present
	if response.ContainerRegistryAuthId != "" {
		logFields = append(logFields, "containerRegistryAuthId", response.ContainerRegistryAuthId)
	}
	
	// Add templateId to log if present
	if response.TemplateId != "" {
		logFields = append(logFields, "templateId", response.TemplateId)
	}
	
	c.logger.Info("Pod deployed successfully", logFields...)

	return response.ID, response.CostPerHr, nil
}


// DeployPod deploys a pod to RunPod
func (c *Client) DeployPod(params map[string]interface{}) (string, float64, error) {
	//https://graphql-spec.io/#definition-PodFindAndDeployOnDemandInput
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
func (c *Client) TerminatePod(podID string) error {
	endpoint := fmt.Sprintf("/pods/%s/stop", podID)

	resp, err := c.makeRESTRequest("POST", endpoint, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Error closing response body", "error", err)
		}
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("unauthorized: invalid API key")
	}

	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("invalid pod ID: %s", podID)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to terminate pod, status: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

// makeRESTRequest is a helper function to make REST API requests to RunPod
func (c *Client) makeRESTRequest(method, endpoint string, body io.Reader) (*http.Response, error) {
	url := fmt.Sprintf("%s%s", c.baseRESTURL, endpoint)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	// Determine an appropriate timeout based on the endpoint
	timeout := DefaultAPITimeout
	if method == "POST" && endpoint == "pods" {
		timeout = 60 * time.Second // Use a longer timeout for pod deployment
	}

	// Don't use context with timeout for the request, use it for the client instead
	client := &http.Client{Timeout: timeout}

	// No context timeout here, just use the request as is
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	// No longer treat 404 as an error - return the response as is
	// The calling function will determine how to handle 404 status codes
	return resp, nil
}

// GetDetailedPodStatus gets detailed information about a RunPod instance
func (c *Client) GetDetailedPodStatus(podID string) (*DetailedStatus, error) {
	endpoint := fmt.Sprintf("pods/%s", podID)

	//example response
	//{\"consumerUserId\":\"user_2QkkFxZghNIYZFVD7mAVsVxNjF1\",\"containerDiskInGb\":15,\"costPerHr\":0.29,\"createdAt\":\"2025-03-30 19:14:42.76 +0000 UTC\",\"desiredStatus\":\"RUNNING\",\"env\":{\"TEST_ENV_VAR\":\"test_value\"},\"gpuCount\":1,\"id\":\"bhxlpbddq58wog\",\"imageName\":\"nvidia/cuda:11.7.1-base-ubuntu22.04\",\"lastStartedAt\":\"2025-03-30 19:14:42.752 +0000 UTC\",\"lastStatusChange\":\"Rented by User: Sun Mar 30 2025 19:14:42 GMT+0000 (Coordinated Universal Time)\",\"machine\":{},\"machineId\":\"p1pm7pqlvvco\",\"memoryInGb\":62,\"name\":\"runpod-test-pod-1743362081\",\"portMappings\":null,\"publicIp\":\"\",\"templateId\":\"8pjdlrsfmx\",\"vcpuCount\":16,\"volumeInGb\":0,\"volumeMountPath\":\"/workspace\"}\n"}
	resp, err := c.makeRESTRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod details: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Error closing response body", "error", err)
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return &DetailedStatus{
			ID:            podID,
			DesiredStatus: string(PodNotFound),
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("RunPod API error with status %d and error reading body: %w",
				resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("RunPod API error: status %d, body: %s",
			resp.StatusCode, string(body))
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	// Parse the response
	var status DetailedStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to parse pod status: %w", err)
	}

	return &status, nil
}

// IsSuccessfulCompletion determines if a RunPod instance exited successfully
func IsSuccessfulCompletion(status *DetailedStatus) bool {
	// If we don't have runtime information, we can't determine
	if status.Runtime == nil {
		return false
	}

	// Check exit code - 0 indicates success
	exitCode := status.Runtime.Container.ExitCode

	// Some pods might not have a clear exit code but have a completion status
	if exitCode == 0 {
		return true
	}

	// Check completion status for additional context
	completionStatus := status.Runtime.PodCompletionStatus
	if completionStatus != "" {
		return strings.Contains(strings.ToLower(completionStatus), "success") ||
			strings.Contains(strings.ToLower(completionStatus), "completed")
	}

	return false
}

// FormatEnvVarsForGraphQL formats environment variables for GraphQL API
func FormatEnvVarsForGraphQL(envVars []RunPodEnv) []map[string]string {
	formatted := make([]map[string]string, len(envVars))
	for i, env := range envVars {
		formatted[i] = map[string]string{
			"key":   env.Key,
			"value": env.Value,
		}
	}
	return formatted
}

// FormatEnvVarsForREST formats environment variables for REST API
func FormatEnvVarsForREST(envVars []RunPodEnv) map[string]string {
	formatted := make(map[string]string)
	for _, env := range envVars {
		formatted[env.Key] = env.Value
	}
	return formatted
}

type secretMapping struct {
	SecretKey string
	EnvKey    string
}

type secretCollector struct {
	secretsToFetch map[string]bool
	secretEnvVars  map[string][]secretMapping
	secretRefEnvs  map[string]bool
}

func newSecretCollector() *secretCollector {
	return &secretCollector{
		secretsToFetch: make(map[string]bool),
		secretEnvVars:  make(map[string][]secretMapping),
		secretRefEnvs:  make(map[string]bool),
	}
}

// isK8sAutoInjectedVar checks if a key is a Kubernetes auto-injected variable
func isK8sAutoInjectedVar(key string) bool {
	// Common prefixes for auto-injected variables.
	prefixes := []string{
		"KUBERNETES_",
		"_PORT_",         //this might lead to false positive for unaware users
		"_TCP_",          //this might lead to false positive for unaware users
		"_SERVICE_PORT_", //k8s adds services as env vars (https://kubernetes.io/docs/concepts/containers/container-environment/#cluster-information)
		"_SERVICE_HOST",  //auto added services
	}

	// Check if the key matches any of the patterns
	for _, prefix := range prefixes {
		if strings.Contains(key, prefix) {
			return true
		}
	}

	return false
}

// processContainerEnv processes environment variables from container
func (c *Client) processContainerEnv(container v1.Container, envVars *[]RunPodEnv, collector *secretCollector) {
	// Add regular environment variables
	for _, env := range container.Env {
		// Skip Kubernetes auto-injected variables
		if isK8sAutoInjectedVar(env.Name) {
			continue
		}

		// If this is a regular env var, add it directly
		if env.Value != "" {
			*envVars = append(*envVars, RunPodEnv{
				Key:   env.Name,
				Value: env.Value,
			})
		} else if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			// This is a secret reference, track it for fetching
			secretName := env.ValueFrom.SecretKeyRef.Name
			collector.secretsToFetch[secretName] = true

			if _, ok := collector.secretEnvVars[secretName]; !ok {
				collector.secretEnvVars[secretName] = []secretMapping{}
			}

			// Store the mapping between secret key and env var name
			collector.secretEnvVars[secretName] = append(collector.secretEnvVars[secretName], secretMapping{
				SecretKey: env.ValueFrom.SecretKeyRef.Key,
				EnvKey:    env.Name,
			})
		}
	}

	// Handle envFrom references
	for _, envFrom := range container.EnvFrom {
		if envFrom.SecretRef != nil {
			secretName := envFrom.SecretRef.Name
			collector.secretsToFetch[secretName] = true
			collector.secretRefEnvs[secretName] = true
		}
	}
}

// processVolumeSecrets processes secrets from volume mounts
func (c *Client) processVolumeSecrets(volumes []v1.Volume, collector *secretCollector) {
	// Also check for secrets mounted as volumes that should be included as env vars
	for _, volume := range volumes {
		if volume.Secret == nil {
			continue
		}

		secretName := volume.Secret.SecretName
		collector.secretsToFetch[secretName] = true

		// For volume mounts with specific items
		if items := volume.Secret.Items; len(items) > 0 {
			if _, ok := collector.secretEnvVars[secretName]; !ok {
				collector.secretEnvVars[secretName] = []secretMapping{}
			}

			for _, item := range items {
				// When mounted as a volume item, use the path as env var name if not specified
				envKey := item.Path
				if envKey == "" {
					envKey = item.Key
				}

				collector.secretEnvVars[secretName] = append(collector.secretEnvVars[secretName], secretMapping{
					SecretKey: item.Key,
					EnvKey:    envKey,
				})
			}
		}
	}
}

// processSecretData processes data from a fetched secret
func (c *Client) processSecretData(secret *v1.Secret, secretName string, collector *secretCollector, envVars *[]RunPodEnv) {
	// Handle specific secret keys mapped to env vars
	if mappings, ok := collector.secretEnvVars[secretName]; ok {
		for _, mapping := range mappings {
			if secretValue, ok := secret.Data[mapping.SecretKey]; ok {
				// Skip if the mapped key is a Kubernetes auto-injected variable
				if isK8sAutoInjectedVar(mapping.EnvKey) {
					continue
				}

				// Add it as an environment variable with the correct name
				*envVars = append(*envVars, RunPodEnv{
					Key:   mapping.EnvKey,
					Value: strings.ReplaceAll(string(secretValue), "\n", "\\n"),
				})
			} else {
				c.logger.Warn("Secret key not found",
					"namespace", secret.Namespace,
					"secret", secretName,
					"key", mapping.SecretKey)
			}
		}
	}

	// Handle envFrom that should import all keys from the secret
	if collector.secretRefEnvs[secretName] {
		for key, value := range secret.Data {
			// Skip Kubernetes auto-injected variables
			if isK8sAutoInjectedVar(key) {
				continue
			}

			*envVars = append(*envVars, RunPodEnv{
				Key:   key,
				Value: strings.ReplaceAll(string(value), "\n", "\\n"),
			})
		}
	}
}

// ExtractEnvVars extracts environment variables from a pod, filters auto-injected variables as they are only relevant for pods running inside the cluster and not for RunPod, reduce attack surface
func (c *Client) ExtractEnvVars(pod *v1.Pod) ([]RunPodEnv, error) {
	var envVars []RunPodEnv
	collector := newSecretCollector()

	// First pass: collect all secrets we need to fetch
	if len(pod.Spec.Containers) > 0 {
		c.processContainerEnv(pod.Spec.Containers[0], &envVars, collector)
	}

	// Process volume secrets
	c.processVolumeSecrets(pod.Spec.Volumes, collector)

	// Second pass: fetch all needed secrets and extract values
	for secretName := range collector.secretsToFetch {
		// Use the clientset from the Client struct
		secret, err := c.clientset.CoreV1().Secrets(pod.Namespace).Get(
			context.Background(),
			secretName,
			metav1.GetOptions{},
		)
		if err != nil {
			c.logger.Error("failed to get secret",
				"namespace", pod.Namespace,
				"secret", secretName, "err", err)
			continue
		}

		c.processSecretData(secret, secretName, collector, &envVars)
	}

	return envVars, nil
}

// getOwnerJob retrieves the owner job of a pod if it exists
func (c *Client) getOwnerJob(pod *v1.Pod) *batchv1.Job {
	c.logger.Debug("Looking for owner job for pod", 
		"pod", pod.Name, 
		"namespace", pod.Namespace,
		"ownerReferences", len(pod.OwnerReferences))
	
	for _, owner := range pod.OwnerReferences {
		c.logger.Debug("Found owner reference", 
			"kind", owner.Kind, 
			"name", owner.Name, 
			"uid", owner.UID)
			
		if owner.Kind == "Job" {
			job, err := c.clientset.BatchV1().Jobs(pod.Namespace).Get(
				context.Background(),
				owner.Name,
				metav1.GetOptions{},
			)
			if err == nil {
				// Verify the UID matches to ensure we have the correct job instance
				if job.UID == owner.UID {
					c.logger.Debug("Found owner job with annotations", 
						"job", job.Name, 
						"jobUID", job.UID,
						"annotations", len(job.Annotations))
					return job
				} else {
					c.logger.Debug("Job found but UID mismatch", 
						"job", job.Name,
						"expectedUID", owner.UID,
						"actualUID", job.UID)
				}
			} else {
				c.logger.Warn("Failed to get owner job", 
					"job", owner.Name, 
					"namespace", pod.Namespace, 
					"error", err)
			}
		}
	}
	c.logger.Debug("No owner job found for pod", "pod", pod.Name)
	return nil
}

// getAnnotationWithFallback gets annotation value with fallback to job annotations
func getAnnotationWithFallback(pod *v1.Pod, ownerJob *batchv1.Job, annotationKey string, defaultValue string) string {
	if val, exists := pod.Annotations[annotationKey]; exists && val != "" {
		return val
	}
	if ownerJob != nil && ownerJob.Annotations != nil {
		if val, exists := ownerJob.Annotations[annotationKey]; exists && val != "" {
			return val
		}
	}
	return defaultValue
}

// validateCloudType validates and normalizes the cloud type value
func (c *Client) validateCloudType(cloudTypeVal string, pod *v1.Pod) string {
	// Determine cloud type - default to SECURE but allow override via annotation
	defaultCloudType := "SECURE"
	if cloudTypeVal == "" {
		return defaultCloudType
	}

	// Validate and normalize the cloud type value
	cloudTypeUpperCase := strings.ToUpper(cloudTypeVal)
	if cloudTypeUpperCase == "SECURE" || cloudTypeUpperCase == "COMMUNITY" {
		return cloudTypeUpperCase
	}

	c.logger.Warn("Invalid cloud type specified, using default",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"specifiedValue", cloudTypeVal,
		"defaultValue", defaultCloudType)
	return defaultCloudType
}

// extractGPUMemory extracts GPU memory from annotations
func extractGPUMemory(memStr string) int {
	defaultMemory := 16 // Default minimum memory
	if memStr == "" {
		return defaultMemory
	}

	if mem, err := strconv.Atoi(memStr); err == nil {
		return mem
	}
	return defaultMemory
}

// extractPortsFromPod extracts port specifications from a Kubernetes pod
// and converts them to RunPod format (e.g., "8080/http", "5432/tcp")
func (c *Client) extractPortsFromPod(pod *v1.Pod) []string {
	var ports []string
	
	// Common HTTP ports that should default to HTTP protocol
	httpPorts := map[int32]bool{
		80:   true,
		443:  true,
		8080: true,
		8000: true,
		3000: true,
		5000: true,
		8888: true,
		9000: true,
	}
	
	// Process all containers in the pod
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			// Skip unsupported protocols (RunPod only supports TCP-based protocols)
			if port.Protocol != "" && port.Protocol != v1.ProtocolTCP {
				c.logger.Warn("Skipping unsupported protocol",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"port", port.ContainerPort,
					"protocol", port.Protocol)
				continue
			}
			
			// Determine protocol - default to TCP unless it's a common HTTP port
			protocol := "tcp"
			if httpPorts[port.ContainerPort] {
				protocol = "http"
				c.logger.Info("Auto-detecting HTTP protocol for common port",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"port", port.ContainerPort,
					"note", "Use annotation 'runpod.io/ports' to override if needed")
			}
			
			// Format as RunPod expects: "port/protocol"
			portSpec := fmt.Sprintf("%d/%s", port.ContainerPort, protocol)
			ports = append(ports, portSpec)
			
			c.logger.Debug("Adding port to RunPod deployment",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"portSpec", portSpec)
		}
	}
	
	return ports
}

// determineInterruptible checks if a pod should be deployed as interruptible (spot instance)
// based on its PriorityClassName or annotation
func (c *Client) determineInterruptible(pod *v1.Pod, ownerJob *batchv1.Job) bool {
	// First check for explicit annotation override
	if val, exists := pod.Annotations[InterruptibleAnnotation]; exists {
		if val == "true" {
			return true
		} else if val == "false" {
			return false
		}
	}
	
	// Check job annotations if pod is owned by a job
	if ownerJob != nil && ownerJob.Annotations != nil {
		if val, exists := ownerJob.Annotations[InterruptibleAnnotation]; exists {
			if val == "true" {
				return true
			} else if val == "false" {
				return false
			}
		}
	}
	
	// Check PriorityClassName - if it contains "spot" (case-insensitive), use interruptible
	if pod.Spec.PriorityClassName != "" {
		priorityClass := strings.ToLower(pod.Spec.PriorityClassName)
		if strings.Contains(priorityClass, "spot") {
			return true
		}
	}
	
	// Default to non-interruptible (priority/reserved instance)
	return false
}

// PrepareRunPodParameters prepares parameters for RunPod deployment
// Update PrepareRunPodParameters to use the clientset from the Client struct
func (c *Client) PrepareRunPodParameters(pod *v1.Pod, graphql bool) (map[string]interface{}, error) {
	// Check if pod is owned by a job and use job annotations if available
	ownerJob := c.getOwnerJob(pod)

	// Helper function for getting annotations
	getAnnotation := func(annotationKey string, defaultValue string) string {
		return getAnnotationWithFallback(pod, ownerJob, annotationKey, defaultValue)
	}

	// Get and validate cloud type
	cloudTypeVal := getAnnotation(CloudTypeAnnotation, "")
	cloudType := c.validateCloudType(cloudTypeVal, pod)

	// Get other annotations
	containerRegistryAuthId := getAnnotation(ContainerRegistryAuthAnnotation, "")
	templateId := getAnnotation(TemplateIdAnnotation, "")
	datacenterID := getAnnotation(DatacenterAnnotation, "")
	portsOverride := getAnnotation(PortsAnnotation, "")

	// Determine minimum GPU memory required
	memStr := getAnnotation(GpuMemoryAnnotation, "")
	minRAMPerGPU := extractGPUMemory(memStr)

	// Get GPU types - pass the cloud type to filter correctly
	gpuTypes, err := c.GetGPUTypes(minRAMPerGPU, DefaultMaxPrice, cloudType)
	if err != nil {
		return nil, fmt.Errorf("failed to get GPU types: %w", err)
	}

	// Extract environment variables from job
	envVars, err := c.ExtractEnvVars(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to extract environment variables: %w", err)
	}

	// Format environment variables
	var formattedEnvVars interface{}
	if graphql {
		formattedEnvVars = FormatEnvVarsForGraphQL(envVars)
	} else {
		formattedEnvVars = FormatEnvVarsForREST(envVars)
	}

	// Determine image name from pod
	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod has no containers")
	}
	imageName := pod.Spec.Containers[0].Image

	// Use the pod name as the RunPod name
	runpodName := pod.Name

	// Extract ports from pod specification
	ports := c.extractPortsFromPod(pod)
	
	// Allow manual override via annotation
	if portsOverride != "" {
		// Parse the comma-separated list of ports
		overridePorts := strings.Split(portsOverride, ",")
		// Trim spaces from each port spec
		for i, port := range overridePorts {
			overridePorts[i] = strings.TrimSpace(port)
		}
		
		c.logger.Info("Using manual port specification from annotation",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"manualPorts", overridePorts,
			"autoDetectedPorts", ports)
		ports = overridePorts
	}

	// Default values
	volumeInGb := 0
	containerDiskInGb := 15
	
	// Determine if this should be an interruptible (spot) instance
	interruptible := c.determineInterruptible(pod, ownerJob)

	// Create deployment parameters
	params := map[string]interface{}{
		"cloudType":         cloudType,
		"volumeInGb":        volumeInGb,
		"containerDiskInGb": containerDiskInGb,
		"minRAMPerGPU":      minRAMPerGPU,
		"gpuTypeIds":        gpuTypes, // Use the array directly, don't stringify it
		"name":              runpodName,
		"imageName":         imageName,
		"env":               formattedEnvVars,
		"interruptible":     interruptible,
	}
	
	// Add ports to parameters if any were specified
	if len(ports) > 0 {
		params["ports"] = ports
		instanceType := "priority"
		if interruptible {
			instanceType = "spot"
		}
		c.logger.Info("Configuring RunPod deployment",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"instanceType", instanceType,
			"ports", ports)
	} else {
		// Log even when no ports are specified
		instanceType := "priority"
		if interruptible {
			instanceType = "spot"
		}
		c.logger.Info("Configuring RunPod deployment",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"instanceType", instanceType)
	}

	// Add datacenter IDs if specified in pod annotations
	if datacenterID != "" {
		// Split comma-separated datacenter IDs
		datacenterIds := strings.Split(datacenterID, ",")
		// Trim whitespace from each ID
		for i, id := range datacenterIds {
			datacenterIds[i] = strings.TrimSpace(id)
		}
		params["dataCenterIds"] = datacenterIds
	}

	// Add templateId to params if it exists
	if templateId != "" {
		params["templateId"] = templateId
	}

	if containerRegistryAuthId != "" {
		params["containerRegistryAuthId"] = containerRegistryAuthId
	}

	// Return both params and the ports that were requested
	// We'll need to update the callers to handle the ports
	return params, nil
}

// GetRequestedPorts extracts the ports that were configured for a pod deployment
// This is a helper to get ports separately when needed
func (c *Client) GetRequestedPorts(pod *v1.Pod) []string {
	// Check for annotation override first
	if portsOverride, exists := pod.Annotations[PortsAnnotation]; exists && portsOverride != "" {
		overridePorts := strings.Split(portsOverride, ",")
		for i, port := range overridePorts {
			overridePorts[i] = strings.TrimSpace(port)
		}
		return overridePorts
	}
	
	// Otherwise extract from pod spec
	return c.extractPortsFromPod(pod)
}

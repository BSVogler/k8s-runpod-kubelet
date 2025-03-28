package runpod

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
	"time"
)

// Client handles all interactions with the RunPod API
type Client struct {
	httpClient     *http.Client
	apiKey         string
	baseGraphqlURL string
	baseRESTURL    string
	logger         *slog.Logger
}

// Constants for RunPod integration
const (
	RunpodOffloadAnnotation               = "runpod.io/offload"
	RunpodOffloadedAnnotation             = "runpod.io/offloaded"
	RunpodPodIDAnnotation                 = "runpod.io/pod-id"
	RunpodCostAnnotation                  = "runpod.io/cost-per-hr"
	RunpodRetryAnnotation                 = "runpod.io/retry-after"
	RunpodCloudTypeAnnotation             = "runpod.io/cloud-type"
	RunpodTemplateIdAnnotation            = "runpod.io/templateId"
	GpuMemoryAnnotation                   = "runpod.io/required-gpu-memory"
	RunpodContainerRegistryAuthAnnotation = "runpod.io/container-registry-auth-id"
	RunpodFailureAnnotation               = "runpod.io/failure-count"
	RunpodManagedLabel                    = "runpod.io/managed"
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

// RunPodInfo stores information about a RunPod instance in the cluster
type RunPodInfo struct {
	ID            string
	CostPerHr     float64
	PodName       string
	Namespace     string
	Status        string
	StatusMessage string
	ExitCode      int
	CreationTime  time.Time
}

type RunPodDetailedStatus struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	DesiredStatus string            `json:"desiredStatus"`
	CurrentStatus string            `json:"currentStatus,omitempty"`
	CostPerHr     string            `json:"costPerHr"`
	Image         string            `json:"image"`
	Env           map[string]string `json:"env"`
	MachineID     string            `json:"machineId"`
	Runtime       *RuntimeInfo      `json:"runtime,omitempty"`
	Machine       *MachineInfo      `json:"machine,omitempty"`
	LastError     string            `json:"lastError,omitempty"`
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
func NewRunPodClient(logger *slog.Logger) *Client {
	apiKey := os.Getenv("RUNPOD_KEY")
	if apiKey == "" {
		logger.Error("RUNPOD_KEY environment variable is not set")
	}

	return &Client{
		httpClient:     &http.Client{Timeout: DefaultAPITimeout},
		apiKey:         apiKey,
		baseGraphqlURL: "https://api.io/graphql",
		baseRESTURL:    "https://rest.io/v1/",
		logger:         logger,
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
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(response)
}

// GetPodStatus checks the status of a RunPod instance
func (c *Client) GetPodStatus(podID string) (PodStatus, error) {
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

func (c *Client) GetPodStatusREST(podID string) (PodStatus, error) {
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

		if err == nil && resp != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound) {
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
	// https://rest.io/v1/docs#tag/pods/POST/pods
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
		ID            string `json:"id"`
		CostPerHr     string `json:"costPerHr"` // JSON returns this as a string
		MachineID     string `json:"machineId"`
		Name          string `json:"name"`
		DesiredStatus string `json:"desiredStatus"`
		Image         string `json:"image"` // Change from ImageName to match JSON

		Machine struct {
			DataCenterID string `json:"dataCenterId"`
			GpuTypeID    string `json:"gpuTypeId"`
			Location     string `json:"location"`
			SecureCloud  bool   `json:"secureCloud"`
		} `json:"machine"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.ID == "" {
		return "", 0, fmt.Errorf("pod deployment failed: %s", string(body))
	}

	// Convert CostPerHr to float64
	costPerHr, err := strconv.ParseFloat(response.CostPerHr, 64)
	if err != nil {
		c.logger.Error("Failed to convert CostPerHr to float64", "costPerHr", response.CostPerHr, "err", err)
		return "", 0, fmt.Errorf("invalid costPerHr value: %s", response.CostPerHr)
	}

	c.logger.Info("Pod deployed successfully",
		"podId", response.ID,
		"costPerHr", costPerHr,
		"machineId", response.MachineID,
		"gpuType", response.Machine.GpuTypeID,
		"location", response.Machine.Location,
		"dataCenter", response.Machine.DataCenterID)

	return response.ID, costPerHr, nil
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
	defer resp.Body.Close()

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

// GetDetailedPodStatus gets detailed information about a RunPod instance
func (c *Client) GetDetailedPodStatus(podID string) (*RunPodDetailedStatus, error) {
	endpoint := fmt.Sprintf("pods/%s", podID)

	resp, err := c.makeRESTRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod details: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &RunPodDetailedStatus{
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
	var status RunPodDetailedStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to parse pod status: %w", err)
	}

	return &status, nil
}

// IsSuccessfulCompletion determines if a RunPod instance exited successfully
func IsSuccessfulCompletion(status *RunPodDetailedStatus) bool {
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

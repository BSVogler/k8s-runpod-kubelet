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
	clientset      *kubernetes.Clientset // Add clientset field
}

// Constants for RunPod integration
const (
	RunpodPodIDAnnotation                 = "runpod.io/pod-id"
	RunpodCostAnnotation                  = "runpod.io/cost-per-hr"
	RunpodCloudTypeAnnotation             = "runpod.io/cloud-type"
	RunpodTemplateIdAnnotation            = "runpod.io/templateId"
	GpuMemoryAnnotation                   = "runpod.io/required-gpu-memory"
	RunpodContainerRegistryAuthAnnotation = "runpod.io/container-registry-auth-id"
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
func NewRunPodClient(logger *slog.Logger, clientset *kubernetes.Clientset) *Client {
	apiKey := os.Getenv("RUNPOD_KEY")
	if apiKey == "" {
		logger.Error("RUNPOD_KEY environment variable is not set")
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
		return PodNotFound, fmt.Errorf("%s", errorMsg)
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

// ExtractEnvVars extracts environment variables from a pod
func (c *Client) ExtractEnvVars(pod *v1.Pod) ([]RunPodEnv, error) {
	var envVars []RunPodEnv
	var secretsToFetch = make(map[string]bool)
	var secretEnvVars = make(map[string][]struct {
		SecretKey string
		EnvKey    string
	})
	var secretRefEnvs = make(map[string]bool)

	// First pass: collect all secrets we need to fetch
	if len(pod.Spec.Containers) > 0 {
		container := pod.Spec.Containers[0]

		// Add regular environment variables
		for _, env := range container.Env {
			// If this is a regular env var, add it directly
			if env.Value != "" {
				envVars = append(envVars, RunPodEnv{
					Key:   env.Name,
					Value: env.Value,
				})
			} else if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				// This is a secret reference, track it for fetching
				secretName := env.ValueFrom.SecretKeyRef.Name
				secretsToFetch[secretName] = true

				if _, ok := secretEnvVars[secretName]; !ok {
					secretEnvVars[secretName] = []struct {
						SecretKey string
						EnvKey    string
					}{}
				}

				// Store the mapping between secret key and env var name
				secretEnvVars[secretName] = append(secretEnvVars[secretName], struct {
					SecretKey string
					EnvKey    string
				}{
					SecretKey: env.ValueFrom.SecretKeyRef.Key,
					EnvKey:    env.Name,
				})
			}
		}

		// Handle envFrom references
		for _, envFrom := range container.EnvFrom {
			if envFrom.SecretRef != nil {
				secretName := envFrom.SecretRef.Name
				secretsToFetch[secretName] = true
				secretRefEnvs[secretName] = true
			}
		}
	}

	// Also check for secrets mounted as volumes that should be included as env vars
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			secretName := volume.Secret.SecretName
			secretsToFetch[secretName] = true

			// For volume mounts with specific items
			if items := volume.Secret.Items; len(items) > 0 {
				if _, ok := secretEnvVars[secretName]; !ok {
					secretEnvVars[secretName] = []struct {
						SecretKey string
						EnvKey    string
					}{}
				}

				for _, item := range items {
					// When mounted as a volume item, use the path as env var name if not specified
					envKey := item.Path
					if envKey == "" {
						envKey = item.Key
					}

					secretEnvVars[secretName] = append(secretEnvVars[secretName], struct {
						SecretKey string
						EnvKey    string
					}{
						SecretKey: item.Key,
						EnvKey:    envKey,
					})
				}
			}
		}
	}

	// Second pass: fetch all needed secrets and extract values
	for secretName := range secretsToFetch {
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

		// Handle specific secret keys mapped to env vars
		if mappings, ok := secretEnvVars[secretName]; ok {
			for _, mapping := range mappings {
				if secretValue, ok := secret.Data[mapping.SecretKey]; ok {
					// Add it as an environment variable with the correct name
					envVars = append(envVars, RunPodEnv{
						Key:   mapping.EnvKey,
						Value: strings.ReplaceAll(string(secretValue), "\n", "\\n"),
					})
				} else {
					c.logger.Warn("Secret key not found",
						"namespace", pod.Namespace,
						"secret", secretName,
						"key", mapping.SecretKey)
				}
			}
		}

		// Handle envFrom that should import all keys from the secret
		if secretRefEnvs[secretName] {
			for key, value := range secret.Data {
				envVars = append(envVars, RunPodEnv{
					Key:   key,
					Value: strings.ReplaceAll(string(value), "\n", "\\n"),
				})
			}
		}
	}

	return envVars, nil
}

// PrepareRunPodParameters prepares parameters for RunPod deployment
// Update PrepareRunPodParameters to use the clientset from the Client struct
func (c *Client) PrepareRunPodParameters(pod *v1.Pod, graphql bool) (map[string]interface{}, error) {
	// Check if pod is owned by a job and use job annotations if available
	var ownerJob *batchv1.Job
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			// Fetch the job to access its annotations using the clientset
			job, err := c.clientset.BatchV1().Jobs(pod.Namespace).Get(
				context.Background(),
				owner.Name,
				metav1.GetOptions{},
			)
			if err == nil {
				ownerJob = job
				break
			}
		}
	}

	// Determine cloud type - default to SECURE but allow override via annotation
	cloudType := "SECURE"
	if cloudTypeVal := getAnnotation(RunpodCloudTypeAnnotation, ""); cloudTypeVal != "" {
		// Validate and normalize the cloud type value
		cloudTypeUpperCase := strings.ToUpper(cloudTypeVal)
		if cloudTypeUpperCase == "SECURE" || cloudTypeUpperCase == "COMMUNITY" {
			cloudType = cloudTypeUpperCase
		} else {
			c.logger.Warn("Invalid cloud type specified, using default",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"specifiedValue", cloudTypeVal,
				"defaultValue", cloudType)
		}
	}

	// Helper function to get annotation value with fallback to job annotations
	getAnnotation := func(annotationKey string, defaultValue string) string {
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

	// Extract container registry auth ID if provided
	containerRegistryAuthId := getAnnotation(RunpodContainerRegistryAuthAnnotation, "")

	// Extract template ID if provided
	templateId := getAnnotation(RunpodTemplateIdAnnotation, "")

	// Determine minimum GPU memory required
	minRAMPerGPU := 16 // Default minimum memory
	if memStr := getAnnotation(GpuMemoryAnnotation, ""); memStr != "" {
		if mem, err := strconv.Atoi(memStr); err == nil {
			minRAMPerGPU = mem
		}
	}

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

	var formattedEnvVars interface{}
	if graphql {
		formattedEnvVars = FormatEnvVarsForGraphQL(envVars)
	} else {
		formattedEnvVars = FormatEnvVarsForREST(envVars)
	}

	// Determine image name from pod
	var imageName string
	if len(pod.Spec.Containers) > 0 {
		imageName = pod.Spec.Containers[0].Image
	} else {
		return nil, fmt.Errorf("pod has no containers")
	}

	// Use the pod name as the RunPod name
	runpodName := pod.Name

	// Default values
	volumeInGb := 0
	containerDiskInGb := 15

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

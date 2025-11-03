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
	"strings"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
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
	DefaultMaxPrice   = 0.5
	DefaultAPITimeout = 30 * time.Second
)

// PodStatus represents the status of a RunPod instance
type PodStatus string

const (
	PodRunning     PodStatus = "RUNNING"
	PodStarting    PodStatus = "STARTING"
	PodTerminating PodStatus = "TERMINATING"
	PodTerminated  PodStatus = "TERMINATED"
	PodNotFound    PodStatus = "NOT_FOUND"
	PodExited      PodStatus = "EXITED"
)

// GPUType represents the GPU type from RunPod API
type GPUType struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"displayName"`
	MemoryInGb     int     `json:"memoryInGb"`
	SecureCloud    bool    `json:"secureCloud"`
	SecurePrice    float64 `json:"securePrice"`
	CommunityCloud bool    `json:"communityCloud"`
	CommunityPrice float64 `json:"communityPrice"`
}

// DetailedStatus represents detailed pod status from RunPod
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

// RuntimeInfo represents runtime information from RunPod
type RuntimeInfo struct {
	Container struct {
		ExitCode int    `json:"exitCode,omitempty"`
		Message  string `json:"message,omitempty"`
	} `json:"container,omitempty"`
	PodCompletionStatus string `json:"podCompletionStatus,omitempty"`
}

// MachineInfo represents machine information from RunPod
type MachineInfo struct {
	GPUTypeID    string `json:"gpuTypeId"`
	Location     string `json:"location"`
	DataCenterID string `json:"dataCenterId"`
}

// NewClient creates a new RunPod API client
func NewClient(logger *slog.Logger) *Client {
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
	}
}

// GetName returns the provider name
func (c *Client) GetName() string {
	return "runpod"
}

// Deploy creates a new GPU instance on RunPod
func (c *Client) Deploy(ctx context.Context, params *websocket.DeployParams) (*websocket.DeployResult, error) {
	// Get API key from params (platform-managed) or fall back to local environment
	apiKey := c.getAPIKey(params.APIKey)
	if apiKey == "" {
		return nil, providers.NewProviderError("runpod", "missing_api_key",
			"no API key provided and RUNPOD_API_KEY not set", false)
	}

	// Convert parameters to RunPod format
	runpodParams := c.convertDeployParams(params)

	// Deploy using REST API with the API key
	podID, costPerHour, err := c.deployPodRESTWithKey(runpodParams, apiKey)
	if err != nil {
		return nil, providers.NewProviderError("runpod", "deployment_failed", err.Error(), true)
	}

	result := &websocket.DeployResult{
		ProviderPodID: podID,
		CostPerHour:   costPerHour,
		Status:        string(PodStarting),
	}

	return result, nil
}

// GetStatus retrieves the current status of a RunPod instance
func (c *Client) GetStatus(ctx context.Context, params *websocket.StatusParams) (*websocket.StatusResult, error) {
	// Get API key from params (platform-managed) or fall back to local environment
	apiKey := c.getAPIKey(params.APIKey)
	if apiKey == "" {
		return nil, providers.NewProviderError("runpod", "missing_api_key",
			"no API key provided and RUNPOD_API_KEY not set", false)
	}

	status, err := c.getDetailedPodStatusWithKey(params.ProviderPodID, apiKey)
	if err != nil {
		return nil, providers.NewProviderError("runpod", "status_check_failed", err.Error(), true)
	}

	if status == nil {
		return &websocket.StatusResult{
			Status:       string(PodNotFound),
			IsRunning:    false,
			IsTerminated: true,
			IsSuccessful: false,
			LastUpdated:  time.Now(),
		}, nil
	}

	result := &websocket.StatusResult{
		Status:       providers.GetStandardStatus(status.DesiredStatus, "runpod"),
		LastUpdated:  time.Now(),
		IsRunning:    status.DesiredStatus == string(PodRunning),
		IsTerminated: status.DesiredStatus == string(PodTerminated) || status.DesiredStatus == string(PodExited),
		IsSuccessful: c.isSuccessfulCompletion(status),
	}

	if status.Runtime != nil {
		result.ExitCode = status.Runtime.Container.ExitCode
		result.Message = status.Runtime.Container.Message
		if result.Message == "" {
			result.Message = status.LastError
		}
	}

	return result, nil
}

// Terminate stops and removes a RunPod instance
func (c *Client) Terminate(ctx context.Context, params *websocket.TerminateParams) error {
	// Get API key from params (platform-managed) or fall back to local environment
	apiKey := c.getAPIKey(params.APIKey)
	if apiKey == "" {
		return providers.NewProviderError("runpod", "missing_api_key",
			"no API key provided and RUNPOD_API_KEY not set", false)
	}

	err := c.terminatePodWithKey(params.ProviderPodID, apiKey)
	if err != nil {
		return providers.NewProviderError("runpod", "termination_failed", err.Error(), true)
	}
	return nil
}

// NOTE: GetPricing and GetAvailability methods have been removed.
// These represent routing logic that belongs in the platform, not the kubelet.
// Platform services should maintain their own RunPod clients for pricing/availability queries.

// Ping tests connectivity to RunPod API
func (c *Client) Ping(ctx context.Context) error {
	if c.apiKey == "" {
		return providers.NewProviderError("runpod", "auth_failed", "API key not configured", false)
	}

	// Try a simple GraphQL query to test connectivity
	query := `query { gpuTypes { id } }`
	var response struct {
		Data struct {
			GPUTypes []struct {
				ID string `json:"id"`
			} `json:"gpuTypes"`
		} `json:"data"`
	}

	err := c.executeGraphQL(query, nil, &response)
	if err != nil {
		return providers.NewProviderError("runpod", "connectivity_failed", err.Error(), true)
	}

	return nil
}

// Private helper methods

func (c *Client) convertDeployParams(params *websocket.DeployParams) map[string]interface{} {
	runpodParams := map[string]interface{}{
		"name":              params.Name,
		"imageName":         params.Image,
		"containerDiskInGb": params.ContainerDiskInGb,
		"volumeInGb":        params.VolumeInGb,
		"env":               params.Env,
	}

	if len(params.GPUTypeIDs) > 0 {
		runpodParams["gpuTypeIds"] = params.GPUTypeIDs
	}

	if len(params.Ports) > 0 {
		runpodParams["ports"] = params.Ports
	}

	if len(params.DatacenterIDs) > 0 {
		runpodParams["dataCenterIds"] = params.DatacenterIDs
	}

	if params.CloudType != "" {
		runpodParams["cloudType"] = params.CloudType
	}

	if params.TemplateID != "" {
		runpodParams["templateId"] = params.TemplateID
	}

	if params.ContainerAuthID != "" {
		runpodParams["containerRegistryAuthId"] = params.ContainerAuthID
	}

	if params.MinRAMPerGPU > 0 {
		runpodParams["minRAMPerGPU"] = params.MinRAMPerGPU
	}

	return runpodParams
}

func (c *Client) executeGraphQL(query string, variables map[string]interface{}, response interface{}) error {
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

func (c *Client) getGPUTypes(minRAMPerGPU int, maxPrice float64, cloudType string) ([]GPUType, error) {
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

	if err := c.executeGraphQL(query, nil, &response); err != nil {
		return nil, err
	}

	// Filter GPUs based on criteria and cloud type
	var filteredGPUs []GPUType
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

		if cloudCheck && price > 0 && price < maxPrice && gpu.MemoryInGb >= minRAMPerGPU {
			filteredGPUs = append(filteredGPUs, gpu)
		}
	}

	// Sort by price ascending
	sort.Slice(filteredGPUs, func(i, j int) bool {
		priceI := filteredGPUs[i].SecurePrice
		priceJ := filteredGPUs[j].SecurePrice
		if cloudType == "COMMUNITY" {
			priceI = filteredGPUs[i].CommunityPrice
			priceJ = filteredGPUs[j].CommunityPrice
		}
		return priceI < priceJ
	})

	return filteredGPUs, nil
}

func (c *Client) deployPodREST(params map[string]interface{}) (string, float64, error) {
	return c.deployPodRESTWithKey(params, c.apiKey)
}

func (c *Client) deployPodRESTWithKey(params map[string]interface{}, apiKey string) (string, float64, error) {
	reqBody, err := json.Marshal(params)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.makeRESTRequestWithKey("POST", "pods", bytes.NewBuffer(reqBody), apiKey)
	if err != nil {
		return "", 0, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", 0, fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		ID        string  `json:"id"`
		CostPerHr float64 `json:"costPerHr"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.ID == "" {
		return "", 0, fmt.Errorf("pod deployment failed: %s", string(body))
	}

	return response.ID, response.CostPerHr, nil
}

func (c *Client) getDetailedPodStatus(podID string) (*DetailedStatus, error) {
	return c.getDetailedPodStatusWithKey(podID, c.apiKey)
}

func (c *Client) getDetailedPodStatusWithKey(podID string, apiKey string) (*DetailedStatus, error) {
	endpoint := fmt.Sprintf("pods/%s", podID)

	resp, err := c.makeRESTRequestWithKey("GET", endpoint, nil, apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod details: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // Pod not found
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	var status DetailedStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to parse pod status: %w", err)
	}

	return &status, nil
}

func (c *Client) terminatePod(podID string) error {
	return c.terminatePodWithKey(podID, c.apiKey)
}

func (c *Client) terminatePodWithKey(podID string, apiKey string) error {
	endpoint := fmt.Sprintf("pods/%s/stop", podID)

	resp, err := c.makeRESTRequestWithKey("POST", endpoint, nil, apiKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to terminate pod, status: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) makeRESTRequest(method, endpoint string, body io.Reader) (*http.Response, error) {
	return c.makeRESTRequestWithKey(method, endpoint, body, c.apiKey)
}

func (c *Client) makeRESTRequestWithKey(method, endpoint string, body io.Reader, apiKey string) (*http.Response, error) {
	url := fmt.Sprintf("%s%s", c.baseRESTURL, endpoint)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	timeout := DefaultAPITimeout
	if method == "POST" && endpoint == "pods" {
		timeout = 60 * time.Second // Longer timeout for pod deployment
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	return resp, nil
}

func (c *Client) isSuccessfulCompletion(status *DetailedStatus) bool {
	if status.Runtime == nil {
		return false
	}

	exitCode := status.Runtime.Container.ExitCode
	if exitCode == 0 {
		return true
	}

	completionStatus := status.Runtime.PodCompletionStatus
	if completionStatus != "" {
		return strings.Contains(strings.ToLower(completionStatus), "success") ||
			strings.Contains(strings.ToLower(completionStatus), "completed")
	}

	return false
}

// getAPIKey returns the API key from params if provided, otherwise falls back to client's configured key or environment
func (c *Client) getAPIKey(paramKey string) string {
	// Use key from command params if provided (SaaS mode - platform-managed)
	if paramKey != "" {
		return paramKey
	}

	// Fall back to client's configured key (from environment at initialization)
	if c.apiKey != "" {
		return c.apiKey
	}

	// Last resort: check environment variable directly
	return os.Getenv("RUNPOD_API_KEY")
}
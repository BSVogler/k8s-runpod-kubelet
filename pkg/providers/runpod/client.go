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
	ID                      string            `json:"id"`
	Name                    string            `json:"name"`
	DesiredStatus           string            `json:"desiredStatus"`
	CurrentStatus           string            `json:"currentStatus,omitempty"`
	CostPerHr               float64           `json:"costPerHr"`
	Image                   string            `json:"image"`
	Env                     map[string]string `json:"env"`
	MachineID               string            `json:"machineId"`
	PortMappings            map[string]int    `json:"portMappings"`
	Runtime                 *RuntimeInfo      `json:"runtime,omitempty"`
	Machine                 *MachineInfo      `json:"machine,omitempty"`
	LastError               string            `json:"lastError,omitempty"`
	ContainerRegistryAuthId string            `json:"containerRegistryAuthId,omitempty"`
	TemplateId              string            `json:"templateId,omitempty"`
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
	created, err := c.deployPodRESTWithKey(runpodParams, apiKey)
	if err != nil {
		return nil, providers.NewProviderError("runpod", "deployment_failed", err.Error(), true)
	}

	status := created.DesiredStatus
	if status == "" {
		status = string(PodStarting)
	}

	result := &websocket.DeployResult{
		ProviderPodID: created.ID,
		Provider:      c.GetName(),
		Status:        providers.GetStandardStatus(status, "runpod"),
		CostPerHour:   created.CostPerHr,
		MachineID:     created.MachineID,
		Ports:         proxyPortURLs(created.ID, params.Ports),
	}
	if created.Machine != nil {
		result.GPUType = created.Machine.GPUTypeID
		result.DatacenterID = created.Machine.DataCenterID
	}

	return result, nil
}

// proxyPortURLs builds the external URL map for HTTP ports exposed through the
// RunPod proxy (https://{podId}-{port}.proxy.runpod.net). TCP ports are only
// reachable once the pod reports its public IP and are left out.
func proxyPortURLs(podID string, ports []string) map[string]string {
	if podID == "" || len(ports) == 0 {
		return nil
	}
	urls := make(map[string]string)
	for _, spec := range ports {
		port, proto, _ := strings.Cut(spec, "/")
		port = strings.TrimSpace(port)
		if port == "" || !strings.EqualFold(strings.TrimSpace(proto), "http") {
			continue
		}
		urls[port] = fmt.Sprintf("https://%s-%s.proxy.runpod.net", podID, port)
	}
	if len(urls) == 0 {
		return nil
	}
	return urls
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
			ProviderPodID: params.ProviderPodID,
			Provider:      c.GetName(),
			Status:        string(PodNotFound),
			Phase:         providers.PhaseFailed,
			IsRunning:     false,
			IsTerminated:  true,
			Message:       "pod not found on RunPod",
		}, nil
	}

	standardStatus := providers.GetStandardStatus(status.DesiredStatus, "runpod")
	successful := c.isSuccessfulCompletion(status)

	result := &websocket.StatusResult{
		ProviderPodID: params.ProviderPodID,
		Provider:      c.GetName(),
		Status:        standardStatus,
		Phase:         providers.PhaseForStatus(standardStatus, successful),
		IsRunning:     status.DesiredStatus == string(PodRunning),
		IsTerminated:  status.DesiredStatus == string(PodTerminated) || status.DesiredStatus == string(PodExited),
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

	applyContainerSpec(runpodParams, params)

	return runpodParams
}

// applyContainerSpec maps command, args and GPU count onto RunPod REST parameters.
// K8s command overrides the image ENTRYPOINT and args override CMD, matching RunPod's
// dockerEntrypoint and dockerStartCmd. Unset fields keep the image defaults; a GPU
// count of 0 is omitted so RunPod applies its default of one.
func applyContainerSpec(runpodParams map[string]interface{}, params *websocket.DeployParams) {
	if len(params.Command) > 0 {
		runpodParams["dockerEntrypoint"] = params.Command
	}
	if len(params.Args) > 0 {
		runpodParams["dockerStartCmd"] = params.Args
	}
	if params.GPUCount > 0 {
		runpodParams["gpuCount"] = params.GPUCount
	}
}

// createPodResponse is the subset of the RunPod REST create-pod response we use.
type createPodResponse struct {
	ID            string       `json:"id"`
	CostPerHr     float64      `json:"costPerHr"`
	DesiredStatus string       `json:"desiredStatus"`
	MachineID     string       `json:"machineId"`
	Machine       *MachineInfo `json:"machine,omitempty"`
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

func (c *Client) deployPodREST(params map[string]interface{}) (*createPodResponse, error) {
	return c.deployPodRESTWithKey(params, c.apiKey)
}

func (c *Client) deployPodRESTWithKey(params map[string]interface{}, apiKey string) (*createPodResponse, error) {
	reqBody, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.makeRESTRequestWithKey("POST", "pods", bytes.NewBuffer(reqBody), apiKey)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API returned error: %d %s", resp.StatusCode, string(body))
	}

	var response createPodResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.ID == "" {
		return nil, fmt.Errorf("pod deployment failed: %s", string(body))
	}

	return &response, nil
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

// terminatePodWithKey deletes the pod. DELETE is used instead of /stop because a
// stopped pod stays in the account and keeps billing for its disk.
func (c *Client) terminatePodWithKey(podID string, apiKey string) error {
	if strings.TrimSpace(podID) == "" {
		return fmt.Errorf("invalid pod ID: %q", podID)
	}

	endpoint := fmt.Sprintf("pods/%s", podID)

	resp, err := c.makeRESTRequestWithKey("DELETE", endpoint, nil, apiKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Already gone counts as terminated
	if resp.StatusCode == http.StatusNotFound {
		c.logger.Info("RunPod instance already deleted", "provider_pod_id", podID)
		return nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
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

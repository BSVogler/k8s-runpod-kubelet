package providers

import (
	"context"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

// Provider defines the interface that all cloud GPU providers must implement
type Provider interface {
	// GetName returns the provider name (e.g., "runpod", "vastai")
	GetName() string

	// Deploy creates a new GPU instance based on the provided parameters
	Deploy(ctx context.Context, params *websocket.DeployParams) (*websocket.DeployResult, error)

	// GetStatus retrieves the current status of a deployed instance
	GetStatus(ctx context.Context, providerPodID string) (*websocket.StatusResult, error)

	// Terminate stops and removes a deployed instance
	Terminate(ctx context.Context, providerPodID string) error

	// GetPricing retrieves current pricing information for available GPU types
	GetPricing(ctx context.Context) (*PricingResult, error)

	// GetAvailability checks which GPU types are currently available
	GetAvailability(ctx context.Context, requirements *AvailabilityQuery) (*AvailabilityResult, error)

	// Ping tests connectivity to the provider's API
	Ping(ctx context.Context) error
}

// PricingResult contains pricing information from a provider
type PricingResult struct {
	Provider  string     `json:"provider"`
	Currency  string     `json:"currency"`
	Timestamp time.Time  `json:"timestamp"`
	GPUTypes  []GPUPrice `json:"gpu_types"`
}

// GPUPrice represents the price of a specific GPU type
type GPUPrice struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	MemoryGB     int     `json:"memory_gb"`
	PricePerHour float64 `json:"price_per_hour"`
	Available    bool    `json:"available"`
	CloudType    string  `json:"cloud_type,omitempty"` // For providers that have different cloud types
}

// AvailabilityQuery represents requirements for checking availability
type AvailabilityQuery struct {
	MinMemoryGB   int      `json:"min_memory_gb"`
	MaxPricePerHr float64  `json:"max_price_per_hr"`
	DatacenterIDs []string `json:"datacenter_ids,omitempty"`
	CloudType     string   `json:"cloud_type,omitempty"`
}

// AvailabilityResult contains availability information
type AvailabilityResult struct {
	Provider       string             `json:"provider"`
	Timestamp      time.Time          `json:"timestamp"`
	AvailableGPUs  []AvailableGPU     `json:"available_gpus"`
	Datacenters    []DatacenterInfo   `json:"datacenters,omitempty"`
}

// AvailableGPU represents an available GPU instance
type AvailableGPU struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	MemoryGB     int     `json:"memory_gb"`
	PricePerHour float64 `json:"price_per_hour"`
	DatacenterID string  `json:"datacenter_id,omitempty"`
	CloudType    string  `json:"cloud_type,omitempty"`
	Count        int     `json:"count"` // Number of available instances
}

// DatacenterInfo represents information about a datacenter
type DatacenterInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country,omitempty"`
}

// ProviderError represents an error from a cloud provider
type ProviderError struct {
	Provider string `json:"provider"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
	Retry    bool   `json:"retry"` // Whether the operation should be retried
}

func (e *ProviderError) Error() string {
	if e.Code != "" {
		return e.Provider + " error " + e.Code + ": " + e.Message
	}
	return e.Provider + " error: " + e.Message
}

// NewProviderError creates a new provider error
func NewProviderError(provider, code, message string, retry bool) *ProviderError {
	return &ProviderError{
		Provider: provider,
		Code:     code,
		Message:  message,
		Retry:    retry,
	}
}

// StatusMapping maps provider-specific statuses to standardized statuses
type StatusMapping struct {
	ProviderStatus string
	StandardStatus string
	IsRunning      bool
	IsTerminated   bool
	IsSuccessful   bool
}

// Common status constants that providers should map to
const (
	StatusPending     = "PENDING"
	StatusStarting    = "STARTING"
	StatusRunning     = "RUNNING"
	StatusTerminating = "TERMINATING"
	StatusTerminated  = "TERMINATED"
	StatusFailed      = "FAILED"
	StatusExited      = "EXITED"
	StatusUnknown     = "UNKNOWN"
)

// GetStandardStatus returns a standardized status from provider-specific status
func GetStandardStatus(providerStatus string, provider string) string {
	// This can be expanded to handle provider-specific mappings
	switch provider {
	case "runpod":
		return mapRunPodStatus(providerStatus)
	case "vastai":
		return mapVastAIStatus(providerStatus)
	default:
		return providerStatus
	}
}

// mapRunPodStatus maps RunPod statuses to standard statuses
func mapRunPodStatus(status string) string {
	switch status {
	case "STARTING":
		return StatusStarting
	case "RUNNING":
		return StatusRunning
	case "TERMINATING":
		return StatusTerminating
	case "TERMINATED":
		return StatusTerminated
	case "EXITED":
		return StatusExited
	default:
		return status
	}
}

// mapVastAIStatus maps Vast.ai statuses to standard statuses
func mapVastAIStatus(status string) string {
	switch status {
	case "loading":
		return StatusStarting
	case "running":
		return StatusRunning
	case "exited":
		return StatusExited
	default:
		return status
	}
}
package providers

import (
	"context"

	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

// Provider defines the interface that all cloud GPU providers must implement.
//
// NOTE: This interface has been simplified for SaaS platform architecture.
// Pricing and availability queries have been removed as they represent routing logic
// that belongs in the platform, not the kubelet. The kubelet is a pure command executor.
//
// All methods now accept params objects that may contain platform-managed API keys.
type Provider interface {
	// GetName returns the provider name (e.g., "runpod", "vastai")
	GetName() string

	// Deploy creates a new GPU instance based on the provided parameters
	Deploy(ctx context.Context, params *websocket.DeployParams) (*websocket.DeployResult, error)

	// GetStatus retrieves the current status of a deployed instance
	GetStatus(ctx context.Context, params *websocket.StatusParams) (*websocket.StatusResult, error)

	// Terminate stops and removes a deployed instance
	Terminate(ctx context.Context, params *websocket.TerminateParams) error

	// Ping tests connectivity to the provider's API
	Ping(ctx context.Context) error
}

// NOTE: Pricing and availability types have been removed.
// These represent routing/intelligence logic that belongs in the platform, not the kubelet.
// Platform services should maintain their own provider clients for pricing and availability queries.

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
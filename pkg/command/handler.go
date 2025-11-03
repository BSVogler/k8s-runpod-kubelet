package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

// Handler processes WebSocket commands from the backend
type Handler struct {
	providerManager *providers.Manager
	logger          *slog.Logger
}

// NewHandler creates a new command handler
func NewHandler(providerManager *providers.Manager, logger *slog.Logger) *Handler {
	return &Handler{
		providerManager: providerManager,
		logger:          logger,
	}
}

// HandleCommand processes a command message and returns a response
func (h *Handler) HandleCommand(ctx context.Context, msg *websocket.Message) *websocket.Response {
	h.logger.Debug("Processing command", "type", msg.Type, "id", msg.ID)

	cmd, err := msg.ParseCommand()
	if err != nil {
		h.logger.Error("Failed to parse command", "error", err, "message_id", msg.ID)
		return &websocket.Response{
			CommandID: msg.ID,
			Success:   false,
			Error:     fmt.Sprintf("Failed to parse command: %v", err),
		}
	}

	switch msg.Type {
	case websocket.CommandDeploy:
		return h.handleDeploy(ctx, cmd)
	case websocket.CommandTerminate:
		return h.handleTerminate(ctx, cmd)
	case websocket.CommandStatus:
		return h.handleStatus(ctx, cmd)
	case websocket.CommandPing:
		return h.handlePing(ctx, cmd)
	default:
		h.logger.Warn("Unknown command type", "type", msg.Type, "id", msg.ID)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     fmt.Sprintf("Unknown command type: %s", msg.Type),
		}
	}
}

// handleDeploy processes a deploy command
func (h *Handler) handleDeploy(ctx context.Context, cmd *websocket.Command) *websocket.Response {
	h.logger.Info("Handling deploy command",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID)

	// Parse deploy parameters
	params, err := h.parseDeployParams(cmd.Params)
	if err != nil {
		h.logger.Error("Failed to parse deploy parameters",
			"command_id", cmd.ID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     fmt.Sprintf("Failed to parse deploy parameters: %v", err),
		}
	}

	// Execute deployment
	result, err := h.providerManager.Deploy(ctx, string(cmd.Provider), params)
	if err != nil {
		h.logger.Error("Deployment failed",
			"command_id", cmd.ID,
			"provider", cmd.Provider,
			"pod_id", cmd.PodID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     err.Error(),
		}
	}

	h.logger.Info("Deployment successful",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID,
		"provider_pod_id", result.ProviderPodID,
		"cost_per_hour", result.CostPerHour)

	return &websocket.Response{
		CommandID: cmd.ID,
		Success:   true,
		Result:    result,
	}
}

// handleTerminate processes a terminate command
func (h *Handler) handleTerminate(ctx context.Context, cmd *websocket.Command) *websocket.Response {
	h.logger.Info("Handling terminate command",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID)

	// Parse terminate parameters
	params, err := h.parseTerminateParams(cmd.Params)
	if err != nil {
		h.logger.Error("Failed to parse terminate parameters",
			"command_id", cmd.ID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     fmt.Sprintf("Failed to parse terminate parameters: %v", err),
		}
	}

	// Execute termination
	err = h.providerManager.Terminate(ctx, string(cmd.Provider), params)
	if err != nil {
		h.logger.Error("Termination failed",
			"command_id", cmd.ID,
			"provider", cmd.Provider,
			"pod_id", cmd.PodID,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     err.Error(),
		}
	}

	h.logger.Info("Termination successful",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID,
		"provider_pod_id", params.ProviderPodID)

	return &websocket.Response{
		CommandID: cmd.ID,
		Success:   true,
		Result: map[string]interface{}{
			"terminated": true,
		},
	}
}

// handleStatus processes a status command
func (h *Handler) handleStatus(ctx context.Context, cmd *websocket.Command) *websocket.Response {
	h.logger.Debug("Handling status command",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID)

	// Parse status parameters
	params, err := h.parseStatusParams(cmd.Params)
	if err != nil {
		h.logger.Error("Failed to parse status parameters",
			"command_id", cmd.ID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     fmt.Sprintf("Failed to parse status parameters: %v", err),
		}
	}

	// Get status
	result, err := h.providerManager.GetStatus(ctx, string(cmd.Provider), params)
	if err != nil {
		h.logger.Error("Status check failed",
			"command_id", cmd.ID,
			"provider", cmd.Provider,
			"pod_id", cmd.PodID,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   false,
			Error:     err.Error(),
		}
	}

	h.logger.Debug("Status check successful",
		"command_id", cmd.ID,
		"provider", cmd.Provider,
		"pod_id", cmd.PodID,
		"provider_pod_id", params.ProviderPodID,
		"status", result.Status)

	return &websocket.Response{
		CommandID: cmd.ID,
		Success:   true,
		Result:    result,
	}
}

// handlePing processes a ping command
func (h *Handler) handlePing(ctx context.Context, cmd *websocket.Command) *websocket.Response {
	h.logger.Debug("Handling ping command", "command_id", cmd.ID)

	// Test connectivity to all providers or specific provider
	if cmd.Provider != "" {
		// Test specific provider
		provider, err := h.providerManager.GetProvider(string(cmd.Provider))
		if err != nil {
			return &websocket.Response{
				CommandID: cmd.ID,
				Success:   false,
				Error:     err.Error(),
			}
		}

		err = provider.Ping(ctx)
		if err != nil {
			return &websocket.Response{
				CommandID: cmd.ID,
				Success:   false,
				Error:     err.Error(),
			}
		}

		return &websocket.Response{
			CommandID: cmd.ID,
			Success:   true,
			Result: map[string]interface{}{
				"provider": cmd.Provider,
				"status":   "healthy",
				"timestamp": time.Now(),
			},
		}
	}

	// Test all providers
	healthStatus := h.providerManager.HealthCheck(ctx)

	return &websocket.Response{
		CommandID: cmd.ID,
		Success:   true,
		Result: map[string]interface{}{
			"providers": healthStatus,
			"timestamp": time.Now(),
		},
	}
}

// parseDeployParams parses deploy parameters from command data
func (h *Handler) parseDeployParams(params interface{}) (*websocket.DeployParams, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var deployParams websocket.DeployParams
	err = json.Unmarshal(data, &deployParams)
	return &deployParams, err
}

// parseTerminateParams parses terminate parameters from command data
func (h *Handler) parseTerminateParams(params interface{}) (*websocket.TerminateParams, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var terminateParams websocket.TerminateParams
	err = json.Unmarshal(data, &terminateParams)
	return &terminateParams, err
}

// parseStatusParams parses status parameters from command data
func (h *Handler) parseStatusParams(params interface{}) (*websocket.StatusParams, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var statusParams websocket.StatusParams
	err = json.Unmarshal(data, &statusParams)
	return &statusParams, err
}

// GetProviderManager returns the provider manager (for testing)
func (h *Handler) GetProviderManager() *providers.Manager {
	return h.providerManager
}

// GetHealthStatus returns the health status of all providers
func (h *Handler) GetHealthStatus(ctx context.Context) map[string]bool {
	return h.providerManager.HealthCheck(ctx)
}

// GetAvailableProviders returns a list of available provider names
func (h *Handler) GetAvailableProviders() []string {
	return h.providerManager.ListProviders()
}

// ExecuteCommand is a convenience method that handles the full command processing pipeline
func (h *Handler) ExecuteCommand(ctx context.Context, msg *websocket.Message) *websocket.Response {
	startTime := time.Now()

	response := h.HandleCommand(ctx, msg)

	duration := time.Since(startTime)
	h.logger.Debug("Command execution completed",
		"command_id", msg.ID,
		"command_type", msg.Type,
		"success", response.Success,
		"duration_ms", duration.Milliseconds())

	return response
}
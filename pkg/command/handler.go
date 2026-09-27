package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

// Error codes used in error responses.
const (
	ErrCodeInvalidCommand    = "invalid_command"
	ErrCodeInvalidParams     = "invalid_params"
	ErrCodeUnknownCommand    = "unknown_command"
	ErrCodeDeploymentFailed  = "deployment_failed"
	ErrCodeTerminationFailed = "termination_failed"
	ErrCodeStatusFailed      = "status_failed"
	ErrCodeRejectFailed      = "reject_failed"
)

// RejectFunc is invoked when the platform rejects a pod. The kubelet marks
// the Kubernetes pod as Failed with reason=code and the given message.
type RejectFunc func(ctx context.Context, params *websocket.RejectParams) error

// Handler processes WebSocket commands from the backend
type Handler struct {
	providerManager *providers.Manager
	logger          *slog.Logger
	rejectFunc      RejectFunc
}

// NewHandler creates a new command handler
func NewHandler(providerManager *providers.Manager, logger *slog.Logger) *Handler {
	return &Handler{
		providerManager: providerManager,
		logger:          logger,
	}
}

// SetRejectHandler registers the callback for "reject" commands.
func (h *Handler) SetRejectHandler(fn RejectFunc) {
	h.rejectFunc = fn
}

// HandleCommand decodes a "command" envelope and returns the response.
func (h *Handler) HandleCommand(ctx context.Context, env *websocket.Envelope) *websocket.Response {
	var cmd websocket.Command
	if err := env.DecodePayload(&cmd); err != nil {
		h.logger.Error("Failed to parse command", "error", err, "command_id", env.ID)
		return websocket.NewErrorResponse(env.ID, ErrCodeInvalidCommand,
			fmt.Sprintf("failed to parse command: %v", err), "")
	}

	return h.Handle(ctx, env.ID, &cmd)
}

// Handle dispatches an already decoded command.
func (h *Handler) Handle(ctx context.Context, commandID string, cmd *websocket.Command) *websocket.Response {
	h.logger.Debug("Processing command", "type", cmd.Type, "command_id", commandID)

	switch cmd.Type {
	case websocket.CommandDeploy:
		return h.handleDeploy(ctx, commandID, cmd)
	case websocket.CommandTerminate:
		return h.handleTerminate(ctx, commandID, cmd)
	case websocket.CommandStatus:
		return h.handleStatus(ctx, commandID, cmd)
	case websocket.CommandPing:
		return h.handlePing(ctx, commandID)
	case websocket.CommandReject:
		return h.handleReject(ctx, commandID, cmd)
	default:
		h.logger.Warn("Unknown command type", "type", cmd.Type, "command_id", commandID)
		return websocket.NewErrorResponse(commandID, ErrCodeUnknownCommand,
			fmt.Sprintf("unknown command type: %s", cmd.Type), "")
	}
}

// handleDeploy processes a deploy command
func (h *Handler) handleDeploy(ctx context.Context, commandID string, cmd *websocket.Command) *websocket.Response {
	var params websocket.DeployParams
	if err := cmd.DecodeData(&params); err != nil {
		h.logger.Error("Failed to parse deploy parameters", "command_id", commandID, "error", err)
		return websocket.NewErrorResponse(commandID, ErrCodeInvalidParams,
			fmt.Sprintf("failed to parse deploy parameters: %v", err), "")
	}

	h.logger.Info("Handling deploy command",
		"command_id", commandID,
		"provider", params.Provider,
		"pod", websocket.PodKey(params.Namespace, params.PodName),
		"image", params.Image)

	result, err := h.providerManager.Deploy(ctx, params.Provider, &params)
	if err != nil {
		h.logger.Error("Deployment failed",
			"command_id", commandID,
			"provider", params.Provider,
			"pod", websocket.PodKey(params.Namespace, params.PodName),
			"error", err)
		return errorResponse(commandID, ErrCodeDeploymentFailed, err)
	}

	if result.Provider == "" {
		result.Provider = params.Provider
	}

	h.logger.Info("Deployment successful",
		"command_id", commandID,
		"provider", params.Provider,
		"pod", websocket.PodKey(params.Namespace, params.PodName),
		"provider_pod_id", result.ProviderPodID,
		"cost_per_hour", result.CostPerHour)

	return websocket.NewSuccessResponse(commandID, result)
}

// handleTerminate processes a terminate command
func (h *Handler) handleTerminate(ctx context.Context, commandID string, cmd *websocket.Command) *websocket.Response {
	var params websocket.TerminateParams
	if err := cmd.DecodeData(&params); err != nil {
		h.logger.Error("Failed to parse terminate parameters", "command_id", commandID, "error", err)
		return websocket.NewErrorResponse(commandID, ErrCodeInvalidParams,
			fmt.Sprintf("failed to parse terminate parameters: %v", err), "")
	}

	h.logger.Info("Handling terminate command",
		"command_id", commandID,
		"provider", params.Provider,
		"pod", websocket.PodKey(params.Namespace, params.PodName),
		"provider_pod_id", params.ProviderPodID)

	if err := h.providerManager.Terminate(ctx, params.Provider, &params); err != nil {
		h.logger.Error("Termination failed",
			"command_id", commandID,
			"provider", params.Provider,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return errorResponse(commandID, ErrCodeTerminationFailed, err)
	}

	h.logger.Info("Termination successful",
		"command_id", commandID,
		"provider", params.Provider,
		"provider_pod_id", params.ProviderPodID)

	return websocket.NewSuccessResponse(commandID, &websocket.TerminateResult{Terminated: true})
}

// handleStatus processes a status command
func (h *Handler) handleStatus(ctx context.Context, commandID string, cmd *websocket.Command) *websocket.Response {
	var params websocket.StatusParams
	if err := cmd.DecodeData(&params); err != nil {
		h.logger.Error("Failed to parse status parameters", "command_id", commandID, "error", err)
		return websocket.NewErrorResponse(commandID, ErrCodeInvalidParams,
			fmt.Sprintf("failed to parse status parameters: %v", err), "")
	}

	h.logger.Debug("Handling status command",
		"command_id", commandID,
		"provider", params.Provider,
		"provider_pod_id", params.ProviderPodID)

	result, err := h.providerManager.GetStatus(ctx, params.Provider, &params)
	if err != nil {
		h.logger.Error("Status check failed",
			"command_id", commandID,
			"provider", params.Provider,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return errorResponse(commandID, ErrCodeStatusFailed, err)
	}

	result.PodName = params.PodName
	result.ProviderPodID = params.ProviderPodID
	if result.Provider == "" {
		result.Provider = params.Provider
	}

	h.logger.Debug("Status check successful",
		"command_id", commandID,
		"provider", params.Provider,
		"provider_pod_id", params.ProviderPodID,
		"status", result.Status)

	return websocket.NewSuccessResponse(commandID, result)
}

// handlePing answers a ping command with an empty success response
func (h *Handler) handlePing(_ context.Context, commandID string) *websocket.Response {
	h.logger.Debug("Handling ping command", "command_id", commandID)
	return websocket.NewSuccessResponse(commandID, map[string]interface{}{})
}

// handleReject processes a reject command
func (h *Handler) handleReject(ctx context.Context, commandID string, cmd *websocket.Command) *websocket.Response {
	var params websocket.RejectParams
	if err := cmd.DecodeData(&params); err != nil {
		h.logger.Error("Failed to parse reject parameters", "command_id", commandID, "error", err)
		return websocket.NewErrorResponse(commandID, ErrCodeInvalidParams,
			fmt.Sprintf("failed to parse reject parameters: %v", err), "")
	}

	h.logger.Warn("Platform rejected pod",
		"command_id", commandID,
		"pod", websocket.PodKey(params.Namespace, params.PodName),
		"code", params.Code,
		"message", params.Message)

	if h.rejectFunc == nil {
		return websocket.NewErrorResponse(commandID, ErrCodeRejectFailed,
			"no reject handler configured", "")
	}

	if err := h.rejectFunc(ctx, &params); err != nil {
		h.logger.Error("Failed to apply rejection",
			"command_id", commandID,
			"pod", websocket.PodKey(params.Namespace, params.PodName),
			"error", err)
		return websocket.NewErrorResponse(commandID, ErrCodeRejectFailed, err.Error(), "")
	}

	return websocket.NewSuccessResponse(commandID, map[string]interface{}{})
}

// errorResponse builds an error response, surfacing provider error details when present.
func errorResponse(commandID, code string, err error) *websocket.Response {
	var providerErr *providers.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Code != "" {
			code = providerErr.Code
		}
		return websocket.NewErrorResponse(commandID, code, providerErr.Error(), providerErr.Message)
	}
	return websocket.NewErrorResponse(commandID, code, err.Error(), "")
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

// ExecuteCommand handles a command envelope and logs the execution time
func (h *Handler) ExecuteCommand(ctx context.Context, env *websocket.Envelope) *websocket.Response {
	startTime := time.Now()

	response := h.HandleCommand(ctx, env)

	h.logger.Debug("Command execution completed",
		"command_id", env.ID,
		"status", response.Status,
		"duration_ms", time.Since(startTime).Milliseconds())

	return response
}

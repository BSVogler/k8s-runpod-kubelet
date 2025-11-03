package providers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

// Manager manages multiple cloud providers and routes commands to the appropriate provider
type Manager struct {
	providers map[string]Provider
	logger    *slog.Logger
	mutex     sync.RWMutex
}

// NewManager creates a new provider manager
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{
		providers: make(map[string]Provider),
		logger:    logger,
	}
}

// RegisterProvider registers a new provider with the manager
func (m *Manager) RegisterProvider(provider Provider) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	name := provider.GetName()
	if _, exists := m.providers[name]; exists {
		return fmt.Errorf("provider %s is already registered", name)
	}

	m.providers[name] = provider
	m.logger.Info("Provider registered", "provider", name)
	return nil
}

// GetProvider returns a provider by name
func (m *Manager) GetProvider(name string) (Provider, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	provider, exists := m.providers[name]
	if !exists {
		return nil, fmt.Errorf("provider %s not found", name)
	}

	return provider, nil
}

// ListProviders returns a list of all registered provider names
func (m *Manager) ListProviders() []string {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var names []string
	for name := range m.providers {
		names = append(names, name)
	}
	return names
}

// Deploy executes a deployment command on the specified provider
func (m *Manager) Deploy(ctx context.Context, providerName string, params *websocket.DeployParams) (*websocket.DeployResult, error) {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	m.logger.Info("Deploying pod via provider",
		"provider", providerName,
		"pod_name", params.Name,
		"image", params.Image)

	result, err := provider.Deploy(ctx, params)
	if err != nil {
		m.logger.Error("Deployment failed",
			"provider", providerName,
			"pod_name", params.Name,
			"error", err)
		return nil, err
	}

	m.logger.Info("Deployment successful",
		"provider", providerName,
		"pod_name", params.Name,
		"provider_pod_id", result.ProviderPodID,
		"cost_per_hour", result.CostPerHour)

	return result, nil
}

// GetStatus retrieves the status of a pod from the specified provider
func (m *Manager) GetStatus(ctx context.Context, providerName string, params *websocket.StatusParams) (*websocket.StatusResult, error) {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	m.logger.Debug("Getting pod status",
		"provider", providerName,
		"provider_pod_id", params.ProviderPodID)

	result, err := provider.GetStatus(ctx, params)
	if err != nil {
		m.logger.Error("Failed to get pod status",
			"provider", providerName,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return nil, err
	}

	m.logger.Debug("Pod status retrieved",
		"provider", providerName,
		"provider_pod_id", params.ProviderPodID,
		"status", result.Status)

	return result, nil
}

// Terminate terminates a pod on the specified provider
func (m *Manager) Terminate(ctx context.Context, providerName string, params *websocket.TerminateParams) error {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return err
	}

	m.logger.Info("Terminating pod",
		"provider", providerName,
		"provider_pod_id", params.ProviderPodID)

	err = provider.Terminate(ctx, params)
	if err != nil {
		m.logger.Error("Failed to terminate pod",
			"provider", providerName,
			"provider_pod_id", params.ProviderPodID,
			"error", err)
		return err
	}

	m.logger.Info("Pod terminated successfully",
		"provider", providerName,
		"provider_pod_id", params.ProviderPodID)

	return nil
}

// Removed routing logic methods (GetPricing, GetAvailability, GetAllPricing, GetAllAvailability).
// These methods represented routing/intelligence logic that belongs in the platform, not the kubelet.
// Platform services should maintain their own provider clients for pricing and availability queries.

// PingAll tests connectivity to all registered providers
func (m *Manager) PingAll(ctx context.Context) map[string]error {
	m.mutex.RLock()
	providers := make(map[string]Provider)
	for name, provider := range m.providers {
		providers[name] = provider
	}
	m.mutex.RUnlock()

	results := make(map[string]error)
	var wg sync.WaitGroup
	var resultMutex sync.Mutex

	for name, provider := range providers {
		wg.Add(1)
		go func(name string, provider Provider) {
			defer wg.Done()

			err := provider.Ping(ctx)

			resultMutex.Lock()
			results[name] = err
			resultMutex.Unlock()

			if err != nil {
				m.logger.Warn("Provider ping failed",
					"provider", name,
					"error", err)
			} else {
				m.logger.Debug("Provider ping successful", "provider", name)
			}
		}(name, provider)
	}

	wg.Wait()
	return results
}

// HealthCheck returns the health status of all providers
func (m *Manager) HealthCheck(ctx context.Context) map[string]bool {
	pingResults := m.PingAll(ctx)
	healthStatus := make(map[string]bool)

	for provider, err := range pingResults {
		healthStatus[provider] = err == nil
	}

	return healthStatus
}
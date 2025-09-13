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
func (m *Manager) GetStatus(ctx context.Context, providerName string, providerPodID string) (*websocket.StatusResult, error) {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	m.logger.Debug("Getting pod status",
		"provider", providerName,
		"provider_pod_id", providerPodID)

	result, err := provider.GetStatus(ctx, providerPodID)
	if err != nil {
		m.logger.Error("Failed to get pod status",
			"provider", providerName,
			"provider_pod_id", providerPodID,
			"error", err)
		return nil, err
	}

	m.logger.Debug("Pod status retrieved",
		"provider", providerName,
		"provider_pod_id", providerPodID,
		"status", result.Status)

	return result, nil
}

// Terminate terminates a pod on the specified provider
func (m *Manager) Terminate(ctx context.Context, providerName string, providerPodID string) error {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return err
	}

	m.logger.Info("Terminating pod",
		"provider", providerName,
		"provider_pod_id", providerPodID)

	err = provider.Terminate(ctx, providerPodID)
	if err != nil {
		m.logger.Error("Failed to terminate pod",
			"provider", providerName,
			"provider_pod_id", providerPodID,
			"error", err)
		return err
	}

	m.logger.Info("Pod terminated successfully",
		"provider", providerName,
		"provider_pod_id", providerPodID)

	return nil
}

// GetPricing retrieves pricing information from a specific provider
func (m *Manager) GetPricing(ctx context.Context, providerName string) (*PricingResult, error) {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	m.logger.Debug("Getting pricing information", "provider", providerName)

	result, err := provider.GetPricing(ctx)
	if err != nil {
		m.logger.Error("Failed to get pricing information",
			"provider", providerName,
			"error", err)
		return nil, err
	}

	m.logger.Debug("Pricing information retrieved",
		"provider", providerName,
		"gpu_types_count", len(result.GPUTypes))

	return result, nil
}

// GetAvailability checks availability for a specific provider
func (m *Manager) GetAvailability(ctx context.Context, providerName string, query *AvailabilityQuery) (*AvailabilityResult, error) {
	provider, err := m.GetProvider(providerName)
	if err != nil {
		return nil, err
	}

	m.logger.Debug("Checking availability",
		"provider", providerName,
		"min_memory_gb", query.MinMemoryGB,
		"max_price", query.MaxPricePerHr)

	result, err := provider.GetAvailability(ctx, query)
	if err != nil {
		m.logger.Error("Failed to check availability",
			"provider", providerName,
			"error", err)
		return nil, err
	}

	m.logger.Debug("Availability checked",
		"provider", providerName,
		"available_gpus_count", len(result.AvailableGPUs))

	return result, nil
}

// GetAllPricing retrieves pricing information from all registered providers
func (m *Manager) GetAllPricing(ctx context.Context) (map[string]*PricingResult, error) {
	m.mutex.RLock()
	providers := make(map[string]Provider)
	for name, provider := range m.providers {
		providers[name] = provider
	}
	m.mutex.RUnlock()

	results := make(map[string]*PricingResult)
	errors := make(map[string]error)

	var wg sync.WaitGroup
	var resultMutex sync.Mutex

	for name, provider := range providers {
		wg.Add(1)
		go func(name string, provider Provider) {
			defer wg.Done()

			result, err := provider.GetPricing(ctx)

			resultMutex.Lock()
			if err != nil {
				errors[name] = err
			} else {
				results[name] = result
			}
			resultMutex.Unlock()
		}(name, provider)
	}

	wg.Wait()

	// Log any errors but don't fail the entire operation
	for name, err := range errors {
		m.logger.Warn("Failed to get pricing from provider",
			"provider", name,
			"error", err)
	}

	return results, nil
}

// GetAllAvailability checks availability across all registered providers
func (m *Manager) GetAllAvailability(ctx context.Context, query *AvailabilityQuery) (map[string]*AvailabilityResult, error) {
	m.mutex.RLock()
	providers := make(map[string]Provider)
	for name, provider := range m.providers {
		providers[name] = provider
	}
	m.mutex.RUnlock()

	results := make(map[string]*AvailabilityResult)
	errors := make(map[string]error)

	var wg sync.WaitGroup
	var resultMutex sync.Mutex

	for name, provider := range providers {
		wg.Add(1)
		go func(name string, provider Provider) {
			defer wg.Done()

			result, err := provider.GetAvailability(ctx, query)

			resultMutex.Lock()
			if err != nil {
				errors[name] = err
			} else {
				results[name] = result
			}
			resultMutex.Unlock()
		}(name, provider)
	}

	wg.Wait()

	// Log any errors but don't fail the entire operation
	for name, err := range errors {
		m.logger.Warn("Failed to check availability from provider",
			"provider", name,
			"error", err)
	}

	return results, nil
}

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
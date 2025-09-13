package virtualkubelet

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// HealthServer provides health check endpoints for the proxy kubelet
type HealthServer struct {
	server       *http.Server
	logger       *slog.Logger
	readinessFunc func() bool
	livenessFunc  func() bool
	mutex        sync.RWMutex
}

// HealthStatus represents the health status response
type HealthStatus struct {
	Status      string            `json:"status"`
	Timestamp   time.Time         `json:"timestamp"`
	Uptime      time.Duration     `json:"uptime"`
	Version     string            `json:"version"`
	Providers   map[string]bool   `json:"providers,omitempty"`
	WebSocket   WebSocketStatus   `json:"websocket"`
	Details     map[string]string `json:"details,omitempty"`
}

// WebSocketStatus represents WebSocket connection status
type WebSocketStatus struct {
	Connected   bool      `json:"connected"`
	LastPing    time.Time `json:"last_ping,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
}

// NewHealthServer creates a new health server
func NewHealthServer(address string, readinessFunc func() bool) *HealthServer {
	mux := http.NewServeMux()

	server := &http.Server{
		Addr:         address,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	hs := &HealthServer{
		server:        server,
		logger:        slog.Default(),
		readinessFunc: readinessFunc,
		livenessFunc:  func() bool { return true }, // Default liveness check
	}

	// Register health check endpoints
	mux.HandleFunc("/healthz", hs.handleLiveness)
	mux.HandleFunc("/readyz", hs.handleReadiness)
	mux.HandleFunc("/health", hs.handleHealth)
	mux.HandleFunc("/status", hs.handleStatus)

	return hs
}

// SetLogger sets the logger for the health server
func (hs *HealthServer) SetLogger(logger *slog.Logger) {
	hs.mutex.Lock()
	hs.logger = logger
	hs.mutex.Unlock()
}

// SetLivenessFunc sets the liveness check function
func (hs *HealthServer) SetLivenessFunc(fn func() bool) {
	hs.mutex.Lock()
	hs.livenessFunc = fn
	hs.mutex.Unlock()
}

// Start starts the health server
func (hs *HealthServer) Start() {
	go func() {
		hs.logger.Info("Starting health server", "address", hs.server.Addr)

		if err := hs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			hs.logger.Error("Health server failed", "error", err)
		}
	}()
}

// Stop gracefully stops the health server
func (hs *HealthServer) Stop() error {
	hs.logger.Info("Stopping health server")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return hs.server.Shutdown(ctx)
}

// handleLiveness handles the liveness probe endpoint
func (hs *HealthServer) handleLiveness(w http.ResponseWriter, r *http.Request) {
	hs.mutex.RLock()
	livenessFunc := hs.livenessFunc
	hs.mutex.RUnlock()

	if livenessFunc != nil && !livenessFunc() {
		http.Error(w, "Service not alive", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]interface{}{
		"status":    "alive",
		"timestamp": time.Now(),
	}

	json.NewEncoder(w).Encode(response)
}

// handleReadiness handles the readiness probe endpoint
func (hs *HealthServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	hs.mutex.RLock()
	readinessFunc := hs.readinessFunc
	hs.mutex.RUnlock()

	if readinessFunc != nil && !readinessFunc() {
		http.Error(w, "Service not ready", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]interface{}{
		"status":    "ready",
		"timestamp": time.Now(),
	}

	json.NewEncoder(w).Encode(response)
}

// handleHealth handles the general health check endpoint
func (hs *HealthServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	hs.mutex.RLock()
	readinessFunc := hs.readinessFunc
	livenessFunc := hs.livenessFunc
	hs.mutex.RUnlock()

	healthy := true
	status := "healthy"

	// Check liveness
	if livenessFunc != nil && !livenessFunc() {
		healthy = false
		status = "not_alive"
	}

	// Check readiness
	if readinessFunc != nil && !readinessFunc() {
		healthy = false
		if status == "healthy" {
			status = "not_ready"
		}
	}

	w.Header().Set("Content-Type", "application/json")

	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	response := map[string]interface{}{
		"status":    status,
		"healthy":   healthy,
		"timestamp": time.Now(),
	}

	json.NewEncoder(w).Encode(response)
}

// handleStatus handles the detailed status endpoint
func (hs *HealthServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	// This would be populated by the main provider
	// For now, return basic status
	status := HealthStatus{
		Status:    "running",
		Timestamp: time.Now(),
		Version:   "1.0.0",
		WebSocket: WebSocketStatus{
			Connected: false, // This should be updated by the provider
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status)
}

// UpdateWebSocketStatus updates the WebSocket connection status
func (hs *HealthServer) UpdateWebSocketStatus(connected bool, connectedAt time.Time) {
	// This method would be called by the provider to update WebSocket status
	// Implementation would store this information for the status endpoint
}

// UpdateProviderStatus updates the provider health status
func (hs *HealthServer) UpdateProviderStatus(providers map[string]bool) {
	// This method would be called by the provider to update provider status
	// Implementation would store this information for the status endpoint
}
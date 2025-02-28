package controller

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// HealthServer represents a simple HTTP server for health checks
type HealthServer struct {
	server     *http.Server
	healthy    int32
	readyFunc  func() bool
	listenAddr string
}

// NewHealthServer creates a new health check server
func NewHealthServer(listenAddr string, readyFunc func() bool) *HealthServer {
	hs := &HealthServer{
		listenAddr: listenAddr,
		readyFunc:  readyFunc,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", hs.healthzHandler)
	mux.HandleFunc("/readyz", hs.readyzHandler)

	hs.server = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	return hs
}

// Start begins the health check server
func (hs *HealthServer) Start() {
	atomic.StoreInt32(&hs.healthy, 1)
	go func() {
		if err := hs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Log error but don't crash the application
			// This would use the controller's logger in a real implementation
		}
	}()
}

// Stop gracefully shuts down the health check server
func (hs *HealthServer) Stop() error {
	atomic.StoreInt32(&hs.healthy, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.server.Shutdown(ctx)
}

// healthzHandler handles liveness probe requests
func (hs *HealthServer) healthzHandler(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&hs.healthy) == 1 {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// readyzHandler handles readiness probe requests
func (hs *HealthServer) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if atomic.LoadInt32(&hs.healthy) == 1 && hs.readyFunc() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}
package config

import (
	"time"
)

// Config holds configuration for the RunPod controller
type Config struct {
	// ReconcileInterval is how frequently the controller checks for jobs to offload
	ReconcileInterval time.Duration

	// PendingJobThreshold is the number of pending jobs that triggers automatic offloading
	PendingJobThreshold int

	// MaxGPUPrice is the maximum price per hour we're willing to pay for GPU instances
	MaxGPUPrice float64

	// HealthServerAddress is the address where the health server listens
	HealthServerAddress string
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		ReconcileInterval:    30 * time.Second,
		PendingJobThreshold:  5,
		MaxGPUPrice:          0.5,
		HealthServerAddress:  ":8080",
	}
}
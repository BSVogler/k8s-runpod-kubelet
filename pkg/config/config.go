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

	// MaxPendingTime that a job is allowed to stay in pending state before it is offloaded
	MaxPendingTime int

	// MaxGPUPrice is the maximum price per hour we're willing to pay for GPU instances
	MaxGPUPrice float64

	// HealthServerAddress is the address where the health server listens
	HealthServerAddress string

	// DatacenterIDs is a comma-separated list of preferred datacenter IDs for pod placement
	DatacenterIDs string
}

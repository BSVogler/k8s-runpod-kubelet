package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds configuration for the proxy kubelet
type Config struct {
	// Backend connection settings
	BackendURL    string `yaml:"backend_url"`
	BackendAPIKey string `yaml:"backend_api_key"`

	// WebSocket configuration
	WebSocket WebSocketConfig `yaml:"websocket"`

	// Provider configuration
	Providers ProviderConfig `yaml:"providers"`

	// Kubelet settings
	ClusterName         string        `yaml:"cluster_name"`
	NodeName            string        `yaml:"node_name"`
	OperatingSystem     string        `yaml:"operating_system"`
	InternalIP          string        `yaml:"internal_ip"`
	ListenPort          int           `yaml:"listen_port"`
	HealthServerAddress string        `yaml:"health_server_address"`
	Namespace           string        `yaml:"namespace"`
	ReconcileInterval   time.Duration `yaml:"reconcile_interval"`

	// Logging
	LogLevel string `yaml:"log_level"`

	// Heartbeat configuration
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
}

// WebSocketConfig holds WebSocket-specific configuration
type WebSocketConfig struct {
	ReconnectInterval time.Duration `yaml:"reconnect_interval"`
	MaxReconnectDelay time.Duration `yaml:"max_reconnect_delay"`
	PingInterval      time.Duration `yaml:"ping_interval"`
	PongTimeout       time.Duration `yaml:"pong_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
}

// ProviderConfig holds provider-specific configuration
type ProviderConfig struct {
	EnabledProviders []string             `yaml:"enabled_providers"`
	RunPod           RunPodProviderConfig `yaml:"runpod"`
	VastAI           VastAIProviderConfig `yaml:"vastai"`
	Salad            SaladProviderConfig  `yaml:"salad"`
	AWS              AWSProviderConfig    `yaml:"aws"`
	GCP              GCPProviderConfig    `yaml:"gcp"`
}

// RunPodProviderConfig holds RunPod-specific configuration
type RunPodProviderConfig struct {
	APIKey           string   `yaml:"api_key"`
	DefaultCloudType string   `yaml:"default_cloud_type"`
	DatacenterIDs    []string `yaml:"datacenter_ids"`
	MaxGPUPrice      float64  `yaml:"max_gpu_price"`
}

// VastAIProviderConfig holds Vast.ai-specific configuration
type VastAIProviderConfig struct {
	APIKey        string   `yaml:"api_key"`
	DatacenterIDs []string `yaml:"datacenter_ids"`
	MaxGPUPrice   float64  `yaml:"max_gpu_price"`
}

// SaladProviderConfig holds Salad-specific configuration
type SaladProviderConfig struct {
	APIKey      string  `yaml:"api_key"`
	MaxGPUPrice float64 `yaml:"max_gpu_price"`
}

// AWSProviderConfig holds AWS-specific configuration
type AWSProviderConfig struct {
	AccessKeyID       string   `yaml:"access_key_id"`
	SecretAccessKey   string   `yaml:"secret_access_key"`
	Region            string   `yaml:"region"`
	AvailabilityZones []string `yaml:"availability_zones"`
	MaxInstancePrice  float64  `yaml:"max_instance_price"`
}

// GCPProviderConfig holds GCP-specific configuration
type GCPProviderConfig struct {
	ServiceAccountKey string   `yaml:"service_account_key"`
	ProjectID         string   `yaml:"project_id"`
	Zones             []string `yaml:"zones"`
	MaxInstancePrice  float64  `yaml:"max_instance_price"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		BackendURL: "wss://gpuconduit.io/api/kubelet/ws",

		WebSocket: WebSocketConfig{
			ReconnectInterval: 5 * time.Second,
			MaxReconnectDelay: 300 * time.Second,
			PingInterval:      30 * time.Second,
			PongTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			ReadTimeout:       60 * time.Second,
		},

		Providers: ProviderConfig{
			EnabledProviders: []string{"runpod"}, // Default to RunPod only
			RunPod: RunPodProviderConfig{
				DefaultCloudType: "SECURE",
				MaxGPUPrice:      0.5,
			},
			VastAI: VastAIProviderConfig{
				MaxGPUPrice: 0.5,
			},
			Salad: SaladProviderConfig{
				MaxGPUPrice: 0.5,
			},
			AWS: AWSProviderConfig{
				Region:           "us-east-1",
				MaxInstancePrice: 2.0,
			},
			GCP: GCPProviderConfig{
				MaxInstancePrice: 2.0,
			},
		},

		ClusterName:         "default",
		NodeName:            "conduit-node",
		OperatingSystem:     "Linux",
		InternalIP:          "127.0.0.1",
		ListenPort:          10250,
		HealthServerAddress: ":8080",
		Namespace:           "kube-system",
		ReconcileInterval:   30 * time.Second,

		LogLevel: "info",

		HeartbeatInterval: 300 * time.Second,
	}
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.BackendURL == "" {
		return fmt.Errorf("backend_url is required")
	}

	if c.BackendAPIKey == "" {
		return fmt.Errorf("backend_api_key is required")
	}

	if c.NodeName == "" {
		return fmt.Errorf("node_name is required")
	}

	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		return fmt.Errorf("listen_port must be between 1 and 65535")
	}

	// Validate enabled providers
	if len(c.Providers.EnabledProviders) == 0 {
		return fmt.Errorf("at least one provider must be enabled")
	}

	validProviders := map[string]bool{
		"runpod": true,
		"vastai": true,
		"salad":  true,
		"aws":    true,
		"gcp":    true,
	}

	for _, provider := range c.Providers.EnabledProviders {
		if !validProviders[provider] {
			return fmt.Errorf("invalid provider: %s", provider)
		}
	}

	// Validate WebSocket timeouts
	if c.WebSocket.ReconnectInterval <= 0 {
		return fmt.Errorf("websocket reconnect_interval must be positive")
	}

	if c.WebSocket.PingInterval <= 0 {
		return fmt.Errorf("websocket ping_interval must be positive")
	}

	return nil
}

// Key management modes reported in the registration metadata.
const (
	KeyModeLocal    = "local"    // provider API keys are configured on the kubelet
	KeyModePlatform = "platform" // the platform must supply provider API keys with each command
)

// KeyMode returns "local" if any provider key is configured locally, else "platform".
func (c *Config) KeyMode() string {
	if c.HasLocalProviderKey() {
		return KeyModeLocal
	}
	return KeyModePlatform
}

// HasLocalProviderKey reports whether any provider credential is configured locally.
func (c *Config) HasLocalProviderKey() bool {
	p := c.Providers
	return p.RunPod.APIKey != "" ||
		p.VastAI.APIKey != "" ||
		p.Salad.APIKey != "" ||
		(p.AWS.AccessKeyID != "" && p.AWS.SecretAccessKey != "") ||
		p.GCP.ServiceAccountKey != ""
}

// IsProviderEnabled checks if a specific provider is enabled
func (c *Config) IsProviderEnabled(provider string) bool {
	for _, enabled := range c.Providers.EnabledProviders {
		if enabled == provider {
			return true
		}
	}
	return false
}

// GetWebSocketURL returns the WebSocket URL with proper scheme
func (c *Config) GetWebSocketURL() string {
	url := c.BackendURL

	// Ensure WebSocket scheme
	if strings.HasPrefix(url, "http://") {
		url = strings.Replace(url, "http://", "ws://", 1)
	} else if strings.HasPrefix(url, "https://") {
		url = strings.Replace(url, "https://", "wss://", 1)
	} else if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		// Default to secure WebSocket
		url = "wss://" + url
	}

	return url
}

// GetBackendHTTPURL returns the HTTP URL for the backend
func (c *Config) GetBackendHTTPURL() string {
	url := c.BackendURL

	// Convert WebSocket URL to HTTP URL
	if strings.HasPrefix(url, "ws://") {
		url = strings.Replace(url, "ws://", "http://", 1)
	} else if strings.HasPrefix(url, "wss://") {
		url = strings.Replace(url, "wss://", "https://", 1)
	} else if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		// Default to secure HTTP
		url = "https://" + url
	}

	// Remove WebSocket path if present
	if strings.Contains(url, "/api/kubelet/ws") {
		url = strings.Replace(url, "/api/kubelet/ws", "", 1)
	}

	return url
}

// Override configuration with environment variables
func (c *Config) LoadFromEnvironment() {
	if backendURL := os.Getenv("BACKEND_URL"); backendURL != "" {
		c.BackendURL = backendURL
	}

	if apiKey := os.Getenv("BACKEND_API_KEY"); apiKey != "" {
		c.BackendAPIKey = apiKey
	}

	if clusterName := os.Getenv("CLUSTER_NAME"); clusterName != "" {
		c.ClusterName = clusterName
	}

	if nodeName := os.Getenv("NODE_NAME"); nodeName != "" {
		c.NodeName = nodeName
	}

	if namespace := os.Getenv("NAMESPACE"); namespace != "" {
		c.Namespace = namespace
	}

	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		c.LogLevel = logLevel
	}

	// Load provider-specific environment variables
	if runpodKey := os.Getenv("RUNPOD_API_KEY"); runpodKey != "" {
		c.Providers.RunPod.APIKey = runpodKey
	}

	if vastaiKey := os.Getenv("VASTAI_API_KEY"); vastaiKey != "" {
		c.Providers.VastAI.APIKey = vastaiKey
	}

	if saladKey := os.Getenv("SALAD_API_KEY"); saladKey != "" {
		c.Providers.Salad.APIKey = saladKey
	}

	if awsKeyID := os.Getenv("AWS_ACCESS_KEY_ID"); awsKeyID != "" {
		c.Providers.AWS.AccessKeyID = awsKeyID
	}

	if awsSecret := os.Getenv("AWS_SECRET_ACCESS_KEY"); awsSecret != "" {
		c.Providers.AWS.SecretAccessKey = awsSecret
	}

	if awsRegion := os.Getenv("AWS_REGION"); awsRegion != "" {
		c.Providers.AWS.Region = awsRegion
	}

	if gcpKey := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); gcpKey != "" {
		c.Providers.GCP.ServiceAccountKey = gcpKey
	}

	if gcpProject := os.Getenv("GCP_PROJECT_ID"); gcpProject != "" {
		c.Providers.GCP.ProjectID = gcpProject
	}
}

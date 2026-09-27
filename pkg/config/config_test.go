package config

import "testing"

func TestKeyMode(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.KeyMode(); got != KeyModePlatform {
		t.Errorf("no local keys: key_mode = %q, want %q", got, KeyModePlatform)
	}

	cfg.Providers.RunPod.APIKey = "rp"
	if got := cfg.KeyMode(); got != KeyModeLocal {
		t.Errorf("with runpod key: key_mode = %q, want %q", got, KeyModeLocal)
	}

	cfg = DefaultConfig()
	cfg.Providers.AWS.AccessKeyID = "id" // secret missing => not a usable key
	if got := cfg.KeyMode(); got != KeyModePlatform {
		t.Errorf("partial aws creds: key_mode = %q, want %q", got, KeyModePlatform)
	}
}

func TestClusterNameFromEnvironment(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "prod-eu")
	cfg := DefaultConfig()
	if cfg.ClusterName != "default" {
		t.Fatalf("default cluster name = %q", cfg.ClusterName)
	}
	cfg.LoadFromEnvironment()
	if cfg.ClusterName != "prod-eu" {
		t.Errorf("cluster name = %q, want prod-eu", cfg.ClusterName)
	}
}

package websocket

import (
	"net/url"
	"testing"
)

func TestBuildDialURL(t *testing.T) {
	got, err := buildDialURL("wss://gpuconduit.io/api/kubelet/ws", "secret")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("api_key") != "secret" || u.Path != "/api/kubelet/ws" || u.Scheme != "wss" {
		t.Errorf("unexpected dial url: %s", got)
	}

	// An explicit token param is left alone.
	got, _ = buildDialURL("wss://host/ws?token=abc", "secret")
	u, _ = url.Parse(got)
	if u.Query().Get("api_key") != "" || u.Query().Get("token") != "abc" {
		t.Errorf("token param should be preserved: %s", got)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("wss://host/ws?api_key=secret")
	if got != "wss://host/ws?api_key=%2A%2A%2A" && got != "wss://host/ws?api_key=***" {
		t.Errorf("token leaked: %s", got)
	}
}

func TestNewClientDefaultsKubeletIDToToken(t *testing.T) {
	c := NewClient(&ClientConfig{APIToken: "tok"}, discardLogger())
	if c.KubeletID() != "tok" {
		t.Errorf("kubelet id = %q", c.KubeletID())
	}
	if c.config.HeartbeatInterval <= 0 {
		t.Errorf("heartbeat interval must default to a positive value")
	}
}

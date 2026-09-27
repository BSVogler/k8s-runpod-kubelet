package websocket

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// roundTrip marshals in, parses it back as an Envelope and decodes the payload into out.
func roundTrip(t *testing.T, envType EnvelopeType, in interface{}, out interface{}) *Envelope {
	t.Helper()

	env, err := NewEnvelope(envType, in, "kubelet-1")
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	data, err := env.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}

	parsed, err := ParseEnvelope(data)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if parsed.Type != envType {
		t.Fatalf("type = %q, want %q", parsed.Type, envType)
	}
	if parsed.ID == "" {
		t.Fatalf("envelope id is empty")
	}
	if parsed.KubeletID != "kubelet-1" {
		t.Fatalf("kubelet_id = %q", parsed.KubeletID)
	}
	if parsed.Timestamp.IsZero() {
		t.Fatalf("timestamp is zero")
	}
	if err := parsed.DecodePayload(out); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	return parsed
}

func topLevelKeys(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestEnvelopeWireFormat(t *testing.T) {
	env, err := NewEnvelopeWithID("cmd-1", EnvelopeHeartbeat, NewHeartbeat("k"), "k")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := env.ToJSON()
	keys := topLevelKeys(t, data)

	for _, k := range []string{"id", "type", "payload", "timestamp", "kubelet_id"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("envelope missing %q: %s", k, data)
		}
	}
	if _, ok := keys["data"]; ok {
		t.Errorf("envelope must not use legacy 'data' key: %s", data)
	}

	// Timestamp must be RFC3339 with timezone.
	var ts string
	if err := json.Unmarshal(keys["timestamp"], &ts); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", ts, err)
	}
}

func TestEnvelopeKubeletIDOmittedWhenEmpty(t *testing.T) {
	env, _ := NewEnvelope(EnvelopeEvent, &Event{Type: EventKubeletReady, Data: &KubeletReadyEvent{NodeName: "n"}}, "")
	data, _ := env.ToJSON()
	if _, ok := topLevelKeys(t, data)["kubelet_id"]; ok {
		t.Errorf("kubelet_id should be omitted when empty: %s", data)
	}
}

func TestTimestampLenientParsing(t *testing.T) {
	cases := map[string]string{
		"rfc3339":         `"2026-09-27T10:11:12Z"`,
		"rfc3339 offset":  `"2026-09-27T12:11:12+02:00"`,
		"python utcnow":   `"2026-09-27T10:11:12.123456"`,
		"python no micro": `"2026-09-27T10:11:12"`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var ts Timestamp
			if err := json.Unmarshal([]byte(raw), &ts); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if !ts.UTC().Truncate(time.Second).Equal(time.Date(2026, 9, 27, 10, 11, 12, 0, time.UTC)) {
				t.Errorf("parsed %v, want 2026-09-27T10:11:12Z", ts.UTC())
			}
		})
	}

	var bad Timestamp
	if err := json.Unmarshal([]byte(`"yesterday"`), &bad); err == nil {
		t.Errorf("expected error for garbage timestamp")
	}
}

func TestCommandDeployRoundTrip(t *testing.T) {
	// Exactly the wire shape the service sends.
	wire := `{
	  "id": "11111111-2222-3333-4444-555555555555",
	  "type": "command",
	  "timestamp": "2026-09-27T10:11:12.123456",
	  "kubelet_id": "tok",
	  "payload": {
	    "type": "deploy",
	    "data": {
	      "pod_name": "gpu-job-1", "namespace": "default", "provider": "runpod",
	      "api_key": "rp-key",
	      "image": "nvidia/cuda", "name": "gpu-job-1",
	      "env": {"K": "V"}, "ports": ["8080/http", "22/tcp"],
	      "container_disk_in_gb": 10, "volume_in_gb": 0,
	      "gpu_type_ids": ["NVIDIA A100 80GB PCIe"], "gpu_count": 2, "min_ram_per_gpu": 24,
	      "cloud_type": "SECURE", "datacenter_ids": ["EU-RO-1"],
	      "template_id": "", "container_auth_id": "", "max_price": 1.5,
	      "command": ["python"], "args": ["train.py"],
	      "some_future_field": true
	    }
	  }
	}`

	env, err := ParseEnvelope([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != EnvelopeCommand || env.ID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("unexpected envelope: %+v", env)
	}

	var cmd Command
	if err := env.DecodePayload(&cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Type != CommandDeploy {
		t.Fatalf("command type = %q", cmd.Type)
	}

	var params DeployParams
	if err := cmd.DecodeData(&params); err != nil {
		t.Fatal(err)
	}

	want := DeployParams{
		PodName: "gpu-job-1", Namespace: "default", Provider: "runpod", APIKey: "rp-key",
		Image: "nvidia/cuda", Name: "gpu-job-1",
		Env: map[string]string{"K": "V"}, Ports: []string{"8080/http", "22/tcp"},
		ContainerDiskInGb: 10, GPUTypeIDs: []string{"NVIDIA A100 80GB PCIe"}, GPUCount: 2, MinRAMPerGPU: 24,
		CloudType: "SECURE", DatacenterIDs: []string{"EU-RO-1"}, MaxPrice: 1.5,
		Command: []string{"python"}, Args: []string{"train.py"},
	}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("params mismatch\n got: %+v\nwant: %+v", params, want)
	}

	// And back out again through our own marshaller.
	cmd2, err := NewCommand(CommandDeploy, &params)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DeployParams
	roundTrip(t, EnvelopeCommand, cmd2, &Command{})
	if err := cmd2.DecodeData(&decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("re-encoded params mismatch: %+v", decoded)
	}
}

func TestCommandTerminateStatusRejectPingRoundTrip(t *testing.T) {
	terminate := TerminateParams{PodName: "p", Namespace: "ns", Provider: "runpod", ProviderPodID: "abc123"}
	status := StatusParams{PodName: "p", Namespace: "ns", Provider: "runpod", ProviderPodID: "abc123", APIKey: "k"}
	reject := RejectParams{PodName: "p", Namespace: "ns", Code: RejectQuotaExceeded,
		Message: "GPU hours quota exceeded", UpgradeURL: "https://gpuconduit.io/dashboard/billing/"}

	t.Run("terminate", func(t *testing.T) {
		cmd, _ := NewCommand(CommandTerminate, terminate)
		var got Command
		roundTrip(t, EnvelopeCommand, cmd, &got)
		var params TerminateParams
		if err := got.DecodeData(&params); err != nil {
			t.Fatal(err)
		}
		if params != terminate {
			t.Errorf("got %+v", params)
		}
		if _, ok := topLevelKeys(t, got.Data)["api_key"]; ok {
			t.Errorf("api_key must be omitted when empty: %s", got.Data)
		}
	})

	t.Run("status", func(t *testing.T) {
		cmd, _ := NewCommand(CommandStatus, status)
		var got Command
		roundTrip(t, EnvelopeCommand, cmd, &got)
		var params StatusParams
		if err := got.DecodeData(&params); err != nil {
			t.Fatal(err)
		}
		if params != status {
			t.Errorf("got %+v", params)
		}
	})

	t.Run("reject", func(t *testing.T) {
		cmd, _ := NewCommand(CommandReject, reject)
		var got Command
		roundTrip(t, EnvelopeCommand, cmd, &got)
		var params RejectParams
		if err := got.DecodeData(&params); err != nil {
			t.Fatal(err)
		}
		if params != reject {
			t.Errorf("got %+v", params)
		}
		keys := topLevelKeys(t, got.Data)
		for _, k := range []string{"pod_name", "namespace", "code", "message", "upgrade_url"} {
			if _, ok := keys[k]; !ok {
				t.Errorf("reject data missing %q", k)
			}
		}
	})

	t.Run("ping", func(t *testing.T) {
		env, err := ParseEnvelope([]byte(`{"id":"p1","type":"command","payload":{"type":"ping","data":{}},"timestamp":"2026-09-27T10:00:00Z"}`))
		if err != nil {
			t.Fatal(err)
		}
		var cmd Command
		if err := env.DecodePayload(&cmd); err != nil {
			t.Fatal(err)
		}
		if cmd.Type != CommandPing {
			t.Errorf("type = %q", cmd.Type)
		}
	})
}

func TestResponseRoundTrip(t *testing.T) {
	t.Run("success deploy", func(t *testing.T) {
		result := &DeployResult{
			ProviderPodID: "abc123", Provider: "runpod", Status: "RUNNING", CostPerHour: 0.79,
			GPUType: "NVIDIA A100 80GB PCIe", MachineID: "m1", DatacenterID: "EU-RO-1",
			Ports: map[string]string{"8080": "https://abc123-8080.proxy.runpod.net"},
		}
		resp := NewSuccessResponse("cmd-1", result)

		env, _ := NewEnvelope(EnvelopeResponse, resp, "k")
		data, _ := env.ToJSON()
		parsed, _ := ParseEnvelope(data)

		var got struct {
			CommandID string        `json:"command_id"`
			Status    string        `json:"status"`
			Data      DeployResult  `json:"data"`
			Error     *ErrorDetails `json:"error"`
		}
		if err := parsed.DecodePayload(&got); err != nil {
			t.Fatal(err)
		}
		if got.CommandID != "cmd-1" || got.Status != "success" || got.Error != nil {
			t.Errorf("unexpected response: %+v", got)
		}
		if !reflect.DeepEqual(got.Data, *result) {
			t.Errorf("data mismatch: %+v", got.Data)
		}

		keys := topLevelKeys(t, parsed.Payload)
		if string(keys["error"]) != "null" {
			t.Errorf("error should be null on success, got %s", keys["error"])
		}
	})

	t.Run("success terminate", func(t *testing.T) {
		resp := NewSuccessResponse("cmd-2", &TerminateResult{Terminated: true})
		env, _ := NewEnvelope(EnvelopeResponse, resp, "k")
		data, _ := env.ToJSON()
		parsed, _ := ParseEnvelope(data)
		var got struct {
			Data map[string]bool `json:"data"`
		}
		if err := parsed.DecodePayload(&got); err != nil {
			t.Fatal(err)
		}
		if !got.Data["terminated"] {
			t.Errorf("terminated missing: %s", parsed.Payload)
		}
	})

	t.Run("success status", func(t *testing.T) {
		result := &StatusResult{PodName: "p", ProviderPodID: "abc123", Provider: "runpod", Status: "RUNNING",
			Phase: "Running", IsRunning: true, IsTerminated: false, ExitCode: 0}
		resp := NewSuccessResponse("cmd-3", result)
		env, _ := NewEnvelope(EnvelopeResponse, resp, "k")
		data, _ := env.ToJSON()
		parsed, _ := ParseEnvelope(data)
		var got struct {
			Data StatusResult `json:"data"`
		}
		if err := parsed.DecodePayload(&got); err != nil {
			t.Fatal(err)
		}
		if got.Data != *result {
			t.Errorf("status mismatch: %+v", got.Data)
		}
		var raw struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(parsed.Payload, &raw)
		for _, k := range []string{"pod_name", "provider_pod_id", "provider", "status", "phase", "is_running", "is_terminated", "exit_code"} {
			if _, ok := raw.Data[k]; !ok {
				t.Errorf("status result missing %q", k)
			}
		}
	})

	t.Run("empty success", func(t *testing.T) {
		resp := NewSuccessResponse("cmd-4", nil)
		data, _ := json.Marshal(resp)
		if string(topLevelKeys(t, data)["data"]) != "{}" {
			t.Errorf("empty success data should be {}: %s", data)
		}
	})

	t.Run("error", func(t *testing.T) {
		resp := NewErrorResponse("cmd-5", "deployment_failed", "runpod error: boom", "boom")
		var got Response
		roundTrip(t, EnvelopeResponse, resp, &got)
		if got.Status != StatusError || got.CommandID != "cmd-5" {
			t.Errorf("unexpected: %+v", got)
		}
		if got.Error == nil || got.Error.Code != "deployment_failed" || got.Error.Message != "runpod error: boom" || got.Error.ProviderError != "boom" {
			t.Errorf("error details: %+v", got.Error)
		}
		if got.Data != nil {
			t.Errorf("data should be null on error: %v", got.Data)
		}
		data, _ := json.Marshal(resp)
		if string(topLevelKeys(t, data)["data"]) != "null" {
			t.Errorf("data should serialize as null: %s", data)
		}
	})
}

func TestEventRoundTrip(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-job-1", Namespace: "default", UID: "uid-1",
			Annotations: map[string]string{"conduit.io/provider": "runpod"}},
		Spec: v1.PodSpec{Containers: []v1.Container{{Name: "main", Image: "nvidia/cuda", Command: []string{"python"}}}},
	}
	exit := 1

	cases := []struct {
		name  string
		event *Event
		keys  []string
	}{
		{"pod_created", &Event{Type: EventPodCreated, Data: &PodCreatedEvent{
			PodName: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID), PodSpec: pod, Annotations: pod.Annotations}},
			[]string{"pod_name", "namespace", "uid", "pod_spec", "annotations"}},
		{"pod_deleted", &Event{Type: EventPodDeleted, Data: &PodDeletedEvent{
			PodName: "p", Namespace: "ns", UID: "u", ProviderPodID: "abc", Reason: "Pod deleted by Kubernetes"}},
			[]string{"pod_name", "namespace", "uid", "provider_pod_id", "reason"}},
		{"pod_status_change", &Event{Type: EventPodStatusChange, Data: &PodStatusChangeEvent{
			PodName: "p", Namespace: "ns", OldStatus: "RUNNING", NewStatus: "EXITED", Message: "done", ExitCode: &exit}},
			[]string{"pod_name", "namespace", "old_status", "new_status", "message", "exit_code"}},
		{"kubelet_ready", &Event{Type: EventKubeletReady, Data: &KubeletReadyEvent{NodeName: "node"}}, []string{"node_name"}},
		{"kubelet_error", &Event{Type: EventKubeletError, Data: &KubeletErrorEvent{Message: "boom"}}, []string{"message"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Type EventType                  `json:"type"`
				Data map[string]json.RawMessage `json:"data"`
			}
			roundTrip(t, EnvelopeEvent, tc.event, &got)
			if got.Type != tc.event.Type {
				t.Errorf("type = %q", got.Type)
			}
			for _, k := range tc.keys {
				if _, ok := got.Data[k]; !ok {
					t.Errorf("event data missing %q", k)
				}
			}
		})
	}

	t.Run("pod_created carries full pod spec", func(t *testing.T) {
		var got struct {
			Data PodCreatedEvent `json:"data"`
		}
		roundTrip(t, EnvelopeEvent, cases[0].event, &got)
		if got.Data.PodSpec == nil || got.Data.PodSpec.Spec.Containers[0].Image != "nvidia/cuda" || got.Data.UID != "uid-1" {
			t.Errorf("pod spec not preserved: %+v", got.Data)
		}
	})

	t.Run("pod_deleted omits empty provider_pod_id", func(t *testing.T) {
		data, _ := json.Marshal(&PodDeletedEvent{PodName: "p", Namespace: "ns", UID: "u", Reason: "r"})
		if _, ok := topLevelKeys(t, data)["provider_pod_id"]; ok {
			t.Errorf("provider_pod_id should be omitted: %s", data)
		}
	})
}

func TestHeartbeatRoundTrip(t *testing.T) {
	hb := NewHeartbeat("tok")
	var got Heartbeat
	roundTrip(t, EnvelopeHeartbeat, hb, &got)
	if got.KubeletID != "tok" || got.Status != "alive" || got.Timestamp.IsZero() {
		t.Errorf("unexpected heartbeat: %+v", got)
	}
}

func TestRegistrationRoundTrip(t *testing.T) {
	reg := &Registration{
		ID: "tok", Type: KubeletType, ClusterName: "prod", NodeName: "gpu-node", Namespace: "kube-system",
		Capabilities: []string{"runpod"},
		Metadata:     RegistrationMetadata{Version: ProtocolVersion, InternalIP: "10.0.0.5", KeyMode: "local"},
	}
	var got Registration
	env := roundTrip(t, EnvelopeRegistration, reg, &got)
	if !reflect.DeepEqual(got, *reg) {
		t.Errorf("registration mismatch: %+v", got)
	}

	var raw map[string]json.RawMessage
	_ = json.Unmarshal(env.Payload, &raw)
	for _, k := range []string{"id", "type", "cluster_name", "node_name", "namespace", "capabilities", "metadata"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("registration missing %q", k)
		}
	}
	var meta map[string]string
	_ = json.Unmarshal(raw["metadata"], &meta)
	if meta["key_mode"] != "local" || meta["version"] != "2.0.0" || meta["internal_ip"] != "10.0.0.5" {
		t.Errorf("metadata: %v", meta)
	}
}

func TestParseEnvelopeRejectsMissingType(t *testing.T) {
	if _, err := ParseEnvelope([]byte(`{"id":"x","payload":{}}`)); err == nil {
		t.Error("expected error for missing type")
	}
	if _, err := ParseEnvelope([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid json")
	}
}

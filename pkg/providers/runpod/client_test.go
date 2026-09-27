package runpod

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

func testClient(serverURL string) *Client {
	return &Client{
		httpClient:  &http.Client{},
		baseRESTURL: serverURL + "/v1/",
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestConvertDeployParamsMapsCommandArgsGPUCount(t *testing.T) {
	c := testClient("http://unused")

	t.Run("command, args and gpu_count", func(t *testing.T) {
		params := c.convertDeployParams(&websocket.DeployParams{
			Name: "job", Image: "nvidia/cuda",
			Command:  []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
			Args:     []string{"--model", "allenai/olmOCR-2"},
			GPUCount: 4,
		})

		if !reflect.DeepEqual(params["dockerEntrypoint"], []string{"python", "-m", "vllm.entrypoints.openai.api_server"}) {
			t.Errorf("dockerEntrypoint = %v", params["dockerEntrypoint"])
		}
		if !reflect.DeepEqual(params["dockerStartCmd"], []string{"--model", "allenai/olmOCR-2"}) {
			t.Errorf("dockerStartCmd = %v", params["dockerStartCmd"])
		}
		if params["gpuCount"] != 4 {
			t.Errorf("gpuCount = %v", params["gpuCount"])
		}
	})

	t.Run("unset fields keep image defaults", func(t *testing.T) {
		params := c.convertDeployParams(&websocket.DeployParams{Name: "job", Image: "nvidia/cuda"})

		for _, k := range []string{"dockerEntrypoint", "dockerStartCmd", "gpuCount"} {
			if _, ok := params[k]; ok {
				t.Errorf("%s should be omitted when unset: %v", k, params[k])
			}
		}
		if params["imageName"] != "nvidia/cuda" || params["name"] != "job" {
			t.Errorf("base params: %v", params)
		}
	})
}

func TestTerminateUsesDelete(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		expectErr bool
	}{
		{"ok", http.StatusOK, false},
		{"no content", http.StatusNoContent, false},
		{"already gone", http.StatusNotFound, false},
		{"server error", http.StatusInternalServerError, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			client := testClient(server.URL)
			err := client.Terminate(context.Background(), &websocket.TerminateParams{
				Provider: "runpod", ProviderPodID: "abc123", APIKey: "platform-key",
			})

			if tc.expectErr && err == nil {
				t.Fatalf("expected error for status %d", tc.status)
			}
			if !tc.expectErr && err != nil {
				t.Fatalf("unexpected error for status %d: %v", tc.status, err)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", gotMethod)
			}
			if gotPath != "/v1/pods/abc123" {
				t.Errorf("path = %s", gotPath)
			}
			if gotAuth != "Bearer platform-key" {
				t.Errorf("authorization = %q", gotAuth)
			}
		})
	}
}

func TestTerminateRejectsEmptyPodID(t *testing.T) {
	client := testClient("http://unused")
	if err := client.Terminate(context.Background(), &websocket.TerminateParams{APIKey: "k"}); err == nil {
		t.Error("expected error for empty provider_pod_id")
	}
}

func TestDeploySendsMappedParamsAndFillsResult(t *testing.T) {
	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/pods" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc123","costPerHr":0.79,"desiredStatus":"RUNNING","machineId":"m1",
			"machine":{"gpuTypeId":"NVIDIA A100 80GB PCIe","dataCenterId":"EU-RO-1"}}`))
	}))
	defer server.Close()

	client := testClient(server.URL)
	result, err := client.Deploy(context.Background(), &websocket.DeployParams{
		PodName: "gpu-job-1", Namespace: "default", Provider: "runpod", APIKey: "k",
		Name: "gpu-job-1", Image: "nvidia/cuda",
		Ports:    []string{"8080/http", "22/tcp"},
		Command:  []string{"python"},
		Args:     []string{"train.py"},
		GPUCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if body["gpuCount"] != float64(2) {
		t.Errorf("gpuCount sent = %v", body["gpuCount"])
	}
	if !reflect.DeepEqual(body["dockerEntrypoint"], []interface{}{"python"}) {
		t.Errorf("dockerEntrypoint sent = %v", body["dockerEntrypoint"])
	}
	if !reflect.DeepEqual(body["dockerStartCmd"], []interface{}{"train.py"}) {
		t.Errorf("dockerStartCmd sent = %v", body["dockerStartCmd"])
	}

	want := &websocket.DeployResult{
		ProviderPodID: "abc123", Provider: "runpod", Status: "RUNNING", CostPerHour: 0.79,
		GPUType: "NVIDIA A100 80GB PCIe", MachineID: "m1", DatacenterID: "EU-RO-1",
		Ports: map[string]string{"8080": "https://abc123-8080.proxy.runpod.net"},
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result mismatch\n got: %+v\nwant: %+v", result, want)
	}
}

func TestDeployWithoutAPIKeyFails(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "")
	client := testClient("http://unused")
	if _, err := client.Deploy(context.Background(), &websocket.DeployParams{Name: "j", Image: "i"}); err == nil {
		t.Error("expected missing api key error")
	}
}

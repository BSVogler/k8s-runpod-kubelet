package runpod

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestApplyContainerSpec(t *testing.T) {
	t.Run("command, args and gpu limit", func(t *testing.T) {
		params := map[string]interface{}{}
		applyContainerSpec(params, v1.Container{
			Command: []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
			Args:    []string{"--model", "allenai/olmOCR-2"},
			Resources: v1.ResourceRequirements{
				Limits: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
			},
		})

		assert.Equal(t, []string{"python", "-m", "vllm.entrypoints.openai.api_server"}, params["dockerEntrypoint"])
		assert.Equal(t, []string{"--model", "allenai/olmOCR-2"}, params["dockerStartCmd"])
		assert.Equal(t, int64(4), params["gpuCount"])
	})

	t.Run("gpu from requests when no limit", func(t *testing.T) {
		params := map[string]interface{}{}
		applyContainerSpec(params, v1.Container{
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
			},
		})

		assert.Equal(t, int64(2), params["gpuCount"])
	})

	t.Run("unset fields keep image defaults", func(t *testing.T) {
		params := map[string]interface{}{}
		applyContainerSpec(params, v1.Container{Image: "nvidia/cuda"})

		assert.Empty(t, params)
	})
}

func TestTerminatePodDeletes(t *testing.T) {
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
			var gotMethod, gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			client := &Client{
				baseRESTURL: server.URL + "/v1/",
				logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			err := client.TerminatePod("abc123")
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, http.MethodDelete, gotMethod)
			assert.Equal(t, "/v1/pods/abc123", gotPath)
		})
	}
}

package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

func newTestHandler() *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(providers.NewManager(logger), logger)
}

func commandEnvelope(t *testing.T, id string, cmdType websocket.CommandType, data interface{}) *websocket.Envelope {
	t.Helper()
	cmd, err := websocket.NewCommand(cmdType, data)
	if err != nil {
		t.Fatal(err)
	}
	env, err := websocket.NewEnvelopeWithID(id, websocket.EnvelopeCommand, cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestHandleRejectInvokesCallback(t *testing.T) {
	h := newTestHandler()

	var got *websocket.RejectParams
	h.SetRejectHandler(func(_ context.Context, params *websocket.RejectParams) error {
		got = params
		return nil
	})

	reject := websocket.RejectParams{
		PodName: "gpu-job-1", Namespace: "default", Code: websocket.RejectQuotaExceeded,
		Message: "GPU hours quota exceeded (10/10 h this month).", UpgradeURL: "https://gpuconduit.io/dashboard/billing/",
	}
	resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "cmd-1", websocket.CommandReject, reject))

	if resp.CommandID != "cmd-1" || resp.Status != websocket.StatusSuccess || resp.Error != nil {
		t.Fatalf("unexpected response: %+v", resp)
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok || len(data) != 0 {
		t.Errorf("reject response data should be {}, got %#v", resp.Data)
	}
	if got == nil || *got != reject {
		t.Errorf("callback params = %+v, want %+v", got, reject)
	}
}

func TestHandleRejectErrors(t *testing.T) {
	t.Run("callback failure", func(t *testing.T) {
		h := newTestHandler()
		h.SetRejectHandler(func(context.Context, *websocket.RejectParams) error {
			return errors.New("pod default/missing not found")
		})
		resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "cmd-2", websocket.CommandReject,
			websocket.RejectParams{PodName: "missing", Namespace: "default", Code: "no_api_key", Message: "m"}))

		if resp.Status != websocket.StatusError || resp.Error == nil || resp.Error.Code != ErrCodeRejectFailed {
			t.Errorf("unexpected response: %+v", resp)
		}
		if resp.Data != nil {
			t.Errorf("data should be nil on error")
		}
	})

	t.Run("no callback configured", func(t *testing.T) {
		h := newTestHandler()
		resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "cmd-3", websocket.CommandReject,
			websocket.RejectParams{PodName: "p", Namespace: "ns", Code: "provider_error", Message: "m"}))
		if resp.Status != websocket.StatusError || resp.Error == nil || resp.Error.Code != ErrCodeRejectFailed {
			t.Errorf("unexpected response: %+v", resp)
		}
	})

	t.Run("malformed data", func(t *testing.T) {
		h := newTestHandler()
		h.SetRejectHandler(func(context.Context, *websocket.RejectParams) error { return nil })
		env := commandEnvelope(t, "cmd-4", websocket.CommandReject, "not-an-object")
		resp := h.ExecuteCommand(context.Background(), env)
		if resp.Status != websocket.StatusError || resp.Error.Code != ErrCodeInvalidParams {
			t.Errorf("unexpected response: %+v", resp)
		}
	})
}

func TestHandlePing(t *testing.T) {
	h := newTestHandler()
	resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "ping-1", websocket.CommandPing, map[string]interface{}{}))
	if resp.Status != websocket.StatusSuccess || resp.CommandID != "ping-1" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestHandleUnknownCommandAndBadEnvelope(t *testing.T) {
	h := newTestHandler()

	resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "x-1", websocket.CommandType("update_config"), map[string]interface{}{}))
	if resp.Status != websocket.StatusError || resp.Error.Code != ErrCodeUnknownCommand || resp.CommandID != "x-1" {
		t.Errorf("unexpected response: %+v", resp)
	}

	bad := &websocket.Envelope{ID: "x-2", Type: websocket.EnvelopeCommand, Payload: []byte(`[]`)}
	resp = h.ExecuteCommand(context.Background(), bad)
	if resp.Status != websocket.StatusError || resp.Error.Code != ErrCodeInvalidCommand || resp.CommandID != "x-2" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestHandleDeployUnknownProvider(t *testing.T) {
	h := newTestHandler()
	resp := h.ExecuteCommand(context.Background(), commandEnvelope(t, "d-1", websocket.CommandDeploy,
		websocket.DeployParams{PodName: "p", Namespace: "ns", Provider: "vastai", Image: "i", Name: "p"}))
	if resp.Status != websocket.StatusError || resp.Error == nil || resp.Error.Code != ErrCodeDeploymentFailed {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestErrorResponseSurfacesProviderError(t *testing.T) {
	err := providers.NewProviderError("runpod", "deployment_failed", "no GPUs available", true)
	resp := errorResponse("d-2", ErrCodeDeploymentFailed, err)
	if resp.Error.Code != "deployment_failed" || resp.Error.ProviderError != "no GPUs available" {
		t.Errorf("unexpected error details: %+v", resp.Error)
	}
	if resp.Error.Message == "" {
		t.Errorf("message should be set")
	}
}

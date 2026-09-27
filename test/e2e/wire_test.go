//go:build e2e

// Package e2e drives the real WebSocket client and command handler against a
// running conduit-service. It covers the wire contract end to end without a
// Kubernetes API server: registration → pod_created → deploy/reject → response.
//
// Run:
//
//	CONDUIT_URL=ws://localhost:8010/api/kubelet/ws CONDUIT_TOKEN=<token> \
//	  go test -tags=e2e ./test/e2e -v
//
// With RUNPOD_API_KEY set the kubelet registers in local key mode and the
// service is expected to send a deploy command; without it the service must
// reject with no_api_key (unless the org has a platform-managed key).
package e2e

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bsvogler/conduit-kubelet/pkg/command"
	"github.com/bsvogler/conduit-kubelet/pkg/providers"
	"github.com/bsvogler/conduit-kubelet/pkg/providers/runpod"
	"github.com/bsvogler/conduit-kubelet/pkg/websocket"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWireContract(t *testing.T) {
	url, token := os.Getenv("CONDUIT_URL"), os.Getenv("CONDUIT_TOKEN")
	if url == "" || token == "" {
		t.Skip("CONDUIT_URL and CONDUIT_TOKEN not set")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	keyMode := "platform"
	if os.Getenv("RUNPOD_API_KEY") != "" {
		keyMode = "local"
	}

	manager := providers.NewManager(logger)
	if err := manager.RegisterProvider(runpod.NewClient(logger)); err != nil {
		t.Fatal(err)
	}
	handler := command.NewHandler(manager, logger)

	rejects := make(chan *websocket.RejectParams, 4)
	handler.SetRejectHandler(func(_ context.Context, p *websocket.RejectParams) error {
		rejects <- p
		return nil
	})

	client := websocket.NewClient(&websocket.ClientConfig{URL: url, APIToken: token}, logger)
	client.SetOnConnect(func() {
		err := client.SendRegistration(&websocket.Registration{
			ID: token, Type: websocket.KubeletType, ClusterName: "e2e-cluster",
			NodeName: "e2e-node", Namespace: "default", Capabilities: []string{"runpod"},
			Metadata: websocket.RegistrationMetadata{Version: websocket.ProtocolVersion, InternalIP: "127.0.0.1", KeyMode: keyMode},
		})
		if err != nil {
			t.Errorf("registration: %v", err)
		}
	})
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for !client.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("client never connected")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // let registration land

	// Ask for a GPU pod with command/args so the flat deploy params get exercised.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "e2e-gpu-pod", Namespace: "default", UID: "e2e-uid-1",
			Annotations: map[string]string{"runpod.io/required-gpu-memory": "16", "runpod.io/max-price": "0.01"},
		},
		Spec: v1.PodSpec{Containers: []v1.Container{{
			Name: "main", Image: "nvidia/cuda:12.4.0-base-ubuntu22.04",
			Command: []string{"nvidia-smi"}, Args: []string{"-L"},
			Ports:     []v1.ContainerPort{{ContainerPort: 8080}},
			Resources: v1.ResourceRequirements{Limits: v1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}},
		}}},
	}
	err := client.SendEvent(&websocket.Event{Type: websocket.EventPodCreated, Data: &websocket.PodCreatedEvent{
		PodName: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID), PodSpec: pod, Annotations: pod.Annotations,
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Execute whatever the platform sends until we get a reject or a successful deploy.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var sawDeploy bool
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for platform (sawDeploy=%v)", sawDeploy)
		case rp := <-rejects:
			t.Logf("REJECT code=%s message=%q upgrade_url=%s", rp.Code, rp.Message, rp.UpgradeURL)
			if rp.PodName != pod.Name {
				t.Errorf("reject for wrong pod %q", rp.PodName)
			}
			if keyMode == "platform" && rp.Code != "no_api_key" && rp.Code != "quota_exceeded" {
				t.Errorf("unexpected reject code %q in platform mode", rp.Code)
			}
			if keyMode == "local" && !sawDeploy {
				t.Errorf("rejected before any deploy was attempted: %s", rp.Code)
			}
			goto deleted
		case env := <-client.Commands():
			var cmd websocket.Command
			if err := env.DecodePayload(&cmd); err != nil {
				t.Fatal(err)
			}
			t.Logf("COMMAND %s id=%s", cmd.Type, env.ID)
			if cmd.Type == websocket.CommandDeploy {
				sawDeploy = true
				var dp websocket.DeployParams
				if err := cmd.DecodeData(&dp); err != nil {
					t.Fatal(err)
				}
				t.Logf("deploy params: image=%s gpu_count=%d command=%v args=%v ports=%v api_key_present=%v",
					dp.Image, dp.GPUCount, dp.Command, dp.Args, dp.Ports, dp.APIKey != "")
				if dp.GPUCount != 1 || len(dp.Command) != 1 || len(dp.Args) != 1 || dp.PodName != pod.Name {
					t.Errorf("deploy params not carried through: %+v", dp)
				}
			}
			resp := handler.ExecuteCommand(ctx, env)
			t.Logf("RESPONSE status=%s error=%+v", resp.Status, resp.Error)
			if err := client.SendResponse(resp); err != nil {
				t.Fatal(err)
			}
			if cmd.Type == websocket.CommandDeploy && resp.Status == websocket.StatusSuccess {
				goto deleted
			}
		}
	}

deleted:
	// Deleting the pod must not crash either side.
	if err := client.SendEvent(&websocket.Event{Type: websocket.EventPodDeleted, Data: &websocket.PodDeletedEvent{
		PodName: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID), Reason: "e2e cleanup",
	}}); err != nil {
		t.Fatal(err)
	}
	// Drain a possible terminate command.
	select {
	case env := <-client.Commands():
		resp := handler.ExecuteCommand(ctx, env)
		_ = client.SendResponse(resp)
		t.Logf("post-delete command handled: %s", resp.Status)
	case <-time.After(3 * time.Second):
	}
}

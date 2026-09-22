package runpod

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"golang.org/x/crypto/ssh"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func generateExecSSHKey() (string, ssh.Signer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), signer, nil
}

func ensureSSHPort(ports []string) []string {
	for _, p := range ports {
		if p == "22/tcp" || p == "22" {
			return ports
		}
	}
	return append(ports, "22/tcp")
}

func (p *Provider) emitEvent(pod *v1.Pod, eventType, reason, message string) {
	if p.clientset == nil || pod == nil {
		return
	}
	ev := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pod.Name + "-",
			Namespace:    pod.Namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Pod",
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        pod.UID,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         v1.EventSource{Component: p.nodeName},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
	}
	if _, err := p.clientset.CoreV1().Events(pod.Namespace).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		p.logger.Debug("failed to emit event", "pod", pod.Name, "reason", reason, "error", err)
	}
}

func sshEndpoint(status *DetailedStatus) (string, error) {
	if status == nil {
		return "", fmt.Errorf("missing RunPod status")
	}
	port := 22
	if status.PortMappings != nil {
		if mapped, ok := status.PortMappings["22"]; ok && mapped > 0 {
			port = mapped
		}
	}
	if status.PublicIP != "" {
		return net.JoinHostPort(status.PublicIP, fmt.Sprintf("%d", port)), nil
	}
	return "", fmt.Errorf("RunPod instance %s has no public IP yet", status.ID)
}

// RunInContainer implements kubectl exec via SSH using the kubelet-held key.
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	p.logger.Info("RunInContainer", "namespace", namespace, "pod", podName, "container", containerName, "cmd", cmd)
	if p.runpodClient.sshSigner == nil {
		return fmt.Errorf("exec SSH key is not available")
	}
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return fmt.Errorf("get pod for exec: %w", err)
	}
	podID := pod.Annotations[PodIDAnnotation]
	if podID == "" {
		return fmt.Errorf("pod %s/%s has no RunPod ID", namespace, podName)
	}
	var addr, user string
	deadline := time.Now().Add(45 * time.Second)
	for {
		addr, user, err = p.runpodClient.resolveSSHEndpoint(ctx, podID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for SSH endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	p.logger.Info("exec SSH endpoint ready", "pod", podName, "addr", addr, "user", user)
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(p.runpodClient.sshSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	defer conn.Close()
	session, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()
	if attach != nil {
		if attach.Stdout() != nil {
			session.Stdout = attach.Stdout()
		}
		if attach.Stderr() != nil {
			session.Stderr = attach.Stderr()
		}
		if attach.Stdin() != nil {
			session.Stdin = attach.Stdin()
		}
		if attach.TTY() {
			_ = session.RequestPty("xterm", 24, 80, ssh.TerminalModes{})
		}
	}
	run := strings.Join(cmd, " ")
	if run == "" {
		run = "sh"
	}
	done := make(chan error, 1)
	go func() { done <- session.Run(run) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// GetContainerLogs implements kubectl logs via RunPod v2 SSE.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	p.logger.Info("GetContainerLogs",
		"namespace", namespace,
		"pod", podName,
		"container", containerName,
		"follow", opts.Follow,
		"tail", opts.Tail)
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, fmt.Errorf("get pod for logs: %w", err)
	}
	podID := pod.Annotations[PodIDAnnotation]
	if podID == "" {
		return nil, fmt.Errorf("pod %s/%s has no RunPod ID annotation", namespace, podName)
	}
	tail := opts.Tail
	if tail <= 0 {
		if opts.Follow {
			tail = 100
		} else {
			tail = 1000
		}
	}
	stream, err := p.runpodClient.StreamPodLogs(ctx, podID, "container", tail, opts.SinceTime)
	if err != nil {
		return nil, err
	}
	out := newSSEToTextReader(stream)
	if !opts.Follow {
		return newIdleEOFReader(out, 1500*time.Millisecond, 4*time.Second), nil
	}
	return out, nil
}

func (c *Client) resolveSSHEndpoint(ctx context.Context, podID string) (addr, user string, err error) {
	if view, v2Err := c.getPodV2(ctx, podID); v2Err == nil {
		if addr, user, err = sshAddrFromV2(view); err == nil {
			return addr, user, nil
		}
	} else {
		err = v2Err
	}
	status, v1Err := c.GetDetailedPodStatus(podID)
	if v1Err != nil {
		if err != nil {
			return "", "", err
		}
		return "", "", v1Err
	}
	addr, v1Err = sshEndpoint(status)
	if v1Err != nil {
		if err != nil {
			return "", "", err
		}
		return "", "", v1Err
	}
	return addr, "root", nil
}

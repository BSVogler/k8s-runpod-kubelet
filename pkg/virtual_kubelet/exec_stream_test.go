package runpod

import "testing"

func TestFilterStreamProtocolsDropsV5(t *testing.T) {
	got := filterStreamProtocols("v5.channel.k8s.io,v4.channel.k8s.io,v3.channel.k8s.io")
	if got != "v4.channel.k8s.io,v3.channel.k8s.io" {
		t.Fatalf("got %q", got)
	}
}

func TestFilterStreamProtocolsFallsBackWhenOnlyV5(t *testing.T) {
	got := filterStreamProtocols("v5.channel.k8s.io")
	if got != "v4.channel.k8s.io,v3.channel.k8s.io,v2.channel.k8s.io,channel.k8s.io" {
		t.Fatalf("got %q", got)
	}
}

func TestIsExecOrAttachPath(t *testing.T) {
	if !isExecOrAttachPath("/exec/default/cpu-exec-probe/main") {
		t.Fatal("expected exec path")
	}
	if !isExecOrAttachPath("/attach/default/cpu-exec-probe/main") {
		t.Fatal("expected attach path")
	}
	if isExecOrAttachPath("/containerLogs/default/cpu-exec-probe/main") {
		t.Fatal("logs path should not be rewritten")
	}
}

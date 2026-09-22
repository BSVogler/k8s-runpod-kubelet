package runpod

import "testing"

func TestSSHAddrFromV2Direct(t *testing.T) {
	view := &v2PodView{
		ID: "abc",
		SSH: &struct {
			Direct *v2SSHEndpoint `json:"direct"`
			Proxy  *v2SSHEndpoint `json:"proxy"`
		}{
			Direct: &v2SSHEndpoint{Host: "195.26.233.3", Port: 34446, Username: "root"},
		},
	}
	addr, user, err := sshAddrFromV2(view)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "195.26.233.3:34446" || user != "root" {
		t.Fatalf("addr=%q user=%q", addr, user)
	}
}

func TestSSHAddrFromV2RuntimePort(t *testing.T) {
	pub := 22100
	view := &v2PodView{
		ID: "abc",
		Runtime: &struct {
			Ports []v2RuntimePort `json:"ports"`
		}{
			Ports: []v2RuntimePort{{Private: 22, Public: &pub, IP: "1.2.3.4", Type: "tcp"}},
		},
	}
	addr, user, err := sshAddrFromV2(view)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "1.2.3.4:22100" || user != "root" {
		t.Fatalf("addr=%q user=%q", addr, user)
	}
}

func TestSSHAddrFromV2NotReady(t *testing.T) {
	if _, _, err := sshAddrFromV2(&v2PodView{ID: "abc"}); err == nil {
		t.Fatal("expected not ready")
	}
}

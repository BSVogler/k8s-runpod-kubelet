package runpod

import (
	"crypto/tls"
	"os"
	"testing"
)

func TestWriteSelfSignedKubeletCert(t *testing.T) {
	dir := t.TempDir()
	cert, key, err := WriteSelfSignedKubeletCert(dir, "10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cert); err != nil {
		t.Fatal(err)
	}
}

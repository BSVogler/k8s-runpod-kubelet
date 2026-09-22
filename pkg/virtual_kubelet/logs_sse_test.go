package runpod

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestSSEToTextReader(t *testing.T) {
	raw := "event: log\ndata: {\"source\":\"container\",\"line\":\"hello\"}\n\ndata: {\"line\":\"world\"}\n"
	r := newSSEToTextReader(io.NopCloser(strings.NewReader(raw)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\nworld\n" {
		t.Fatalf("got %q", string(got))
	}
}

func TestIdleEOFReaderFinishesAfterBackfill(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, "data: {\"line\":\"boot\"}\n")
		// leave the writer open to mimic a live SSE stream
		time.Sleep(3 * time.Second)
		_ = pw.Close()
	}()
	r := newIdleEOFReader(newSSEToTextReader(pr), 200*time.Millisecond, 500*time.Millisecond)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "boot\n" {
		t.Fatalf("got %q", string(got))
	}
}

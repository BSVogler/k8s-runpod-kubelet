package runpod

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// sseToTextReader converts RunPod v2 SSE log lines into plain text for kubectl logs.
type sseToTextReader struct {
	src     *bufio.Scanner
	pending []byte
	err     error
	closed  io.Closer
}

func newSSEToTextReader(r io.ReadCloser) io.ReadCloser {
	return &sseToTextReader{src: bufio.NewScanner(r), closed: r}
}

func (r *sseToTextReader) Read(p []byte) (int, error) {
	if r.err != nil && len(r.pending) == 0 {
		return 0, r.err
	}
	for len(r.pending) == 0 {
		if !r.src.Scan() {
			if err := r.src.Err(); err != nil {
				r.err = err
			} else {
				r.err = io.EOF
			}
			if len(r.pending) == 0 {
				return 0, r.err
			}
			break
		}
		line := r.src.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev struct {
			Line string `json:"line"`
		}
		out := payload
		if err := json.Unmarshal([]byte(payload), &ev); err == nil && ev.Line != "" {
			out = ev.Line
		}
		r.pending = []byte(out + "\n")
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *sseToTextReader) Close() error {
	if r.closed != nil {
		return r.closed.Close()
	}
	return nil
}

// idleEOFReader closes after firstWait with no data, or idle after the last byte.
// kubectl logs without --follow needs EOF; RunPod SSE stays open.
func newIdleEOFReader(src io.ReadCloser, idle, firstWait time.Duration) io.ReadCloser {
	pr, pw := io.Pipe()
	go drainUntilIdle(src, pw, idle, firstWait)
	return struct {
		io.Reader
		io.Closer
	}{Reader: pr, Closer: closerFunc(func() error {
		_ = pr.Close()
		return src.Close()
	})}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

type readChunk struct {
	data []byte
	err  error
}

func drainUntilIdle(src io.ReadCloser, pw *io.PipeWriter, idle, firstWait time.Duration) {
	defer func() {
		_ = src.Close()
		_ = pw.Close()
	}()
	if firstWait <= 0 {
		firstWait = idle
	}
	buf := make([]byte, 4096)
	reads := make(chan readChunk, 1)
	reading := false
	startRead := func() {
		if reading {
			return
		}
		reading = true
		go func() {
			n, err := src.Read(buf)
			chunk := readChunk{err: err}
			if n > 0 {
				chunk.data = append([]byte(nil), buf[:n]...)
			}
			reads <- chunk
		}()
	}
	timer := time.NewTimer(firstWait)
	defer timer.Stop()
	gotData := false
	startRead()
	for {
		select {
		case chunk := <-reads:
			reading = false
			if len(chunk.data) > 0 {
				gotData = true
				if _, err := pw.Write(chunk.data); err != nil {
					return
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			}
			if chunk.err != nil {
				if chunk.err != io.EOF {
					_ = pw.CloseWithError(chunk.err)
				}
				return
			}
			startRead()
		case <-timer.C:
			if !gotData {
				return
			}
			return
		}
	}
}

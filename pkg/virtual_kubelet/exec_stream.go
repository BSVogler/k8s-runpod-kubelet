package runpod

import (
	"net/http"
	"strings"
)

const streamProtocolV5 = "v5.channel.k8s.io"

var supportedExecStreamProtocols = []string{
	"v4.channel.k8s.io",
	"v3.channel.k8s.io",
	"v2.channel.k8s.io",
	"channel.k8s.io",
}

// FilterExecStreamProtocols drops v5.channel.k8s.io so virtual-kubelet 1.9
// does not panic on a nil SPDY protocol handler.
func FilterExecStreamProtocols(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isExecOrAttachPath(r.URL.Path) {
			r = r.Clone(r.Context())
			if raw := r.Header.Get("X-Stream-Protocol-Version"); raw != "" {
				r.Header.Set("X-Stream-Protocol-Version", filterStreamProtocols(raw))
			}
			if raw := r.Header.Get("Sec-WebSocket-Protocol"); raw != "" {
				r.Header.Set("Sec-WebSocket-Protocol", filterStreamProtocols(raw))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isExecOrAttachPath(path string) bool {
	return strings.Contains(path, "/exec/") || strings.Contains(path, "/attach/")
}

func filterStreamProtocols(header string) string {
	kept := make([]string, 0, 4)
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if p == "" || p == streamProtocolV5 {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return strings.Join(supportedExecStreamProtocols, ",")
	}
	return strings.Join(kept, ",")
}

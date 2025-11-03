# Conduit Kubelet - Developer Documentation

**Technical documentation for developers working on the kubelet**

This document contains detailed technical information for contributors and developers. For end-user documentation, see [README.md](README.md).

## Architecture

```
K8s Cluster → Conduit Kubelet (open source) → Platform (your SaaS) → GPU Providers
```

**What the Kubelet Does:**
- Detects pod lifecycle events (created, deleted)
- Reports events to platform via WebSocket
- Executes commands from platform (deploy, terminate, status)
- Updates Kubernetes pod status
- Handles platform rejections (plan limits, quotas)

**What the Platform Does (Not This Project):**
- Analyzes pod requirements and selects provider
- Enforces plan limits and quotas
- Manages API keys and billing
- Optimizes costs across clusters and providers

**For detailed architecture**: See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

---

## Project Structure

```
conduit-kubelet/
├── cmd/virtual_kubelet/      # Main entry point
├── pkg/
│   ├── websocket/           # WebSocket client & protocol
│   ├── providers/           # Provider implementations
│   │   ├── runpod/         # RunPod client
│   │   ├── vastai/         # Vast.ai client
│   │   └── interface.go    # Provider interface
│   ├── command/            # Command handler
│   ├── config/             # Configuration
│   └── virtual_kubelet/    # K8s integration
├── deploy/                 # Kubernetes manifests
└── docs/                   # Documentation
```

---

## Development Setup

### Prerequisites

- Go 1.21+
- Kubernetes cluster (local or remote)
- Docker (for container builds)

### Building from Source

```bash
# Clone repository
git clone https://github.com/yourusername/conduit-kubelet
cd conduit-kubelet

# Install dependencies
go mod download

# Build binary
go build -o conduit-kubelet ./cmd/virtual_kubelet

# Run tests
go test ./...

# Run locally with kubeconfig
./conduit-kubelet \
  --kubeconfig=$HOME/.kube/config \
  --backend-url="wss://localhost:8000/api/kubelet/ws" \
  --backend-api-key="test-key" \
  --log-level=debug
```

### Running with Docker

```bash
# Build container
docker build -t conduit-kubelet:dev .

# Run container
docker run --rm \
  -e BACKEND_URL="wss://backend.example.com/api/kubelet/ws" \
  -e BACKEND_API_KEY="your-key" \
  -e RUNPOD_API_KEY="your-runpod-key" \
  conduit-kubelet:dev
```

---

## WebSocket Protocol

### Message Types

- **Commands** (Backend → Kubelet): `deploy`, `terminate`, `status`, `ping`
- **Events** (Kubelet → Backend): `pod_created`, `pod_status_change`, `pod_deleted`
- **Responses** (Kubelet → Backend): `result`, `error`

### Example Messages

**Deploy Command:**
```json
{
  "type": "deploy",
  "provider": "runpod",
  "params": {
    "api_key": "optional_platform_provided_key",
    "image": "nvidia/cuda:11.8-base-ubuntu22.04",
    "gpu_type": "RTX_4090",
    "gpu_count": 2
  }
}
```

**Status Command:**
```json
{
  "type": "status",
  "provider": "runpod",
  "params": {
    "provider_pod_id": "runpod-xyz-789"
  }
}
```

**Pod Created Event:**
```json
{
  "type": "pod_created",
  "pod_spec": { /* Full K8s pod spec */ },
  "annotations": { /* User preferences */ }
}
```

---

## API Key Management

### Flexible Key Sourcing

The kubelet accepts provider API keys from multiple sources with this priority:

1. **Command parameters** (from platform) - Used if provided in command
2. **Local environment** - Falls back to `RUNPOD_API_KEY` env var
3. **Error** - Fails if neither exists

### Implementation

In `pkg/providers/runpod/client.go`:

```go
func (c *Client) getAPIKey(paramKey string) string {
    // Use key from command params if provided (platform-managed)
    if paramKey != "" {
        return paramKey
    }
    
    // Fall back to client's configured key
    if c.apiKey != "" {
        return c.apiKey
    }
    
    // Last resort: check environment
    return os.Getenv("RUNPOD_API_KEY")
}
```

---

## Adding a New Provider

### 1. Create Provider Directory

```bash
mkdir pkg/providers/newprovider
```

### 2. Implement Provider Interface

```go
// pkg/providers/newprovider/client.go
package newprovider

import (
    "context"
    "github.com/bsvogler/conduit-kubelet/pkg/providers"
    "github.com/bsvogler/conduit-kubelet/pkg/websocket"
)

type Client struct {
    apiKey string
}

func NewClient(apiKey string) *Client {
    return &Client{apiKey: apiKey}
}

func (c *Client) GetName() string {
    return "newprovider"
}

func (c *Client) Deploy(ctx context.Context, params *websocket.DeployParams) (*websocket.DeployResult, error) {
    // Get API key with fallback
    apiKey := c.getAPIKey(params.APIKey)
    if apiKey == "" {
        return nil, providers.NewProviderError("newprovider", "missing_api_key",
            "no API key provided", false)
    }
    
    // Implement deployment logic
    // ...
}

func (c *Client) GetStatus(ctx context.Context, params *websocket.StatusParams) (*websocket.StatusResult, error) {
    // Implement status check
    // ...
}

func (c *Client) Terminate(ctx context.Context, params *websocket.TerminateParams) error {
    // Implement termination
    // ...
}

func (c *Client) Ping(ctx context.Context) error {
    // Test connectivity
    // ...
}

func (c *Client) getAPIKey(paramKey string) string {
    if paramKey != "" {
        return paramKey
    }
    if c.apiKey != "" {
        return c.apiKey
    }
    return os.Getenv("NEWPROVIDER_API_KEY")
}
```

### 3. Register Provider

In `cmd/virtual_kubelet/main.go`:

```go
// Add to initializeProviders()
if strings.Contains(cfg.Providers.EnabledProviders, "newprovider") {
    apiKey := os.Getenv("NEWPROVIDER_API_KEY")
    newProviderClient := newprovider.NewClient(apiKey)
    if err := providerManager.RegisterProvider(newProviderClient); err != nil {
        return nil, fmt.Errorf("failed to register newprovider: %w", err)
    }
}
```

### 4. Update Configuration

Add environment variable support in `pkg/config/config.go` if needed.

---

## Configuration

### Environment Variables

**Required:**
- `BACKEND_URL` - WebSocket URL for platform
- `BACKEND_API_KEY` - Platform authentication token

**Provider Keys (optional):**
- `RUNPOD_API_KEY` - RunPod API key
- `VASTAI_API_KEY` - Vast.ai API key
- `SALAD_API_KEY` - Salad API key

**Optional:**
- `NODE_NAME` - Kubernetes node name (default: "virtual-proxy")
- `NAMESPACE` - Kubernetes namespace (default: "kube-system")
- `LOG_LEVEL` - Logging level: debug, info, warn, error (default: "info")

### Command Line Flags

```bash
./conduit-kubelet --help

Flags:
  --kubeconfig string              Path to kubeconfig file
  --backend-url string             Backend WebSocket URL
  --backend-api-key string         Backend authentication key
  --nodename string                Node name for Kubernetes (default "virtual-proxy")
  --operating-system string        Operating system (default "Linux")
  --internal-ip string             Internal IP address (default "127.0.0.1")
  --listen-port int                Port to listen on (default 10250)
  --log-level string               Log level (default "info")
  --health-server-address string   Address for health checks (default ":8080")
  --namespace string               Kubernetes namespace (default "kube-system")
  --reconcile-interval int         Reconcile interval in seconds (default 30)
  --enabled-providers string       Comma-separated provider list (default "runpod")
```

---

## Testing

### Unit Tests

```bash
# Run all tests
go test ./...

# Test specific package
go test ./pkg/websocket -v

# Test with coverage
go test -cover ./...

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Integration Tests

```bash
# Set up test environment
export BACKEND_API_KEY=test-key
export RUNPOD_API_KEY=your-test-key

# Run integration tests
go test -tags=integration ./...
```

### Manual Testing

```bash
# Start local backend mock (separate project)
cd ../conduit-backend
go run cmd/mock-backend/main.go

# Run kubelet with debug logging
./conduit-kubelet \
  --kubeconfig=$HOME/.kube/config \
  --backend-url="ws://localhost:8000/api/kubelet/ws" \
  --backend-api-key="test-key" \
  --log-level=debug

# Deploy test pod
kubectl apply -f examples/test-pod.yaml

# Watch logs
kubectl logs -n kube-system -l app=conduit-kubelet -f
```

---

## Debugging

### Enable Debug Logging

```bash
# Via flag
./conduit-kubelet --log-level=debug

# Via environment
export LOG_LEVEL=debug
./conduit-kubelet

# In Kubernetes
kubectl set env deployment/conduit-kubelet -n kube-system LOG_LEVEL=debug
```

### Debug WebSocket Communication

```bash
# Filter for WebSocket messages
kubectl logs -n kube-system -l app=conduit-kubelet | grep -E "(WebSocket|Command|Response)"

# Monitor specific message types
kubectl logs -n kube-system -l app=conduit-kubelet | grep -E "(deploy|terminate)"
```

### Debug Provider Calls

```bash
# Check provider connectivity
curl http://localhost:8080/status | jq '.providers'

# View detailed status
curl http://localhost:8080/status | jq '.'

# Test provider ping
kubectl exec -n kube-system deployment/conduit-kubelet -- \
  curl localhost:8080/healthz
```

### Common Issues

**WebSocket Connection Failed:**
```bash
# Check logs for connection errors
kubectl logs -n kube-system -l app=conduit-kubelet | grep -i "websocket\|connection"

# Verify backend URL
kubectl get deployment conduit-kubelet -n kube-system -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BACKEND_URL")].value}'
```

**Provider API Errors:**
```bash
# Check if API keys are set
kubectl get secret conduit-provider-keys -n kube-system -o json | jq '.data'

# Test provider directly (requires API key)
curl -H "Authorization: Bearer $RUNPOD_API_KEY" https://api.runpod.io/v2/gpuTypes
```

---

## Health Checks

The kubelet exposes health endpoints on port 8080:

```bash
# Liveness probe
curl http://localhost:8080/healthz

# Readiness probe
curl http://localhost:8080/readyz

# Detailed status
curl http://localhost:8080/status | jq '.'
```

---

## Performance Considerations

### Resource Usage

- **CPU**: Minimal (~10m under load)
- **Memory**: ~50-100MB depending on pod count
- **Network**: Event-driven, low bandwidth

### Scaling

- Each kubelet manages pods on a single K8s node
- Deploy multiple kubelets for multi-node clusters
- WebSocket connection per kubelet instance

---

## Security

### API Key Security

- Keys never logged (sanitized in logs)
- Platform-managed keys: encrypted in transit (WSS)
- Local keys: stored as K8s secrets
- Keys used in-memory only, never written to disk

### Network Security

- Kubelet initiates outbound connections only
- No inbound ports exposed (except health checks)
- WebSocket uses TLS (wss://) in production
- Provider API calls from kubelet's network namespace

### Code Audit

```bash
# Check for logged secrets
grep -r "logger.*api.*key" pkg/

# Review WebSocket protocol
cat pkg/websocket/protocol.go

# Review provider implementations
ls -la pkg/providers/*/client.go
```

---

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) - Detailed architecture
- [`docs/CURRENT_STATE.md`](docs/CURRENT_STATE.md) - Implementation status
- [`docs/PRODUCTION_GAPS.md`](docs/PRODUCTION_GAPS.md) - Production gaps
- [`docs/PRODUCTION_CHECKLIST.md`](docs/PRODUCTION_CHECKLIST.md) - Go-live checklist
- [`CLAUDE.md`](CLAUDE.md) - Claude Code guidelines

---

## Contributing

See [README.md](README.md#contributing) for contribution guidelines.

---

## License

MIT License - see [LICENSE](LICENSE) file for details.

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with the k8s-proxy-kubelet repository.

## Project Overview

This is a command-and-control virtual kubelet that acts as a proxy between Kubernetes and multiple GPU cloud providers. Unlike traditional virtual kubelets that make provider API calls automatically, this kubelet only executes provider calls when instructed by a backend service via WebSocket commands. This architecture enables centralized routing decisions while keeping API keys secure in-cluster.

## Architecture

### Key Components

**WebSocket Communication** (`pkg/websocket/`)
- `protocol.go`: Message types and data structures for WebSocket communication
- `client.go`: WebSocket client with auto-reconnection and message handling

**Provider System** (`pkg/providers/`)
- `interface.go`: Abstract provider interface for all cloud providers
- `manager.go`: Multi-provider manager for routing commands
- `runpod/client.go`: RunPod API integration (ported from original kubelet)

**Command Processing** (`pkg/command/`)
- `handler.go`: Processes WebSocket commands from backend and executes provider calls

**Virtual Kubelet** (`pkg/virtual_kubelet/`)
- `kubelet.go`: Main kubelet provider implementing command-driven architecture
- `health.go`: Health check endpoints

**Main Entry** (`cmd/virtual_kubelet/`)
- `main.go`: Application entry point with configuration and controller setup

### Data Flow

1. **Pod Creation**: K8s schedules pod → Kubelet reports to backend via WebSocket
2. **Backend Decision**: Backend analyzes pod requirements and selects provider
3. **Command Execution**: Backend sends deploy command → Kubelet executes RunPod/Vast.ai API call
4. **Status Updates**: Kubelet reports results and updates K8s pod status

## Build and Development Commands

### Build Commands
```bash
# Build the main binary
go build -o proxy-kubelet ./cmd/virtual_kubelet

# Build for testing locally
go build -o proxy-kubelet-dev ./cmd/virtual_kubelet

# Run locally with kubeconfig
./proxy-kubelet --kubeconfig=$HOME/.kube/config \
  --backend-url="wss://localhost:8000/api/kubelet/ws" \
  --backend-api-key="test-key"

# Run with debug logging
./proxy-kubelet --log-level=debug \
  --nodename=proxy-test \
  --namespace=default
```

### Testing Commands
```bash
# Unit tests
go test ./...

# Test specific package
go test ./pkg/websocket -v

# Test with coverage
go test -cover ./...

# Integration tests (requires backend service and provider API keys)
export BACKEND_API_KEY=test-key
export RUNPOD_API_KEY=your-key
go test -tags=integration ./...
```

### Container Commands
```bash
# Build container image
docker build -t k8s-proxy-kubelet:latest .

# Run in container (requires backend service)
docker run --rm \
  -e BACKEND_URL="wss://backend.example.com/api/kubelet/ws" \
  -e BACKEND_API_KEY="your-key" \
  -e RUNPOD_API_KEY="your-runpod-key" \
  k8s-proxy-kubelet:latest
```

## Key Differences from Original Kubelet

### Command-Driven vs Event-Driven
- **Original**: Automatically deploys pods when K8s schedules them
- **Proxy**: Reports pod events to backend, waits for deployment commands

### API Key Storage
- **Original**: Provider keys stored in kubelet environment
- **Proxy**: Keys stored in-cluster, backend uses separate authentication

### Provider Selection
- **Original**: Uses annotations and availability checks locally
- **Proxy**: Backend makes all routing decisions centrally

### WebSocket Communication
- **Original**: Direct HTTP/GraphQL calls to providers
- **Proxy**: WebSocket-based command protocol with backend

## Configuration

### Environment Variables

**Required:**
- `BACKEND_URL`: WebSocket URL for backend (e.g., "wss://gpuconduit.io/api/kubelet/ws")
- `BACKEND_API_KEY`: Authentication token for backend service

**Provider Keys (at least one required):**
- `RUNPOD_API_KEY`: RunPod API key
- `VASTAI_API_KEY`: Vast.ai API key (when implemented)
- `SALAD_API_KEY`: Salad API key (when implemented)

**Optional:**
- `NODE_NAME`: Kubernetes node name (default: "virtual-proxy")
- `NAMESPACE`: Kubernetes namespace (default: "kube-system")
- `LOG_LEVEL`: Logging level (default: "info")

### Command Line Flags
- `--kubeconfig`: Path to kubeconfig file
- `--backend-url`: Backend WebSocket URL
- `--backend-api-key`: Backend authentication key
- `--nodename`: Node name for Kubernetes
- `--log-level`: Set to "debug" for WebSocket message tracing

## WebSocket Protocol

### Message Types
- **Commands** (Backend → Kubelet): `deploy`, `terminate`, `status`, `ping`
- **Events** (Kubelet → Backend): `pod_created`, `pod_status_change`, `pod_deleted`
- **Responses** (Kubelet → Backend): `result`, `error`

### Debug WebSocket Communication
```bash
# Enable debug logging to see WebSocket messages
./proxy-kubelet --log-level=debug 2>&1 | grep -E "(WebSocket|Command|Response)"

# Monitor specific message types
./proxy-kubelet --log-level=debug 2>&1 | grep -E "(deploy|terminate)"
```

## Provider Implementation

### Adding New Providers
1. Create provider directory: `pkg/providers/newprovider/`
2. Implement `providers.Provider` interface
3. Add provider initialization in `kubelet.go:initializeProviders()`
4. Update configuration structs in `pkg/config/config.go`

### Provider Interface Methods
```go
type Provider interface {
    GetName() string
    Deploy(ctx context.Context, params *websocket.DeployParams) (*websocket.DeployResult, error)
    GetStatus(ctx context.Context, providerPodID string) (*websocket.StatusResult, error)
    Terminate(ctx context.Context, providerPodID string) error
    GetPricing(ctx context.Context) (*PricingResult, error)
    GetAvailability(ctx context.Context, query *AvailabilityQuery) (*AvailabilityResult, error)
    Ping(ctx context.Context) error
}
```

## Debugging

### Common Issues

**WebSocket Connection Problems:**
```bash
# Check WebSocket connectivity
./proxy-kubelet --log-level=debug --backend-url="wss://backend.test.com/api/kubelet/ws"

# Look for connection errors
grep -i "websocket\|connection" kubelet.log
```

**Provider API Issues:**
```bash
# Test provider connectivity separately
export RUNPOD_API_KEY=your-key
go run ./cmd/test-provider --provider=runpod --operation=ping

# Debug provider calls
./proxy-kubelet --log-level=debug 2>&1 | grep -A5 -B5 "provider.*error"
```

**Command Processing Issues:**
```bash
# Monitor command flow
./proxy-kubelet --log-level=debug 2>&1 | grep -E "(Processing command|Command execution)"

# Check for command timeouts
grep -i timeout kubelet.log
```

### Health Checks
```bash
# Check kubelet health
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/status

# Verify provider connectivity
curl http://localhost:8080/status | jq '.providers'
```

## Backend Integration

### Required Backend Endpoints
The backend service must implement:
- `WebSocket /api/kubelet/ws` - Command and event processing
- `PUT /api/kubelet/register` - Kubelet registration
- `GET /api/health` - Backend health check

### Backend Responsibilities
- Pod scheduling decisions and provider selection
- Command queuing and delivery to kubelets
- Cost optimization across multiple clusters
- Provider availability and pricing tracking

## Security Considerations

### API Key Management
- Provider API keys stored as Kubernetes secrets in kubelet namespace
- Backend uses separate authentication (BACKEND_API_KEY)
- WebSocket connections use TLS (wss://) in production

### Network Security
- Kubelet initiates outbound WebSocket connections (no inbound ports)
- Provider API calls originate from kubelet (backend never sees provider keys)
- Health checks available on configurable port (default :8080)

## Performance Notes

### WebSocket Connection
- Auto-reconnection with exponential backoff
- Command queuing during disconnections
- Heartbeat/ping mechanism to maintain connection

### Resource Usage
- Minimal CPU usage (event-driven architecture)
- Memory scales with number of managed pods
- Network usage depends on command frequency

## Deployment

### Kubernetes Deployment
```bash
# Apply RBAC and deployment
kubectl apply -f deploy/kubelet.yaml

# Check deployment status
kubectl get pods -n kube-system -l app=proxy-kubelet

# View logs
kubectl logs -n kube-system -l app=proxy-kubelet -f
```

### Helm Deployment (Future)
```bash
helm install gpu-proxy ./helm/proxy-kubelet \
  --set config.backendURL="wss://your-backend.com/api/kubelet/ws" \
  --set config.backendAPIKey="your-api-key"
```

## Development Workflow

1. **Local Development**: Run kubelet locally with kubeconfig and test backend
2. **Provider Testing**: Use individual provider clients to test API integration
3. **WebSocket Testing**: Use WebSocket debugging tools to test protocol
4. **Integration Testing**: Deploy with test backend service
5. **Production**: Deploy via Helm with proper secrets management

## Related Components

- **Backend Service**: Handles command routing and provider selection
- **Original Kubelet**: `k8s-runpod-kubelet` for direct RunPod integration
- **Virtual Kubelet Framework**: Upstream framework for Kubernetes integration
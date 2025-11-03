# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with the conduit-kubelet repository.

## Project Overview

Conduit Kubelet is an **open-source virtual Kubernetes kubelet** that acts as a pure proxy between Kubernetes clusters and GPU cloud providers. It is designed for a **SaaS platform architecture** where all routing intelligence, cost optimization, and provider selection logic resides in a proprietary platform service.

**Architecture Principle:** The kubelet is a **pure command executor** with no business logic. The platform makes all decisions.

### Key Characteristics

- **Command-and-Control**: Kubelet only executes commands from the platform, never makes routing decisions
- **Event-Driven**: Reports all Kubernetes pod lifecycle events to the platform
- **Dual Key Mode**: Supports both platform-managed API keys (SaaS) and local keys (self-hosted)
- **Open Source**: This kubelet is open source; the platform is proprietary (your business)

### What Kubelet Does

1. Detects Kubernetes pod events (created, deleted)
2. Reports events to platform via WebSocket
3. Executes commands from platform (deploy, terminate, status)
4. Updates Kubernetes pod status based on results
5. Handles platform rejections (plan limits, quotas)

### What Kubelet Does NOT Do

- ❌ Provider selection or routing decisions
- ❌ Cost optimization or pricing queries
- ❌ Availability checking across providers
- ❌ Plan limit enforcement (platform's job)
- ❌ User quota management

**For detailed architecture, see `docs/ARCHITECTURE.md`**

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

### Data Flow (SaaS Mode)

1. **Event Detection**: K8s schedules pod → Kubelet sends `pod_created` event to platform
2. **Platform Intelligence**: Platform analyzes requirements, checks quotas, selects provider
3. **Command Execution**: Platform sends `deploy` command with provider name + API key → Kubelet executes provider API call
4. **Status Updates**: Kubelet returns result → Platform tracks cost → Kubelet updates K8s pod status
5. **Rejection Handling**: If platform rejects (plan limits, quota), kubelet creates K8s event and keeps pod pending

**Terminology:**
- **Platform** = Proprietary SaaS backend service (your business logic)
- **Providers** = GPU cloud providers (RunPod, Vast.ai, Salad, etc.)
- **Kubelet** = This open-source proxy (conduit-kubelet)

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

## API Key Management

Conduit Kubelet supports two modes for API key management:

### Mode 1: Platform-Managed Keys (SaaS)
- User adds provider API keys to platform web UI
- Platform stores keys encrypted
- Platform includes key in each command
- Kubelet never stores keys locally
- **Use Case:** Multi-tenant SaaS deployments

### Mode 2: Local Keys (Self-Hosted)
- Provider API keys stored as Kubernetes secrets
- Mounted to kubelet pod as environment variables
- Platform sends commands WITHOUT keys
- Kubelet falls back to local environment
- **Use Case:** Single-tenant, self-hosted deployments

### Implementation
```go
// Providers check for key in command params first
apiKey := params.APIKey  // From platform (SaaS mode)
if apiKey == "" {
    apiKey = os.Getenv("RUNPOD_API_KEY")  // Fall back to local (self-hosted mode)
}
```

**Security:** Keys are never logged, always transmitted over TLS (WSS://)

## Configuration

### Environment Variables

**Required:**
- `BACKEND_URL`: WebSocket URL for platform (e.g., "wss://platform.example.com/api/kubelet/ws")
- `BACKEND_API_KEY`: Authentication token for platform service

**Provider Keys (optional - only for self-hosted mode):**
- `RUNPOD_API_KEY`: RunPod API key (optional if platform provides keys)
- `VASTAI_API_KEY`: Vast.ai API key (optional if platform provides keys)
- `SALAD_API_KEY`: Salad API key (optional if platform provides keys)

**Optional:**
- `NODE_NAME`: Kubernetes node name (default: "conduit-kubelet")
- `NAMESPACE`: Kubernetes namespace (default: "kube-system")
- `LOG_LEVEL`: Logging level (default: "info")
- `KEY_MODE`: Key management mode - "local", "platform", or "hybrid" (default: "hybrid")

### Command Line Flags
- `--kubeconfig`: Path to kubeconfig file
- `--backend-url`: Backend WebSocket URL
- `--backend-api-key`: Backend authentication key
- `--nodename`: Node name for Kubernetes
- `--log-level`: Set to "debug" for WebSocket message tracing

## WebSocket Protocol

### Message Types
- **Commands** (Platform → Kubelet): `deploy`, `terminate`, `status`, `ping`
- **Events** (Kubelet → Platform): `pod_created`, `pod_deleted`, `kubelet_registration`
- **Responses** (Kubelet → Platform): `result`, `error`
- **Rejections** (Platform → Kubelet): `rejection` with error codes (plan limits, quota, credits)

### Rejection Handling
When the platform rejects a deployment (e.g., plan limit exceeded), the kubelet:
1. Receives rejection message with error code and remediation info
2. Creates Kubernetes Event with reason and message
3. Keeps pod in Pending state (does not fail it)
4. User sees event in `kubectl describe pod`

**Common Rejection Codes:**
- `PLAN_LIMIT_EXCEEDED` - User at concurrent pod limit
- `QUOTA_EXCEEDED` - User/team quota exceeded
- `INSUFFICIENT_CREDITS` - Not enough balance
- `COST_LIMIT_EXCEEDED` - Pod exceeds cost limits

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

    // ⚠️ DEPRECATED: GetPricing and GetAvailability should be called by platform, not kubelet
    // These methods are retained for backward compatibility and will be removed in v2.0
    GetPricing(ctx context.Context) (*PricingResult, error)         // Deprecated
    GetAvailability(ctx context.Context, query *AvailabilityQuery) (*AvailabilityResult, error)  // Deprecated

    Ping(ctx context.Context) error
}
```

**Note:** In SaaS mode, the platform queries provider pricing/availability directly. The kubelet only executes deploy/terminate/status commands.

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

## Platform Integration

### Required Platform Endpoints
The platform service must implement:
- `WebSocket /api/kubelet/ws` - Command and event processing (TLS required)
- Authentication via `BACKEND_API_KEY` header or query param

### Platform Responsibilities (All Intelligence)
- **Pod requirement analysis** - Parse K8s pod specs for GPU/memory/disk needs
- **Provider selection** - Choose optimal provider based on cost, availability, region
- **Cost optimization** - Minimize costs across multiple clusters and providers
- **Plan enforcement** - Enforce concurrent pod limits (free/pro/enterprise plans)
- **Quota management** - Track and enforce user/team resource quotas
- **API key management** - Store and securely provide provider API keys
- **Billing & usage tracking** - Track costs per user, generate invoices
- **Multi-cluster routing** - Optimize pod placement across multiple Kubernetes clusters

### Platform → Kubelet Communication
- Send `deploy` commands with provider name + API key (if platform-managed)
- Send `terminate` commands on pod deletion or quota enforcement
- Send `status` commands to poll provider status
- Send `rejection` messages when plan limits/quotas exceeded

### Kubelet → Platform Communication
- Send `pod_created` events with full pod spec + annotations
- Send `pod_deleted` events when K8s deletes pod
- Send `kubelet_registration` on startup with capabilities
- Send `result` responses with deployment outcomes
- Send `error` responses on failures

## Security Considerations

### API Key Management
**SaaS Mode (Platform-Managed):**
- User API keys stored encrypted in platform
- Platform includes keys in deploy commands
- Kubelet never stores keys locally (only in memory during execution)
- Keys transmitted over TLS (WSS://)

**Self-Hosted Mode (Local Keys):**
- Provider API keys stored as Kubernetes secrets
- Mounted to kubelet pod as environment variables
- Platform never sees provider keys

**Both Modes:**
- Kubelet authenticates to platform with `BACKEND_API_KEY`
- WebSocket connections use TLS (wss://) in production
- Keys are never logged

### Network Security
- Kubelet initiates outbound WebSocket connections (no inbound ports required)
- Provider API calls originate from kubelet (in-cluster, not from platform)
- Health checks available on configurable port (default :8080)
- All WebSocket traffic encrypted (TLS/WSS)

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

## Documentation Structure

For comprehensive documentation, see:

- **`docs/CURRENT_STATE.md`** - Current implementation status (what exists today)
- **`docs/ARCHITECTURE.md`** - Target SaaS architecture (detailed design)
- **`docs/PRODUCTION_GAPS.md`** - Missing features for production launch
- **`docs/PRODUCTION_CHECKLIST.md`** - Go-live checklist
- **`README.md`** - Project overview and quick start

## Related Components

- **Platform Service**: Proprietary SaaS backend (your business logic)
- **GPU Providers**: RunPod, Vast.ai, Salad, AWS, GCP
- **Original Kubelet**: `k8s-runpod-kubelet` (predecessor - direct provider integration)
- **Virtual Kubelet Framework**: Upstream Kubernetes integration framework

## Historical Context

This project evolved from a direct virtual kubelet (`k8s-runpod-kubelet`) that made provider API calls automatically. The architecture was refactored to a command-and-control model to support:
- Centralized multi-cluster routing optimization
- Secure API key management (platform-side)
- Open-source kubelet + proprietary platform business model
- Easy provider addition without kubelet changes

Some routing logic (`GetPricing`, `GetAvailability`) remains from the original implementation and is deprecated for SaaS mode.
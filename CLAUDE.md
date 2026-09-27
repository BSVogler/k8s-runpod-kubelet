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
5. **Rejection Handling**: If platform rejects (plan limits, quota, no key), kubelet marks the pod Failed with the reject code and message

**Terminology:**
- **Platform** = Proprietary SaaS backend service (your business logic)
- **Providers** = GPU cloud providers (RunPod, Vast.ai, Salad, etc.)
- **Kubelet** = This open-source proxy (conduit-kubelet)

## Build and Development Commands

### Build Commands
```bash
# Build the main binary (version is injected via ldflags; printed at startup and by --version)
go build -ldflags "-X main.version=$(git describe --tags --always)" -o conduit-kubelet ./cmd/virtual_kubelet

# Run locally with kubeconfig
./conduit-kubelet --kubeconfig=$HOME/.kube/config \
  --backend-url="ws://localhost:8010/api/kubelet/ws" \
  --backend-api-key="test-key"

# Run with debug logging
./conduit-kubelet --log-level=debug \
  --nodename=conduit-test \
  --namespace=default

# Formatting must be clean (CI checks gofmt -l)
gofmt -l .
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
The `Dockerfile` is multi-stage (golang builder → `gcr.io/distroless/static:nonroot`, CGO disabled, uid 65532) and honours `TARGETOS`/`TARGETARCH` for buildx multi-arch builds. Published image: `ghcr.io/bsvogler/conduit-kubelet` (`latest` on the default branch, `<git tag>` on releases, `<sha>` always).

```bash
# Build container image
docker build --build-arg VERSION=$(git describe --tags --always) -t conduit-kubelet:dev .

# Run in container (requires backend service)
docker run --rm \
  -e BACKEND_URL="wss://gpuconduit.io/api/kubelet/ws" \
  -e BACKEND_API_KEY="your-token" \
  -e RUNPOD_API_KEY="your-runpod-key" \
  conduit-kubelet:dev
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
- `CLUSTER_NAME`: Cluster name reported to the platform (default: "default")
- `NODE_NAME`: Kubernetes node name (default: "conduit-node")
- `NAMESPACE`: Kubernetes namespace (default: "kube-system")
- `LOG_LEVEL`: Logging level (default: "info")

The key mode reported in the registration is derived: "local" if any provider key is configured, otherwise "platform".

### Command Line Flags
- `--kubeconfig`: Path to kubeconfig file
- `--backend-url`: Backend WebSocket URL
- `--backend-api-key`: Backend authentication key
- `--nodename`: Node name for Kubernetes
- `--cluster-name`: Cluster name reported to the platform
- `--namespace`, `--health-server-address`, `--reconcile-interval`
- `--log-level`: Set to "debug" for WebSocket message tracing
- `--version`: Print the build version and exit

Flags default to the zero value so environment variables are not overridden; precedence is flag > env > `config.DefaultConfig()`.

### The Virtual Node
`Provider.ConfigureNode` (`pkg/virtual_kubelet/kubelet.go`) is called from `main.go` before the node controller starts. It sets labels `conduit.io/provider=true`, `type=virtual-kubelet`, `kubernetes.io/role=agent`, `kubernetes.io/hostname=<nodeName>` and the taint `virtual-kubelet.io/provider=conduit:NoSchedule`. Pods need a matching toleration (and normally the nodeSelector) to be scheduled onto the node; the README's first-pod example must stay in sync with these constants. virtual-kubelet only applies labels/taints when it creates the node object, not on updates.

## WebSocket Protocol

Protocol v2 (`pkg/websocket/protocol.go`, mirrored by `conduit-service/src/conduit/websocket/protocol.py`). Every frame is an envelope `{id, type, payload, timestamp, kubelet_id}`; `kubelet_id` is the backend API token because the service identifies kubelets by token.

### Envelope Types
- **`command`** (Platform → Kubelet): payload `{type: deploy|terminate|status|ping|reject, data}`; envelope `id` is the command id
- **`response`** (Kubelet → Platform): `{command_id, status: "success"|"error", data|null, error: {code, message, provider_error?}|null}`
- **`event`** (Kubelet → Platform): `{type: pod_created|pod_deleted|pod_status_change|kubelet_ready|kubelet_error, data}`; `pod_created` carries `pod_name, namespace, uid, pod_spec, annotations`
- **`heartbeat`** (Kubelet → Platform, every 30s): `{timestamp, kubelet_id, status: "alive"}`
- **`kubelet_registration`** (Kubelet → Platform, after every connect): `{id, type: "conduit-kubelet", cluster_name, node_name, namespace, capabilities, metadata: {version, internal_ip, key_mode}}`

### Rejection Handling
When the platform refuses a pod it sends a `reject` command (`{pod_name, namespace, code, message, upgrade_url?}`). The kubelet:
1. Sets the K8s pod phase to `Failed` with `reason` = code and `message` = message
2. Responds `{"status": "success", "data": {}}`
3. User sees the reason and message in `kubectl describe pod`

**Reject Codes:** `quota_exceeded`, `no_api_key`, `unsupported_provider`, `provider_error`, `conversion_error`

### Debug WebSocket Communication
```bash
# Enable debug logging to see WebSocket messages
./conduit-kubelet --log-level=debug 2>&1 | grep -E "(WebSocket|Command|Response)"

# Monitor specific message types
./conduit-kubelet --log-level=debug 2>&1 | grep -E "(deploy|terminate)"
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
./conduit-kubelet --log-level=debug --backend-url="wss://backend.test.com/api/kubelet/ws"

# Look for connection errors
grep -i "websocket\|connection" kubelet.log
```

**Provider API Issues:**
```bash
# Debug provider calls
./conduit-kubelet --log-level=debug 2>&1 | grep -A5 -B5 "provider.*error"
```

**Command Processing Issues:**
```bash
# Monitor command flow
./conduit-kubelet --log-level=debug 2>&1 | grep -E "(Processing command|Command execution)"

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
- Send `reject` commands when plan limits/quotas exceeded or conversion/provider errors occur

### Kubelet → Platform Communication
- Send `pod_created` events with full pod spec + annotations
- Send `pod_deleted` events when K8s deletes pod
- Send `kubelet_registration` after every connect with capabilities and key mode
- Send `heartbeat` every 30s
- Send `response` envelopes (`status: success|error`) with command outcomes

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

### Helm Chart
The chart lives in `deploy/helm/conduit-kubelet/` and is published as `oci://ghcr.io/bsvogler/helm/conduit-kubelet` (chart version = git tag without `v`, appVersion = git tag). Default namespace in docs is `conduit-system`.

Values → env mapping (`templates/deployment.yaml`): `conduit.url`→`BACKEND_URL`, `conduit.apiToken`→Secret key `CONDUIT_API_TOKEN`→`BACKEND_API_KEY`, `runpod.apiKey`→Secret key `RUNPOD_API_KEY`→`RUNPOD_API_KEY` (optional), `cluster.name`→`CLUSTER_NAME`, `kubelet.nodeName`→`NODE_NAME`, `kubelet.namespace`→`NAMESPACE`, `kubelet.logLevel`→`LOG_LEVEL`; `kubelet.healthServerAddress` and `kubelet.reconcileInterval` are passed as flags. `conduit.existingSecret` replaces the chart-managed Secret (same keys). Rendering fails unless `conduit.apiToken` or `conduit.existingSecret` is set.

```bash
helm lint deploy/helm/conduit-kubelet --set conduit.apiToken=x
helm template ck deploy/helm/conduit-kubelet -n conduit-system --set conduit.apiToken=x

helm upgrade --install conduit-kubelet deploy/helm/conduit-kubelet \
  --namespace conduit-system --create-namespace \
  --set conduit.apiToken="your-token" [--set runpod.apiKey="your-runpod-key"]

kubectl -n conduit-system get pods
kubectl get node conduit-node
kubectl -n conduit-system logs deploy/conduit-kubelet -f
```

### CI (`.github/workflows/`)
- `build.yml`: gofmt/vet/test, helm lint, multi-arch image push to ghcr.io on push to `master`/`main` and tags (PRs build without push)
- `helm-publish.yml`: packages and pushes the chart on tags/releases/workflow_dispatch
- `release.yml`: builds linux/darwin amd64/arm64 binaries with `-X main.version=<tag>`, attaches them plus `checksums.txt` to the GitHub release

## Development Workflow

1. **Local Development**: Run kubelet locally with kubeconfig and test backend
2. **Provider Testing**: Use individual provider clients to test API integration
3. **WebSocket Testing**: Use WebSocket debugging tools to test protocol
4. **Integration Testing**: Deploy with test backend service
5. **Production**: Deploy via Helm with proper secrets management

## Documentation Structure

For comprehensive documentation, see:

- **`README.md`** - Public documentation: quick start, configuration, annotations, license
- **`README.dev.md`** - Developer notes
- **`docs/ARCHITECTURE.md`** - Target SaaS architecture (detailed design)
- **`LICENSE`** - PolyForm Strict 1.0.0 (source-available; commercial use via the Conduit platform)

## Related Components

- **Platform Service**: Proprietary SaaS backend (your business logic)
- **GPU Providers**: RunPod, Vast.ai, Salad, AWS, GCP
- **Original Kubelet**: `k8s-runpod-kubelet` (predecessor - direct provider integration; kept on the `legacy` branch / tag `v1-legacy` of the public repo, unsupported)
- **Virtual Kubelet Framework**: Upstream Kubernetes integration framework

## Historical Context

This project evolved from a direct virtual kubelet (`k8s-runpod-kubelet`) that made provider API calls automatically. The architecture was refactored to a command-and-control model to support:
- Centralized multi-cluster routing optimization
- Secure API key management (platform-side)
- Open-source kubelet + proprietary platform business model
- Easy provider addition without kubelet changes

Some routing logic (`GetPricing`, `GetAvailability`) remains from the original implementation and is deprecated for SaaS mode.
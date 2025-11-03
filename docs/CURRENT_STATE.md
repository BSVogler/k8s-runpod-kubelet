# Current State: Conduit Kubelet Implementation

**Document Version:** 1.0
**Last Updated:** 2025-11-02
**Status:** Pre-production / Documentation Phase

## Overview

This document describes the current implementation state of conduit-kubelet, a virtual Kubernetes kubelet that acts as a proxy between Kubernetes clusters and GPU cloud providers. This is a **command-and-control architecture** where the kubelet executes commands sent by a central platform service.

---

## What Currently Exists

### 1. WebSocket Communication Layer

**Location:** `pkg/websocket/`

**Implemented Features:**
- Auto-reconnecting WebSocket client with exponential backoff
- Ping/pong heartbeat mechanism
- Buffered channels for commands, responses, and events
- Thread-safe connection management
- Configurable timeouts (reconnect, ping, read, write)

**Message Types:**

**Commands (Platform → Kubelet):**
- `deploy` - Deploy a pod to a specific provider
- `terminate` - Terminate a running pod
- `status` - Query pod status from provider
- `ping` - Health check

**Events (Kubelet → Platform):**
- `pod_created` - K8s scheduled a pod (includes full pod spec + annotations)
- `pod_deleted` - K8s deleted a pod
- `kubelet_registration` - Kubelet startup registration with capabilities

**Responses (Kubelet → Platform):**
- `result` - Successful command execution with data
- `error` - Command execution failed

**Data Structures:**
- `DeployParams` - Comprehensive deployment parameters (image, GPU config, env vars, ports, disk, volumes)
- `DeployResult` - Provider pod ID, cost/hour, status, location
- `StatusResult` - Detailed status with exit codes, timestamps, running/terminated/successful flags
- `PodCreatedEvent` - Full K8s pod specification + annotations
- `PodStatusChangeEvent` - Status transitions (currently not actively triggered)
- `PodDeletedEvent` - Provider pod ID and deletion reason

**Files:**
- `pkg/websocket/protocol.go` - Protocol definitions
- `pkg/websocket/client.go` - WebSocket client implementation

---

### 2. Provider System

**Location:** `pkg/providers/`

**Provider Interface:**
```go
type Provider interface {
    GetName() string
    Deploy(ctx context.Context, params *DeployParams) (*DeployResult, error)
    GetStatus(ctx context.Context, providerPodID string) (*StatusResult, error)
    Terminate(ctx context.Context, providerPodID string) error
    GetPricing(ctx context.Context) (*PricingResult, error)         // ⚠️ Should move to platform
    GetAvailability(ctx context.Context, query *AvailabilityQuery) (*AvailabilityResult, error)  // ⚠️ Should move to platform
    Ping(ctx context.Context) error
}
```

**Provider Manager** (`pkg/providers/manager.go`):
- Centralized registry for all providers
- Routes commands to provider by name
- **⚠️ Parallel health checks across all providers** (routing logic - should be platform's job)
- **⚠️ Concurrent pricing/availability queries** (routing logic - should be platform's job)
- Thread-safe provider registration

**Implemented Providers:**

**RunPod** (`pkg/providers/runpod/client.go`):
- ✅ Full GraphQL + REST API integration
- ✅ GPU type filtering and pricing queries
- ✅ Secure vs Community cloud support
- ✅ Status mapping (RUNNING, STARTING, TERMINATED, EXITED)
- ✅ Detailed pod status with runtime info, exit codes
- ✅ Uses `RUNPOD_API_KEY` from environment

**Stub Providers** (interface defined, not implemented):
- VastAI
- Salad
- AWS
- GCP

**API Key Management (Current):**
- All keys stored in kubelet environment variables
- `RUNPOD_API_KEY`
- `VASTAI_API_KEY` (planned)
- `SALAD_API_KEY` (planned)
- `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY`
- `GOOGLE_APPLICATION_CREDENTIALS`
- ❌ No support for platform-managed keys yet

**Files:**
- `pkg/providers/interface.go` - Provider interface definition
- `pkg/providers/manager.go` - Multi-provider manager
- `pkg/providers/runpod/client.go` - RunPod implementation
- `pkg/providers/runpod/graphql.go` - GraphQL queries
- `pkg/providers/runpod/types.go` - RunPod data structures

---

### 3. Command Processing

**Location:** `pkg/command/handler.go`

**Current Flow:**
1. WebSocket client receives command message
2. Message routed to command channel
3. `CommandHandler.ExecuteCommand()` processes:
   - Parses command type and parameters
   - Routes to appropriate provider via `ProviderManager`
   - Executes provider API call
   - Returns response with result/error
4. Response sent back to platform via WebSocket

**Implemented Command Handlers:**
- `handleDeploy()` - Converts params, calls `provider.Deploy()`
- `handleTerminate()` - Calls `provider.Terminate()`
- `handleStatus()` - Calls `provider.GetStatus()`
- `handlePing()` - Tests provider connectivity

**On Successful Deployment:**
- Updates local pod tracking with provider pod ID
- Stores cost per hour, machine ID, location
- Triggers K8s pod status update to "Running" (or appropriate state)

**On Command Failure:**
- Logs error
- Sends error response to platform
- ❌ Does NOT handle platform rejections differently from provider failures

---

### 4. Virtual Kubelet Integration

**Location:** `pkg/virtual_kubelet/kubelet.go`

**Kubernetes Event Handling:**

**Pod Creation** (Line 157-171):
- Triggered when K8s schedules pod to this kubelet
- Pod stored in local cache with PENDING status
- Sends `EventPodCreated` to platform with full pod spec
- **Non-blocking** - failure to send event does not fail pod creation
- Platform receives pod requirements and makes routing decision

**Pod Deletion** (Line 205-217):
- Triggered when K8s deletes pod
- Sends `EventPodDeleted` to platform
- Platform should issue terminate command
- Pod removed from local cache

**Kubelet Registration** (Line 517-533):
- Sent on startup
- Includes node name, cluster name, available providers, version, IP, port
- Platform learns kubelet capabilities

**Status Synchronization:**
- ❌ No periodic status polling currently implemented
- ❌ `EventPodStatusChange` defined but not actively triggered
- Status only updated when platform explicitly queries via `status` command

**Files:**
- `pkg/virtual_kubelet/kubelet.go` - Main kubelet provider implementation
- `pkg/virtual_kubelet/health.go` - Health check endpoints

---

### 5. Configuration System

**Location:** `pkg/config/config.go`

**Current Configuration:**
```go
type Config struct {
    BackendURL       string  // WebSocket URL for platform
    BackendAPIKey    string  // Authentication token
    NodeName         string  // K8s node name
    Namespace        string  // K8s namespace
    LogLevel         string  // Logging verbosity
    Kubeconfig       string  // Path to kubeconfig

    // Provider API keys (environment-based)
    RunPodAPIKey     string
    VastAIAPIKey     string
    SaladAPIKey      string
}
```

**Configuration Sources:**
- Command-line flags
- Environment variables
- Kubeconfig file

**Missing Configuration:**
- ❌ Key management mode (local vs platform-managed)
- ❌ Rejection handling behavior
- ❌ Status polling interval
- ❌ Provider selection mode (disabled for SaaS)

---

### 6. Error Handling

**Current Error Handling:**

**Provider Errors:**
```go
type ProviderError struct {
    Provider string
    Code     string
    Message  string
    Retry    bool    // Whether operation should be retried
}
```

**WebSocket Errors:**
- Connection failures → automatic reconnection with exponential backoff
- Message send failures → logged but may not be retried
- Command failures → error response sent to platform

**Command Execution Errors:**
- Parse errors → return error response
- Provider API failures → wrapped in ProviderError, returned to platform
- Deployment failures → platform receives error in response

**Missing Error Handling:**
- ❌ No distinction between provider failures and platform rejections
- ❌ No handling for plan limit exceeded scenarios
- ❌ No K8s event creation for rejection scenarios
- ❌ No error codes for rejection types
- ❌ No pod status updates for backend rejections

---

## What's Currently Working

### End-to-End Flow (Happy Path)

1. ✅ K8s schedules pod to kubelet
2. ✅ Kubelet stores pod locally (PENDING status)
3. ✅ Kubelet sends `EventPodCreated` to platform
4. ✅ Platform receives pod spec and analyzes requirements
5. ✅ Platform sends `CommandDeploy` with provider name + params
6. ✅ Kubelet routes to specified provider
7. ✅ Provider API call executes (e.g., RunPod)
8. ✅ Kubelet updates local pod tracking with provider pod ID
9. ✅ Kubelet sends `ResponseResult` to platform
10. ✅ Kubelet updates K8s pod status

### Provider Integration

- ✅ RunPod fully integrated and tested
- ✅ Deploy, status, terminate operations working
- ✅ GPU filtering and instance selection
- ✅ Container image pulling and execution
- ✅ Environment variables and port mappings
- ✅ Volume mounts and disk configuration

### WebSocket Reliability

- ✅ Auto-reconnection on connection loss
- ✅ Heartbeat mechanism
- ✅ Message queueing during disconnection
- ✅ TLS/WSS support

---

## Known Issues & Limitations

### Architecture Issues

**⚠️ Routing Logic in Kubelet (Should Be Platform):**
- `ProviderManager.GetAllPricing()` - Queries all providers for pricing
- `ProviderManager.GetAvailability()` - Checks availability across providers
- Provider health checks - Concurrent health checks across all providers
- These capabilities should be **removed** or **deprecated** for SaaS mode

**❌ No Platform Rejection Handling:**
- Plan limits exceeded
- Quota exceeded
- Insufficient credits
- Region restrictions
- Cost limits

**❌ No Platform-Managed API Keys:**
- All keys must be in kubelet environment
- No support for keys sent via commands
- No per-user key isolation

**❌ No Status Polling:**
- Platform must explicitly query status
- No proactive status updates to K8s
- No automatic pod status synchronization

### Operational Issues

**❌ No Multi-Tenancy Support:**
- No user/tenant isolation
- No per-user resource tracking
- No cost attribution by user

**❌ Limited Error Recovery:**
- WebSocket reconnection works ✅
- No command retry logic on temporary failures
- No failed deployment cleanup
- No orphaned pod detection

**❌ No Observability:**
- Basic logging only
- No metrics/monitoring integration
- No distributed tracing
- No audit logs

---

## Routing Logic Analysis

### What Routing/Intelligence Currently Exists in Kubelet

**Provider Selection:**
- ❌ Kubelet does NOT select providers
- ✅ Platform sends provider name in deploy command
- ✅ Kubelet routes to specified provider only

**Pricing/Availability Queries:**
- ⚠️ Kubelet CAN query all providers for pricing (should be removed)
- ⚠️ Kubelet CAN check availability across providers (should be removed)
- These are leftover from original kubelet design

**Cost Optimization:**
- ❌ Kubelet does NOT do cost optimization
- ❌ Kubelet does NOT make provider selection decisions
- Platform owns all optimization logic

**Health Checks:**
- ⚠️ Kubelet CAN ping all providers concurrently (should be platform's job)
- Could be useful for capability reporting to platform

### What Should Move to Platform

**All Intelligence/Decision Making:**
- Provider selection based on requirements
- Cost optimization
- Availability checking
- Pricing queries
- Plan limit enforcement
- Quota management
- Regional routing

**Kubelet Should Be Pure Proxy:**
- Execute commands only
- Report events only
- No routing decisions
- No cost analysis
- No provider selection

---

## Data Flow Summary

### Current Data Flow

```
┌─────────────┐
│ Kubernetes  │
└──────┬──────┘
       │ Schedules Pod
       ▼
┌─────────────────────────────────────┐
│ Conduit Kubelet (This Project)      │
│                                      │
│ 1. Receive pod from K8s              │
│ 2. Store in local cache (PENDING)   │
│ 3. Send EventPodCreated to platform  │
│                                      │
│ [Wait for platform decision...]     │
│                                      │
│ 4. Receive CommandDeploy             │
│ 5. Route to specified provider       │
│ 6. Execute provider API call         │
│ 7. Update local pod tracking         │
│ 8. Send ResponseResult               │
│ 9. Update K8s pod status             │
└──────┬──────────────────┬───────────┘
       │                  │
       │ WebSocket        │ Provider
       │ Events/          │ API Calls
       │ Commands         │
       ▼                  ▼
┌──────────────┐   ┌──────────────┐
│   Platform   │   │   Providers  │
│ (Proprietary)│   │ (RunPod, etc)│
│              │   │              │
│ - Receives   │   │ - Deploy GPU │
│   pod events │   │   instances  │
│ - Analyzes   │   │ - Return     │
│   requirements│   │   status     │
│ - Selects    │   │ - Terminate  │
│   provider   │   │   instances  │
│ - Sends      │   │              │
│   commands   │   │              │
└──────────────┘   └──────────────┘
```

### Key Clarifications

- **Platform** = Proprietary SaaS backend service (your business logic)
- **Providers** = GPU cloud providers (RunPod, Vast.ai, Salad, etc.)
- **Kubelet** = This open-source proxy (conduit-kubelet)

---

## API Key Management

### Current Implementation (Local Keys Only)

```go
// Provider initialization
runpodClient := runpod.NewClient(os.Getenv("RUNPOD_API_KEY"))
```

Keys stored in:
- Kubelet environment variables
- Kubernetes secrets mounted to kubelet pod
- Local `.env` file (development only)

### Target Implementation (Dual Mode)

**Mode 1: Local Keys (Self-Hosted)**
```go
// Fall back to environment if no key provided
apiKey := params.APIKey
if apiKey == "" {
    apiKey = os.Getenv("RUNPOD_API_KEY")
}
```

**Mode 2: Platform-Managed Keys (SaaS)**
```go
// Platform sends key with command
{
    "type": "deploy",
    "provider": "runpod",
    "params": {
        "api_key": "encrypted_or_hashed_key",  // ← Platform manages
        "image": "...",
        "gpu_type": "..."
    }
}
```

Platform responsibilities:
- Store user API keys securely
- Encrypt keys in transit
- Include key in deploy/status/terminate commands
- Never expose keys to kubelet logs

---

## Files & Structure

```
conduit-kubelet/
├── cmd/
│   └── virtual_kubelet/
│       └── main.go                    # Application entry point
├── pkg/
│   ├── command/
│   │   └── handler.go                 # Command processing
│   ├── config/
│   │   └── config.go                  # Configuration structs
│   ├── providers/
│   │   ├── interface.go               # Provider interface
│   │   ├── manager.go                 # Multi-provider manager ⚠️ Has routing logic
│   │   └── runpod/
│   │       ├── client.go              # RunPod implementation
│   │       ├── graphql.go             # GraphQL queries
│   │       └── types.go               # Data structures
│   ├── virtual_kubelet/
│   │   ├── kubelet.go                 # K8s integration
│   │   └── health.go                  # Health endpoints
│   └── websocket/
│       ├── client.go                  # WebSocket client
│       └── protocol.go                # Message definitions
├── CLAUDE.md                          # Claude Code instructions (needs update)
└── README.md                          # Project overview (needs update)
```

---

## Testing Status

**Unit Tests:**
- ❌ Minimal test coverage
- ❌ No WebSocket protocol tests
- ❌ No provider manager tests
- ❌ No command handler tests

**Integration Tests:**
- ✅ RunPod provider manually tested
- ❌ No automated integration tests
- ❌ No platform integration tests
- ❌ No end-to-end tests

**Production Readiness:**
- ❌ No load testing
- ❌ No failure scenario testing
- ❌ No security audit
- ❌ No performance benchmarks

---

## Next Steps

See:
- `docs/ARCHITECTURE.md` - Target SaaS architecture
- `docs/PRODUCTION_GAPS.md` - Missing features for production
- `docs/PRODUCTION_CHECKLIST.md` - Go-live checklist

---

## Historical Context

This project originated from a direct virtual kubelet (`k8s-runpod-kubelet`) that made provider API calls automatically. The architecture was refactored to a command-and-control model to support:
- Centralized routing decisions
- Multi-provider optimization
- Cost tracking across clusters
- Secure API key management (platform-side)
- Open-source kubelet + proprietary platform business model

Some routing logic remains from the original implementation and should be removed or deprecated for the SaaS model.

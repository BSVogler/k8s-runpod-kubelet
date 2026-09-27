# Conduit Kubelet: Target SaaS Architecture

**Document Version:** 1.0
**Last Updated:** 2025-11-02
**Architecture Type:** Command-and-Control Proxy

## Executive Summary

Conduit Kubelet is an open-source virtual Kubernetes kubelet that acts as a **pure proxy** between Kubernetes clusters and GPU cloud providers. All routing intelligence, cost optimization, and provider selection logic resides in a proprietary SaaS platform. The kubelet's sole responsibilities are:

1. **Detect** Kubernetes pod lifecycle events
2. **Report** events to the platform
3. **Execute** commands from the platform
4. **Update** Kubernetes pod status based on results

This architecture enables:
- Centralized multi-cluster routing optimization
- Secure API key management (platform-side)
- Open-source kubelet + proprietary platform business model
- Easy provider addition without kubelet changes

---

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                      Kubernetes Cluster                         │
│                                                                 │
│  ┌──────────┐   schedules pod    ┌────────────────────────┐   │
│  │  kube-   │ ───────────────────▶│ Conduit Kubelet       │   │
│  │scheduler │                     │ (Open Source Proxy)   │   │
│  └──────────┘                     │                        │   │
│                                   │ - Event Detection      │   │
│                  updates status   │ - Command Execution    │   │
│  ┌──────────┐◀───────────────────│ - Status Updates       │   │
│  │ K8s API  │                     │ - No Routing Logic     │   │
│  │  Server  │                     └────────┬───────────────┘   │
│  └──────────┘                              │                    │
└────────────────────────────────────────────┼────────────────────┘
                                             │
                                             │ WebSocket
                                             │ (WSS://)
                                             │
                                   ┌─────────▼──────────┐
                                   │   Events (Up)      │
                                   │ - pod_created      │
                                   │ - pod_deleted      │
                                   │ - kubelet_register │
                                   └─────────┬──────────┘
                                             │
                     ┌───────────────────────▼───────────────────────┐
                     │         Platform (Proprietary SaaS)           │
                     │                                               │
                     │  ┌─────────────────────────────────────┐     │
                     │  │    Routing Intelligence Engine      │     │
                     │  │                                     │     │
                     │  │ • Pod requirement analysis          │     │
                     │  │ • Provider selection                │     │
                     │  │ • Cost optimization                 │     │
                     │  │ • Availability checking             │     │
                     │  │ • Plan limit enforcement            │     │
                     │  │ • Quota management                  │     │
                     │  │ • Regional routing                  │     │
                     │  └─────────────────────────────────────┘     │
                     │                                               │
                     │  ┌─────────────────────────────────────┐     │
                     │  │    API Key Management               │     │
                     │  │                                     │     │
                     │  │ • User key storage (encrypted)      │     │
                     │  │ • Key rotation                      │     │
                     │  │ • Per-user isolation                │     │
                     │  │ • Secure key transmission           │     │
                     │  └─────────────────────────────────────┘     │
                     │                                               │
                     │  ┌─────────────────────────────────────┐     │
                     │  │    Business Logic                   │     │
                     │  │                                     │     │
                     │  │ • Plan limits (free/pro/enterprise) │     │
                     │  │ • Usage tracking & billing          │     │
                     │  │ • Cost attribution                  │     │
                     │  │ • User quotas                       │     │
                     │  └─────────────────────────────────────┘     │
                     └───────────────────────┬───────────────────────┘
                                             │
                                   ┌─────────▼──────────┐
                                   │  Commands (Down)   │
                                   │ - deploy           │
                                   │ - terminate        │
                                   │ - status           │
                                   │ + API keys         │
                                   └─────────┬──────────┘
                                             │
                                             │ WebSocket
                                             │
┌────────────────────────────────────────────┼────────────────────┐
│                      Conduit Kubelet       │                    │
│                                            ▼                    │
│                         ┌──────────────────────────┐           │
│                         │  Command Handler         │           │
│                         │                          │           │
│                         │ 1. Receive command       │           │
│                         │ 2. Extract provider name │           │
│                         │ 3. Extract API key       │           │
│                         │ 4. Route to provider     │           │
│                         │ 5. Execute API call      │           │
│                         │ 6. Return result         │           │
│                         └──────────┬───────────────┘           │
│                                    │                            │
│                         ┌──────────▼───────────┐               │
│                         │  Provider Manager    │               │
│                         │                      │               │
│                         │ • Route by name      │               │
│                         │ • No selection logic │               │
│                         │ • No pricing queries │               │
│                         └──────────┬───────────┘               │
└────────────────────────────────────┼────────────────────────────┘
                                     │
                     ┌───────────────┼───────────────┐
                     │               │               │
                     ▼               ▼               ▼
            ┌────────────┐  ┌────────────┐  ┌────────────┐
            │  RunPod    │  │  Vast.ai   │  │   Salad    │
            │    API     │  │    API     │  │    API     │
            └────────────┘  └────────────┘  └────────────┘
```

---

## Component Responsibilities

### Conduit Kubelet (Open Source - This Project)

**Role:** Pure proxy/executor with no business logic

**Responsibilities:**

1. **Event Detection & Reporting:**
   - Monitor Kubernetes pod lifecycle events
   - Send `pod_created` event when K8s schedules pod
   - Send `pod_deleted` event when K8s deletes pod
   - Send `kubelet_registration` on startup
   - Include full pod spec, annotations, and metadata

2. **Command Execution:**
   - Receive `deploy`, `terminate`, `status` commands from platform
   - Route to specified provider (by name)
   - Execute provider API calls
   - Return results to platform
   - No routing decisions - platform specifies provider

3. **API Key Handling:**
   - Accept API keys from two sources:
     - **Platform-provided:** Keys sent with commands (SaaS mode)
     - **Local environment:** Fall back to env vars (self-hosted mode)
   - Use provided key if present, otherwise use local key
   - Never log or expose API keys

4. **Status Synchronization:**
   - Update Kubernetes pod status based on provider responses
   - Create Kubernetes events for important state changes
   - Handle platform rejections (plan limits, quotas)
   - Keep pod in Pending state on rejection

5. **Health & Monitoring:**
   - Provide health check endpoints
   - Maintain WebSocket connection with auto-reconnection
   - Report kubelet health to platform

**What Kubelet Does NOT Do:**
- ❌ Provider selection or routing decisions
- ❌ Cost optimization or pricing queries
- ❌ Availability checking across providers
- ❌ Plan limit enforcement (platform's job)
- ❌ User quota management
- ❌ Multi-cluster routing decisions

---

### Platform (Proprietary SaaS)

**Role:** All intelligence, business logic, and decision-making

**Responsibilities:**

1. **Pod Requirement Analysis:**
   - Receive `pod_created` events from kubelets
   - Parse pod spec for GPU requirements, memory, disk, etc.
   - Extract user preferences from annotations
   - Analyze container image and dependencies

2. **Provider Selection & Routing:**
   - Query providers for pricing and availability (platform maintains provider clients)
   - Select optimal provider based on:
     - Cost optimization
     - GPU availability
     - Regional preferences
     - User plan features
     - Provider reliability/SLAs
   - Support multi-provider failover

3. **Business Logic Enforcement:**
   - **Plan Limits:**
     - Free plan: X concurrent pods
     - Pro plan: Y concurrent pods
     - Enterprise: Custom limits
   - **Quota Management:**
     - Per-user resource quotas
     - Team/organization limits
   - **Credit System:**
     - Check user balance before deployment
     - Reserve credits on deploy
     - Charge on successful deployment
   - **Cost Controls:**
     - Maximum cost per pod limits
     - Budget alerts
     - Auto-termination on overspend

4. **API Key Management:**
   - Store user-provided provider API keys (encrypted)
   - Validate keys before use
   - Rotate keys on schedule or on demand
   - Include appropriate key with each command
   - Never expose keys in logs or UI
   - Support per-user, per-team, or platform-managed keys

5. **Command Issuance:**
   - Send `deploy` command to kubelet with:
     - Selected provider name
     - Deployment parameters
     - Appropriate API key
   - Send `terminate` on pod deletion or quota enforcement
   - Poll status periodically or on-demand
   - Handle command failures and retries

6. **Rejection Handling:**
   - Reject deployments when:
     - Plan limits exceeded
     - Quota exceeded
     - Insufficient credits
     - Provider unavailable
     - Cost exceeds limits
   - Send a `reject` command:
     ```json
     {
       "type": "reject",
       "data": {
         "pod_name": "gpu-job-1", "namespace": "default",
         "code": "quota_exceeded",
         "message": "GPU hours quota exceeded. Upgrade at https://gpuconduit.io/dashboard/billing/",
         "upgrade_url": "https://gpuconduit.io/dashboard/billing/"
       }
     }
     ```

7. **Usage Tracking & Billing:**
   - Track pod runtime per user
   - Calculate costs based on provider pricing
   - Generate invoices
   - Usage analytics and reporting
   - Cost attribution by project/team

8. **Multi-Cluster Management:**
   - Manage connections to multiple kubelets
   - Route pods across clusters
   - Load balancing across regions
   - Global optimization

---

### GPU Providers (RunPod, Vast.ai, Salad, etc.)

**Role:** GPU infrastructure providers

**API Interactions:**
- Deploy GPU instances
- Terminate instances
- Query status
- (Platform queries pricing/availability, not kubelet)

---

## Data Flows

### 1. Pod Deployment Flow (Happy Path)

```
┌──────────┐
│   User   │
└────┬─────┘
     │
     │ kubectl apply -f pod.yaml
     │
     ▼
┌────────────────────────┐
│  Kubernetes Scheduler  │
└────────┬───────────────┘
         │
         │ Schedule to conduit-kubelet node
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Conduit Kubelet                                              │
│                                                               │
│  1. CreatePod() called by K8s                                 │
│  2. Store pod in local cache (status: PENDING)                │
│  3. Send EventPodCreated via WebSocket                        │
│     {                                                         │
│       "type": "pod_created",                                  │
│       "pod_spec": { ... },    // Full K8s pod spec            │
│       "annotations": { ... }  // User preferences             │
│     }                                                         │
│                                                               │
│  [Wait for platform decision...]                             │
└───────────────────────────────────────────────────────────────┘
         │
         │ EventPodCreated
         │ (WebSocket WSS://)
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Platform                                                     │
│                                                               │
│  4. Receive pod_created event                                 │
│  5. Parse pod requirements:                                   │
│     - GPU type: nvidia.com/gpu.product = "RTX-4090"           │
│     - GPU count: 2                                            │
│     - Memory: 32Gi                                            │
│     - Disk: 100Gi                                             │
│  6. Check user plan & quotas:                                 │
│     ✓ Pro plan (10 concurrent pods allowed)                   │
│     ✓ Currently 3 pods running                                │
│     ✓ Credits available: $500                                 │
│  7. Query providers for availability & pricing:               │
│     - RunPod: RTX 4090 available, $1.20/hr                    │
│     - Vast.ai: RTX 4090 available, $0.95/hr (SELECTED)        │
│     - Salad: Not available                                    │
│  8. Select provider: Vast.ai (best price)                     │
│  9. Retrieve user's Vast.ai API key from vault                │
│  10. Send deploy command                                      │
└───────────────────────────────────────────────────────────────┘
         │
         │ CommandDeploy
         │ (WebSocket)
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Conduit Kubelet                                              │
│                                                               │
│  11. Receive deploy command:                                  │
│      {                                                        │
│        "type": "deploy",                                      │
│        "data": {                                              │
│          "pod_name": "gpu-job-1", "namespace": "default",     │
│          "provider": "vastai",                                │
│          "api_key": "key_from_platform",   // platform mode   │
│          "image": "pytorch/pytorch:latest",                   │
│          "gpu_type_ids": ["RTX 4090"],                        │
│          "gpu_count": 2,                                      │
│          "command": ["python"], "args": ["train.py"],         │
│          ...                                                  │
│        }                                                      │
│      }                                                        │
│  12. Route to VastAI provider                                 │
│  13. Execute provider.Deploy() with provided key              │
└───────────────────────────────────────────────────────────────┘
         │
         │ Vast.ai API call
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Vast.ai API                                                  │
│                                                               │
│  14. Create GPU instance                                      │
│  15. Pull container image                                     │
│  16. Start container                                          │
│  17. Return instance ID + status                              │
└───────────────────────────────────────────────────────────────┘
         │
         │ DeployResult
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Conduit Kubelet                                              │
│                                                               │
│  18. Receive DeployResult:                                    │
│      {                                                        │
│        "provider_pod_id": "12345",                            │
│        "cost_per_hour": 0.95,                                 │
│        "status": "STARTING",                                  │
│        "machine_id": "host_789",                              │
│        "location": "us-east-1"                                │
│      }                                                        │
│  19. Update local pod tracking with provider pod ID           │
│  20. Update K8s pod status to "Running"                       │
│  21. Send ResponseResult to platform                          │
└───────────────────────────────────────────────────────────────┘
         │
         │ ResponseResult
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Platform                                                     │
│                                                               │
│  22. Receive deployment success                               │
│  23. Start usage tracking                                     │
│  24. Reserve credits                                          │
│  25. Update user dashboard                                    │
└───────────────────────────────────────────────────────────────┘
```

---

### 2. Plan Limit Exceeded Flow (Rejection)

```
┌──────────┐
│   User   │
└────┬─────┘
     │
     │ kubectl apply -f pod.yaml (4th pod on Free plan)
     │
     ▼
┌───────────────────────────────────────────────────────────────┐
│  Conduit Kubelet                                              │
│                                                               │
│  1. CreatePod() called                                        │
│  2. Store pod (status: PENDING)                               │
│  3. Send EventPodCreated                                      │
└───────────────────────────────────────────────────────────────┘
         │
         │ EventPodCreated
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Platform                                                     │
│                                                               │
│  4. Receive pod_created event                                 │
│  5. Parse pod requirements                                    │
│  6. Check user plan:                                          │
│     ✗ Free plan (2 concurrent pods allowed)                   │
│     ✗ Currently 2 pods running                                │
│     ✗ LIMIT EXCEEDED                                          │
│  7. Reject deployment - NO DEPLOY COMMAND SENT                │
│  8. Send reject command:                                      │
│     {                                                         │
│       "type": "reject",                                       │
│       "data": {                                               │
│         "pod_name": "gpu-training-job-4",                     │
│         "namespace": "default",                               │
│         "code": "quota_exceeded",                             │
│         "message": "Free plan allows 2 concurrent pods...",   │
│         "upgrade_url": "https://..."                          │
│       }                                                       │
│     }                                                         │
└───────────────────────────────────────────────────────────────┘
         │
         │ Rejection message
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  Conduit Kubelet                                              │
│                                                               │
│  9. Receive reject command                                    │
│  10. Set pod status:                                          │
│      Phase:   Failed                                          │
│      Reason:  quota_exceeded                                  │
│      Message: "Free plan allows 2 concurrent pods.            │
│               Upgrade to deploy more. https://..."            │
│  11. Respond {"status": "success", "data": {}}                │
│  12. User sees reason/message in kubectl describe pod         │
└───────────────────────────────────────────────────────────────┘
         │
         │
         ▼
┌───────────────────────────────────────────────────────────────┐
│  User Experience                                              │
│                                                               │
│  $ kubectl get pods                                           │
│  NAME                  STATUS    AGE                          │
│  gpu-training-job-1    Running   10m                          │
│  gpu-training-job-2    Running   5m                           │
│  gpu-training-job-3    Failed    30s  ← Rejected             │
│                                                               │
│  $ kubectl describe pod gpu-training-job-3                    │
│  ...                                                          │
│  Events:                                                      │
│    Warning  PlanLimitExceeded  30s                            │
│      Free plan allows 2 concurrent pods. Upgrade to Pro       │
│      for 10 concurrent pods: https://platform.com/upgrade     │
│                                                               │
│  [User upgrades plan or terminates other pod]                │
└───────────────────────────────────────────────────────────────┘
```

---

### 3. API Key Modes

#### Mode A: Platform-Managed Keys (SaaS)

**User Experience:**
1. User adds Vast.ai API key to platform web UI
2. Platform stores key encrypted in vault
3. User deploys pod via kubectl
4. Platform includes key in deploy command
5. Kubelet never stores key locally

**Benefits:**
- Secure key management
- Per-user isolation
- Easy key rotation
- No kubelet configuration needed
- Audit trail

#### Mode B: Local Keys (Self-Hosted)

**User Experience:**
1. User configures kubelet with provider keys
2. Keys stored as Kubernetes secrets
3. Mounted to kubelet pod as env vars
4. User deploys pod via kubectl
5. Platform sends deploy command WITHOUT key
6. Kubelet uses local key from environment

**Benefits:**
- No trust required in platform
- Keys never leave cluster
- Works offline (platform just for routing)
- Simple setup for single-provider scenarios

**Code Logic:**
```go
func (p *RunPodProvider) Deploy(ctx context.Context, params *DeployParams) (*DeployResult, error) {
    // Use key from command params if provided (SaaS mode)
    apiKey := params.APIKey

    // Fall back to local environment if not provided (self-hosted mode)
    if apiKey == "" {
        apiKey = os.Getenv("RUNPOD_API_KEY")
    }

    if apiKey == "" {
        return nil, fmt.Errorf("no API key provided and RUNPOD_API_KEY not set")
    }

    // Execute provider API call with key
    client := runpod.NewClient(apiKey)
    return client.CreatePod(params)
}
```

---

## WebSocket Protocol (v2)

The kubelet and the platform exchange JSON envelopes over a single WebSocket
(`wss://<host>/api/kubelet/ws?api_key=<token>`, bearer header also sent). The
platform identifies the kubelet by its API token; that token is the `kubelet_id`.

```json
{"id": "<uuid>", "type": "command|response|event|heartbeat|kubelet_registration",
 "payload": { ... }, "timestamp": "<RFC3339>", "kubelet_id": "<token>"}
```

| Envelope type          | Direction          | Payload |
|------------------------|--------------------|---------|
| `kubelet_registration` | kubelet → platform | `{id, type: "conduit-kubelet", cluster_name, node_name, namespace, capabilities, metadata: {version, internal_ip, key_mode}}` — sent after every (re)connect |
| `heartbeat`            | kubelet → platform | `{timestamp, kubelet_id, status: "alive"}` every 30s |
| `event`                | kubelet → platform | `{type: pod_created\|pod_deleted\|pod_status_change\|kubelet_ready\|kubelet_error, data}` |
| `command`              | platform → kubelet | `{type: deploy\|terminate\|status\|ping\|reject, data}`; the envelope `id` is the command id |
| `response`             | kubelet → platform | `{command_id, status: "success"\|"error", data \| null, error: {code, message, provider_error?} \| null}` |

`deploy` data is flat and provider specific (the platform already converted the
pod spec): `pod_name, namespace, provider, api_key?, image, name, env, ports,
container_disk_in_gb, volume_in_gb, gpu_type_ids, gpu_count, min_ram_per_gpu,
cloud_type, datacenter_ids, template_id, container_auth_id, max_price, command, args`.
For RunPod, `command` → `dockerEntrypoint`, `args` → `dockerStartCmd`,
`gpu_count` → `gpuCount` (omitted when 0), and terminate is `DELETE pods/{id}`
(404/204 count as success).

`metadata.key_mode` is `local` when a provider key is configured on the kubelet
(e.g. `RUNPOD_API_KEY`) and `platform` otherwise; in platform mode every
command carries `api_key`.

The full contract lives in `pkg/websocket/protocol.go`; the Python side mirrors
it in `conduit-service/src/conduit/websocket/protocol.py`.

---

## Error Handling

### Rejections

The platform refuses a pod with a `reject` command instead of `deploy`:

```json
{"type": "reject", "data": {
  "pod_name": "gpu-job-123", "namespace": "default",
  "code": "quota_exceeded",
  "message": "GPU hours quota exceeded (10/10 h this month). Upgrade at https://gpuconduit.io/dashboard/billing/",
  "upgrade_url": "https://gpuconduit.io/dashboard/billing/"}}
```

| Code                   | Meaning |
|------------------------|---------|
| `quota_exceeded`       | Active instance limit or monthly GPU hours exhausted |
| `no_api_key`           | Kubelet runs in platform key mode but no key is stored for the provider |
| `unsupported_provider` | `conduit.io/provider` annotation names a provider the platform cannot drive |
| `conversion_error`     | The pod spec could not be converted into provider parameters |
| `provider_error`       | The provider call failed (message carries the provider error) |

The kubelet sets the Kubernetes pod to `Failed` with `reason` = code and
`message` = message, and answers `{"status": "success", "data": {}}`. The
reason and message show up in `kubectl describe pod`.

### Command errors

Failed commands return `status: "error"` with a structured error:
`{"code": "deployment_failed", "message": "runpod error deployment_failed: ...", "provider_error": "..."}`.
For a failed `deploy`, the platform marks its record failed and follows up
with `reject provider_error`.

---

## Security Considerations

### API Key Security

**In Transit:**
- WebSocket connections use TLS (WSS://)
- Keys encrypted in command payloads
- Never logged or exposed

**At Rest:**
- Platform: Keys encrypted in vault (AES-256)
- Kubelet: Keys in environment (K8s secrets)
- No keys stored in kubelet state/cache

**Access Control:**
- Per-user key isolation
- Role-based access control
- Audit logs for key usage

### Network Security

**Kubelet:**
- Initiates outbound WebSocket (no inbound ports except health checks)
- Communicates with K8s API (cluster-internal)
- Calls provider APIs (outbound HTTPS)

**Platform:**
- WebSocket endpoint with authentication
- Rate limiting
- DDoS protection

### Multi-Tenancy

**Isolation:**
- Each user's pods isolated by K8s namespaces
- Provider pod IDs tracked per user
- Cost attribution per user
- No cross-user data leakage

---

## Scalability

### Kubelet Scalability

**Resource Usage:**
- Minimal CPU (event-driven)
- Memory scales with pod count (O(n))
- Network: Low bandwidth (events + commands only)

**Limits:**
- 1000s of pods per kubelet
- Limited by K8s node capacity
- Provider API rate limits

### Platform Scalability

**Horizontal Scaling:**
- Stateless command processing
- WebSocket connections load balanced
- Multiple platform instances

**Database:**
- User data, quotas, credits
- Usage tracking & billing
- Provider availability cache

---

## Configuration

### Kubelet Configuration

```yaml
# Environment Variables
BACKEND_URL: "wss://platform.example.com/api/kubelet/ws"
BACKEND_API_KEY: "kubelet_auth_token_xyz"
CLUSTER_NAME: "prod-eu"          # reported in the registration (flag: --cluster-name)
NODE_NAME: "virtual-gpu-node-1"
LOG_LEVEL: "info"

# Optional: Local provider keys (self-hosted mode)
RUNPOD_API_KEY: "your_runpod_key"
VASTAI_API_KEY: "your_vastai_key"

# Key mode is derived, not configured: "local" if any provider key above is
# set, otherwise "platform" (the platform must send api_key with each command).
```

### Platform Configuration

```yaml
# Kubelet management
KUBELET_AUTH_TOKENS: ["token1", "token2"]
KUBELET_TIMEOUT: "30s"
COMMAND_RETRY_ATTEMPTS: 3

# Provider settings
RUNPOD_ENABLED: true
VASTAI_ENABLED: true
SALAD_ENABLED: false

# Business logic
FREE_PLAN_POD_LIMIT: 2
PRO_PLAN_POD_LIMIT: 10
ENTERPRISE_PLAN_POD_LIMIT: -1  # unlimited

# Cost optimization
ENABLE_COST_OPTIMIZATION: true
PREFER_LOWEST_COST: true
MAX_COST_PER_POD: 5.0  # $/hr
```

---

## Deployment

### Kubelet Deployment (Kubernetes)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: conduit-kubelet
  namespace: kube-system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: kubelet
        image: conduit/kubelet:latest
        env:
        - name: BACKEND_URL
          value: "wss://platform.example.com/api/kubelet/ws"
        - name: BACKEND_API_KEY
          valueFrom:
            secretKeyRef:
              name: kubelet-auth
              key: api_key
        # Optional: Local keys for self-hosted mode
        - name: RUNPOD_API_KEY
          valueFrom:
            secretKeyRef:
              name: provider-keys
              key: runpod_key
              optional: true
```

### Platform Deployment (Example)

```
Platform Service (Load Balanced):
  - WebSocket handler
  - Command processor
  - Event consumer

Database (PostgreSQL):
  - User data
  - Plans & quotas
  - Usage tracking

Cache (Redis):
  - Provider pricing
  - Availability data
  - Session state

Worker Queue (Celery/RQ):
  - Async command processing
  - Status polling
  - Billing jobs
```

---

## Future Enhancements

### Planned Features

1. **Active Status Polling:**
   - Platform polls pod status periodically
   - Kubelet pushes status changes proactively
   - Automatic K8s status synchronization

2. **Multi-Provider Failover:**
   - Platform retries on provider failure
   - Automatic fallback to alternative providers
   - Provider health tracking

3. **Cost Optimization:**
   - Spot instance support
   - Automatic pod migration to cheaper providers
   - Budget alerts

4. **Advanced Routing:**
   - Affinity/anti-affinity rules
   - Regional preferences
   - Custom routing policies

5. **Observability:**
   - Prometheus metrics
   - Distributed tracing
   - Audit logs

---

## Comparison to Original Kubelet

| Feature | Original Kubelet | Conduit Kubelet (Target) |
|---------|------------------|--------------------------|
| Provider selection | Local (annotations) | Platform (centralized) |
| API keys | Local environment | Platform-managed OR local |
| Cost optimization | None | Platform (multi-cluster) |
| Plan limits | None | Platform enforced |
| Routing logic | Kubelet | Platform |
| Pricing queries | Kubelet | Platform |
| Multi-tenancy | None | Platform managed |
| Business model | Self-hosted only | SaaS + self-hosted |

---

## Summary

**Conduit Kubelet = Pure Proxy**
- No routing decisions
- No cost optimization
- No provider selection
- Just execute commands from platform

**Platform = All Intelligence**
- Provider selection
- Cost optimization
- Plan enforcement
- Quota management
- API key management

**Result:**
- Open-source kubelet (this project)
- Proprietary platform (your business)
- Secure, scalable, multi-tenant SaaS

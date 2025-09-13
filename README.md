# K8s Proxy Kubelet

A command-and-control virtual kubelet that acts as a proxy between Kubernetes and multiple GPU cloud providers. The kubelet executes provider API calls only when instructed by a backend service via WebSocket commands, enabling centralized routing decisions while keeping API keys secure in-cluster.

## Architecture

### Command-and-Control Flow
1. K8s schedules pod → Proxy Kubelet
2. Kubelet reports pod event → Backend (via WebSocket)
3. Backend makes provider selection decision
4. Backend sends deployment command → Kubelet (via WebSocket)
5. Kubelet executes RunPod/Vast.ai/etc API call
6. Kubelet reports result → Backend
7. Kubelet updates K8s pod status

### Key Benefits
- **Centralized Decision Making**: Backend orchestrates all provider routing decisions
- **Secure Key Management**: Provider API keys remain in-cluster, not in backend
- **Multi-Provider Support**: Single kubelet supports RunPod, Vast.ai, Salad, AWS, GCP
- **Real-Time Commands**: WebSocket communication for instant command delivery
- **Cost Optimization**: Backend can optimize costs across providers globally

## Quick Start

### Prerequisites
- Kubernetes cluster with RBAC permissions
- Backend service with WebSocket API (see [Backend Requirements](#backend-requirements))
- Provider API keys (RunPod, Vast.ai, etc.)

### Installation

#### 1. Using Helm (Recommended)
```bash
# Add Helm repository (when published)
helm repo add gpu-proxy https://charts.gpuconduit.io
helm repo update

# Install with your backend configuration
helm install gpu-proxy gpu-proxy/proxy-kubelet \
  --set config.backendURL="wss://your-backend.com/api/kubelet/ws" \
  --set config.backendAPIKey="your-api-key" \
  --set providers.runpod.apiKey="your-runpod-key"
```

#### 2. Manual Deployment
```bash
# Clone repository
git clone https://github.com/bsvogler/k8s-proxy-kubelet
cd k8s-proxy-kubelet

# Build container image
docker build -t k8s-proxy-kubelet:latest .

# Apply Kubernetes manifests
kubectl apply -f deploy/
```

#### 3. Local Development
```bash
# Build binary
go build -o proxy-kubelet ./cmd/virtual_kubelet

# Run locally (requires kubeconfig)
./proxy-kubelet \
  --kubeconfig=$HOME/.kube/config \
  --backend-url="wss://your-backend.com/api/kubelet/ws" \
  --backend-api-key="your-api-key" \
  --log-level=debug
```

### Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `BACKEND_URL` | WebSocket URL for backend communication | Yes |
| `BACKEND_API_KEY` | Authentication token for backend | Yes |
| `RUNPOD_API_KEY` | RunPod API key | If RunPod enabled |
| `VASTAI_API_KEY` | Vast.ai API key | If Vast.ai enabled |
| `SALAD_API_KEY` | Salad API key | If Salad enabled |
| `AWS_ACCESS_KEY_ID` | AWS access key | If AWS enabled |
| `AWS_SECRET_ACCESS_KEY` | AWS secret key | If AWS enabled |
| `GOOGLE_APPLICATION_CREDENTIALS` | GCP service account key | If GCP enabled |

## Configuration

### Command Line Flags

```bash
./proxy-kubelet [options]

Options:
  --kubeconfig string           Path to kubeconfig file
  --config string               Path to configuration file
  --nodename string             Kubernetes node name (default "virtual-proxy")
  --backend-url string          Backend WebSocket URL
  --backend-api-key string      Backend API authentication key
  --internal-ip string          Internal IP address (default "127.0.0.1")
  --listen-port int             Port to listen on (default 10250)
  --health-server-address string Health server address (default ":8080")
  --namespace string            Kubernetes namespace (default "kube-system")
  --log-level string            Log level: debug, info, warn, error (default "info")
  --enabled-providers string    Comma-separated list of providers (default "runpod")
```

### YAML Configuration (Future)

```yaml
# config.yaml
backend_url: "wss://gpuconduit.io/api/kubelet/ws"
backend_api_key: "your-api-key"

websocket:
  reconnect_interval: 5s
  ping_interval: 30s
  read_timeout: 60s

providers:
  enabled_providers: ["runpod", "vastai"]
  runpod:
    api_key: "your-runpod-key"
    default_cloud_type: "SECURE"
    max_gpu_price: 0.5
  vastai:
    api_key: "your-vastai-key"
    max_gpu_price: 0.5

node_name: "gpu-proxy-1"
namespace: "kube-system"
log_level: "info"
```

## WebSocket API Protocol

The kubelet communicates with the backend using a WebSocket-based protocol with JSON messages.

### Connection
- **URL**: `wss://backend.example.com/api/kubelet/ws`
- **Authentication**: `Authorization: Bearer <BACKEND_API_TOKEN>` header
- **Auto-reconnect**: Exponential backoff with jitter

### Message Format

All messages follow this structure:
```json
{
  "id": "unique-message-id",
  "type": "message_type",
  "timestamp": "2025-01-13T10:30:00Z",
  "data": { /* type-specific data */ }
}
```

### Commands (Backend → Kubelet)

#### Deploy Pod
```json
{
  "id": "cmd-123",
  "type": "deploy",
  "timestamp": "2025-01-13T10:30:00Z",
  "data": {
    "id": "cmd-123",
    "type": "deploy",
    "provider": "runpod",
    "pod_id": "default/test-pod",
    "params": {
      "name": "test-pod",
      "image": "nvidia/cuda:11.8-base-ubuntu22.04",
      "gpu_type_ids": ["NVIDIA_RTX_4090"],
      "env": {"MODEL_NAME": "llama-2-7b"},
      "ports": ["8080/http"],
      "container_disk_in_gb": 20,
      "cloud_type": "SECURE",
      "datacenter_ids": ["US-TX-1", "US-CA-1"],
      "template_id": "template-xyz",
      "max_price": 0.50
    }
  }
}
```

#### Terminate Pod
```json
{
  "id": "cmd-124",
  "type": "terminate",
  "timestamp": "2025-01-13T10:31:00Z",
  "data": {
    "id": "cmd-124",
    "type": "terminate",
    "provider": "runpod",
    "pod_id": "default/test-pod",
    "params": {
      "provider_pod_id": "runpod-xyz-789"
    }
  }
}
```

#### Status Check
```json
{
  "id": "cmd-125",
  "type": "status",
  "timestamp": "2025-01-13T10:32:00Z",
  "data": {
    "id": "cmd-125",
    "type": "status",
    "provider": "vastai",
    "pod_id": "default/test-pod",
    "params": {
      "provider_pod_id": "vastai-abc-123"
    }
  }
}
```

#### Ping
```json
{
  "id": "ping-001",
  "type": "ping",
  "timestamp": "2025-01-13T10:33:00Z",
  "data": {
    "id": "ping-001",
    "type": "ping",
    "provider": "" // empty = test all providers
  }
}
```

### Responses (Kubelet → Backend)

#### Successful Deploy Response
```json
{
  "id": "resp-123",
  "type": "result",
  "timestamp": "2025-01-13T10:30:15Z",
  "data": {
    "command_id": "cmd-123",
    "success": true,
    "result": {
      "provider_pod_id": "runpod-xyz-789",
      "cost_per_hour": 0.45,
      "machine_id": "gpu-machine-456",
      "status": "STARTING",
      "location": "US-TX-1",
      "datacenter_id": "texas-1"
    }
  }
}
```

#### Error Response
```json
{
  "id": "resp-124",
  "type": "result",
  "timestamp": "2025-01-13T10:31:05Z",
  "data": {
    "command_id": "cmd-124",
    "success": false,
    "error": "Pod not found on RunPod API"
  }
}
```

#### Status Response
```json
{
  "id": "resp-125",
  "type": "result",
  "timestamp": "2025-01-13T10:32:10Z",
  "data": {
    "command_id": "cmd-125",
    "success": true,
    "result": {
      "status": "RUNNING",
      "exit_code": 0,
      "message": "Container running successfully",
      "last_updated": "2025-01-13T10:32:00Z",
      "is_running": true,
      "is_terminated": false,
      "is_successful": false
    }
  }
}
```

### Events (Kubelet → Backend, Unsolicited)

#### Pod Created
```json
{
  "id": "evt-001",
  "type": "pod_created",
  "timestamp": "2025-01-13T10:29:30Z",
  "data": {
    "type": "pod_created",
    "pod_id": "default/new-pod",
    "namespace": "default",
    "data": {
      "pod_spec": { /* Full Kubernetes pod spec */ },
      "annotations": {
        "runpod.io/templateId": "template-xyz",
        "runpod.io/required-gpu-memory": "24"
      }
    }
  }
}
```

#### Pod Status Change
```json
{
  "id": "evt-002",
  "type": "pod_status_change",
  "timestamp": "2025-01-13T10:35:00Z",
  "data": {
    "type": "pod_status_change",
    "pod_id": "default/test-pod",
    "data": {
      "provider_pod_id": "runpod-xyz-789",
      "old_status": "STARTING",
      "new_status": "RUNNING",
      "message": "Container started successfully",
      "exit_code": 0
    }
  }
}
```

#### Pod Deleted
```json
{
  "id": "evt-003",
  "type": "pod_deleted",
  "timestamp": "2025-01-13T10:40:00Z",
  "data": {
    "type": "pod_deleted",
    "pod_id": "default/test-pod",
    "data": {
      "provider_pod_id": "runpod-xyz-789",
      "reason": "Pod deleted by user"
    }
  }
}
```

## Backend Requirements

The backend service must implement the following WebSocket API endpoints and behavior:

### Required Endpoints

1. **WebSocket Endpoint**: `/api/kubelet/ws`
   - Accept kubelet connections with Bearer authentication
   - Handle command routing and response processing
   - Maintain connection state and heartbeat

2. **Registration Endpoint**: `PUT /api/kubelet/register` (HTTP)
   - Register kubelet instances
   - Store cluster metadata and capabilities

3. **Health Check Endpoint**: `GET /api/health` (HTTP)
   - Backend service health status

### Backend Responsibilities

1. **Pod Scheduling Logic**
   - Receive pod creation events
   - Make provider selection decisions (cost, availability, constraints)
   - Send appropriate deployment commands

2. **Provider Management**
   - Track provider availability and pricing
   - Implement cost optimization algorithms
   - Handle provider failover scenarios

3. **Command Queuing**
   - Queue commands for delivery to kubelets
   - Handle command timeouts and retries
   - Track command execution status

4. **Monitoring & Analytics**
   - Collect deployment metrics
   - Track costs across providers
   - Monitor kubelet health and connectivity

## Supported Providers

### RunPod
- **Status**: ✅ Implemented
- **API**: GraphQL + REST
- **Features**: Secure/Community cloud, templates, datacenter selection
- **GPU Types**: RTX 4090, A100, H100, L40S, etc.

### Vast.ai
- **Status**: 🚧 In Development
- **API**: REST
- **Features**: Datacenter filtering, SSH access, custom images
- **GPU Types**: RTX 3090, RTX 4090, A100, etc.

### Salad
- **Status**: 📋 Planned
- **API**: REST
- **Features**: Consumer GPU access, low-cost inference
- **GPU Types**: RTX 3060, RTX 4060, etc.

### AWS (EKS + EC2)
- **Status**: 📋 Planned
- **API**: AWS SDK
- **Features**: Spot instances, reserved capacity, enterprise SLA
- **Instance Types**: p3, p4, g4dn, etc.

### GCP (GKE + Compute)
- **Status**: 📋 Planned
- **API**: Google Cloud SDK
- **Features**: Preemptible instances, committed use discounts
- **Instance Types**: A2, N1 with GPU, etc.

## Pod Annotation Reference

Control provider behavior using Kubernetes pod annotations:

### RunPod Annotations
```yaml
metadata:
  annotations:
    runpod.io/templateId: "template-xyz"           # Use specific template
    runpod.io/required-gpu-memory: "24"           # Min GPU RAM in GB
    runpod.io/datacenter-ids: "US-TX-1,US-CA-1"  # Preferred datacenters
    runpod.io/cloud-type: "SECURE"               # SECURE or COMMUNITY
    runpod.io/ports: "8080/http,5432/tcp"        # Manual port override
    runpod.io/container-auth-id: "registry-auth" # Registry auth ID
```

### Vast.ai Annotations
```yaml
metadata:
  annotations:
    vastai.io/gpu-memory: "16"                    # Min GPU RAM in GB
    vastai.io/max-price: "0.30"                  # Max price per hour
    vastai.io/datacenter-ids: "US,CA"            # Country codes
    vastai.io/ssh-key: "ssh-key-name"            # SSH key for access
```

### Generic Annotations
```yaml
metadata:
  annotations:
    gpu-conduit.io/provider-preference: "runpod,vastai"  # Provider priority
    gpu-conduit.io/max-cost: "1.00"                      # Max total cost/hour
    gpu-conduit.io/scaling-priority: "cost"              # cost|speed|availability
```

## Health Checks

The kubelet exposes health check endpoints:

- **Liveness**: `GET :8080/healthz` - Basic service health
- **Readiness**: `GET :8080/readyz` - Ready to accept pods
- **Health**: `GET :8080/health` - Combined health status
- **Status**: `GET :8080/status` - Detailed status with provider info

## Monitoring & Troubleshooting

### Logs
```bash
# View kubelet logs
kubectl logs deployment/proxy-kubelet -n kube-system

# Debug level logging
./proxy-kubelet --log-level=debug

# Follow WebSocket communication
kubectl logs deployment/proxy-kubelet -n kube-system | grep -i websocket
```

### Common Issues

1. **WebSocket Connection Failed**
   ```
   Error: failed to dial WebSocket: dial tcp: connect: connection refused
   ```
   - Check backend URL and network connectivity
   - Verify backend API key is correct
   - Ensure backend service is running

2. **Provider API Authentication Failed**
   ```
   Error: RunPod API error: invalid API key
   ```
   - Verify provider API keys are set correctly
   - Check key permissions and quotas

3. **Pod Stuck in Pending**
   ```
   Status: Pod creation event sent to backend
   ```
   - Backend may not be sending deploy commands
   - Check backend logs for provider selection logic
   - Verify provider availability and quotas

## Development

### Building from Source
```bash
git clone https://github.com/bsvogler/k8s-proxy-kubelet
cd k8s-proxy-kubelet

# Install dependencies
go mod download

# Run tests
go test ./...

# Build binary
go build -o proxy-kubelet ./cmd/virtual_kubelet

# Build container image
docker build -t k8s-proxy-kubelet:dev .
```

### Project Structure
```
k8s-proxy-kubelet/
├── cmd/virtual_kubelet/          # Main entry point
├── pkg/
│   ├── websocket/               # WebSocket client & protocol
│   ├── providers/               # Provider interface & implementations
│   │   ├── runpod/             # RunPod client
│   │   ├── vastai/             # Vast.ai client (planned)
│   │   └── salad/              # Salad client (planned)
│   ├── command/                # Command handler
│   ├── config/                 # Configuration management
│   └── virtual_kubelet/        # Kubelet provider implementation
├── deploy/                     # Kubernetes manifests
├── helm/                       # Helm charts
└── docs/                       # Additional documentation
```

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Related Projects

- [k8s-runpod-kubelet](https://github.com/bsvogler/k8s-runpod-kubelet) - Original direct RunPod integration
- [conduit-service](https://github.com/bsvogler/conduit-service) - Backend service for GPU orchestration
- [virtual-kubelet](https://github.com/virtual-kubelet/virtual-kubelet) - Upstream virtual kubelet framework
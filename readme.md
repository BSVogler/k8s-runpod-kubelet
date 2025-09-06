# Virtual Kubelet for RunPod

[![Go Report Card](https://goreportcard.com/badge/github.com/bsvogler/k8s-runpod-kubelet)](https://goreportcard.com/report/github.com/bsvogler/k8s-runpod-kubelet)
[![License](https://img.shields.io/github/license/bsvogler/k8s-runpod-kubelet)](LICENSE)
[![Helm Chart](https://img.shields.io/badge/helm-ghcr.io-blue)](https://github.com/users/bsvogler/packages/container/package/charts%2Frunpod-kubelet)
[![Container Image](https://img.shields.io/badge/container-ghcr.io-blue)](https://github.com/bsvogler/k8s-runpod-kubelet/pkgs/container/runpod-kubelet)
[![Release](https://img.shields.io/github/v/release/bsvogler/k8s-runpod-kubelet)](https://github.com/bsvogler/k8s-runpod-kubelet/releases)

This Virtual Kubelet implementation provides seamless integration between Kubernetes and RunPod, enabling dynamic, cloud-native GPU workload scaling across local and cloud resources - automatically extend your cluster with on-demand GPUs without managing infrastructure.

## 🚀 Commercial License & Multi-Cloud Upgrade

**Ready for production?** Get instant access with [GPU Conduit](https://gpuconduit.io) - the commercial license that unlocks:

- ✅ **Multi-Cloud Access**: RunPod, Vast.ai, Lambda Labs, and 7+ more providers in one install
- ✅ **3-5x Cost Savings**: Intelligent routing to cheapest available GPUs
- ✅ **Automatic Failover**: Never get stuck when a provider goes down
- ✅ **Production Support**: Enterprise-grade reliability and support
- ✅ **One Unified Bill**: Stop managing multiple provider accounts

**[Get started with GPU Conduit →](https://gpuconduit.io)**

*This kubelet requires a GPU Conduit license for production use. Register at gpuconduit.io to get your API token.*

## 🌟 Key Features

- **Dynamic Cloud Bursting**: Automatically extend Kubernetes GPU capabilities using RunPod
- **Virtual Node Integration**: Leverages Virtual Kubelet to create a bridge between your Kubernetes cluster and RunPod
- **On-Demand GPU Resources**: Schedule GPU workloads to RunPod directly from your Kubernetes cluster
- **Cost Optimization**: Only pay for GPU time when your workloads need it
- **Transparent Integration**: Appears as a native Kubernetes node with standard scheduling
- **Intelligent Resource Selection**: Choose GPU types based on memory requirements and price constraints
- **Automatic Lifecycle Management**: Handles pod creation, monitoring, and termination across platforms

## 🔧 How It Works

The controller implements the [Virtual Kubelet](https://virtual-kubelet.io/) provider interface to represent RunPod as a
node in your Kubernetes cluster. When pods are scheduled to this virtual node, they are automatically deployed to
RunPod's infrastructure.

The Virtual Kubelet acts as a bridge between Kubernetes and RunPod, providing a fully integrated experience:

1. **Node Representation**:
    - Creates a virtual node in the Kubernetes cluster
    - Exposes RunPod resources as schedulable compute capacity

2. **Pod Lifecycle Management**:
    - Creates pods on RunPod matching Kubernetes specifications
    - Tracks and synchronizes pod statuses
    - Handles pod termination and cleanup

3. **Resource Mapping**:
    - Translates Kubernetes pod specifications to RunPod configurations
    - Manages pod annotations and metadata

```
┌─────────────────────────┐                 ┌─────────────────────┐
│  Kubernetes Cluster     │                 │     RunPod Cloud    │
│                         │                 │                     │
│  ┌───────────────────┐  │    RunPod API   │  ┌───────────────┐  │
│  │                   │  │                 │  │               │  │
│  │  Regular Nodes    │  │                 │  │  GPU Instance │  │
│  │                   │  │                 │  │               │  │
│  └───────────────────┘  │                 │  └───────────────┘  │
│                         │                 │                     │
│  ┌───────────────────┐  │     ◄─────►     │  ┌───────────────┐  │
│  │  RunPod           │  │                 │  │               │  │
│  │  Virtual Node     │──┼─────────────────┼─►│  GPU Instance │  │
│  │                   │  │                 │  │               │  │
│  └───────────────────┘  │                 │  └───────────────┘  │
│                         │                 │                     │
└─────────────────────────┘                 └─────────────────────┘
```

## 📋 Prerequisites

- Kubernetes cluster (v1.19+)
- **GPU Conduit license** - [Register at gpuconduit.io](https://gpuconduit.io) to get your API token
- RunPod account and API key (for direct RunPod usage)
- `kubectl` configured to access your cluster
- Go 1.19+ (for building from source)

## 🚀 Installation

### Using Helm (Recommended)

```bash
# Install from GitHub Container Registry
helm install runpod-kubelet oci://ghcr.io/bsvogler/helm/runpod-kubelet \
  --namespace kube-system \
  --create-namespace \
  --set conduit.apiToken=<your-conduit-api-token> \
  --set runpod.apiKey=<your-runpod-api-key>
```

**Required:** You must obtain your Conduit API token by registering at [gpuconduit.io](https://gpuconduit.io)

#### Configure secret for API keys

```bash
# Create secret with both Conduit and RunPod API keys
kubectl create secret generic runpod-api-secret \
  --namespace kube-system \
  --from-literal=CONDUIT_API_TOKEN=<your-conduit-api-token> \
  --from-literal=RUNPOD_API_KEY=<your-runpod-api-key>

# Install with existing secret
helm install runpod-kubelet oci://ghcr.io/bsvogler/charts/runpod-kubelet \
  --namespace kube-system \
  --set runpod.existingSecret=runpod-api-secret
```

### Installation using kubectl

```bash
# Create secret with both Conduit and RunPod API keys
kubectl create secret generic runpod-kubelet-secrets \
  --namespace kube-system \
  --from-literal=CONDUIT_API_TOKEN=<your-conduit-api-token> \
  --from-literal=RUNPOD_API_KEY=<your-runpod-api-key>

# Apply controller deployment
kubectl apply -f https://raw.githubusercontent.com/bsvogler/k8s-runpod-kubelet/main/deploy/kubelet.yaml
```

### Virtual Node Configuration

#### Helm Configuration

Customize your deployment by creating a `values.yaml` file:

```yaml
kubelet:
  reconcileInterval: 30
  maxGpuPrice: 0.5
  namespace: kube-system

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

Then install with:

```bash
helm install runpod-kubelet oci://ghcr.io/bsvogler/charts/runpod-kubelet \
  --namespace kube-system \
  --values values.yaml
```

#### Command-line Configuration

When running the binary directly, configure using flags:

```bash
./k8s-runpod-kubelet \
  --nodename=virtual-runpod \
  --namespace=kube-system \
  --max-gpu-price=0.5 \
  --log-level=info \
  --reconcile-interval=30
```

Common configuration options:

- `--nodename`: Name of the virtual node (default: "virtual-runpod")
- `--namespace`: Kubernetes namespace (default: "kube-system")
- `--max-gpu-price`: Maximum price per hour for GPU instances (default: 0.5)
- `--reconcile-interval`: Frequency of status updates in seconds (default: 30)
- `--log-level`: Logging verbosity (debug, info, warn, error)
- `--datacenter-ids`: Comma-separated list of preferred datacenter IDs for pod placement

## 🔍 Usage

Once installed, the controller will register a virtual node named `virtual-runpod` in your cluster. It is tainted by default with (virtual-kubelet.io/provider=runpod:NoSchedule) to avoid costs. To allow scheduling a pod to RunPod, add the appropriate *tolerations* and if you want to force the usage of RunPod add a node selector for the virtual kubelet. Example:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-workload
spec:
  containers:
    - name: gpu-container
      image: nvidia/cuda:11.7.1-base-ubuntu22.04
      command: [ "nvidia-smi", "-L" ]
      resources:
        requests:
          nvidia.com/gpu: 1
        limits:
          nvidia.com/gpu: 1
  nodeSelector:
    type: virtual-kubelet
  tolerations:
    - key: "virtual-kubelet.io/provider"
      operator: "Equal"
      value: "runpod"
      effect: "NoSchedule"
```

### Annotations

You can customize the RunPod deployment using annotations. Annotations can be placed on either:
- **The Pod spec** directly (in `pod.metadata.annotations`)
- **The Job spec** that controls the pod (in `job.metadata.annotations`)

The kubelet will check the pod first, then fall back to the job annotations if not found on the pod.

```yaml
metadata:
  annotations:
    runpod.io/required-gpu-memory: "16" # Minimum GPU memory in GB
    runpod.io/container-registry-auth-id: "your-auth-id"  # For private registries. Works as of August 2025 and takes precedence over template authentication. Note: RunPod may return "access forbidden" errors for up to 4 minutes after setting registry auth before it becomes active.
    runpod.io/templateId: "your-template-id"  # Use a specific RunPod template including a preregistered authentication.
    runpod.io/datacenter-ids: "datacenter1,datacenter2" # Comma-separated list of allowed datacenter IDs
```

Experimental:

```yaml
    runpod.io/cloud-type: "COMMUNITY"   # SECURE or COMMUNITY. Note: In tests, only SECURE consistently works with the API, so it defaults to SECURE.
```

### Port Configuration

The kubelet automatically handles port exposure from your standard Kubernetes pod specifications.

```yaml
apiVersion: v1
kind: Pod
spec:
  containers:
  - name: app
    image: nginx:latest
    ports:
    - containerPort: 80    # Auto-detected as HTTP
    - containerPort: 5432  # Auto-detected as TCP
```

**Auto-detected HTTP ports:** 80, 443, 8080, 8000, 3000, 5000, 8888, 9000  
**Everything else:** TCP

#### Manual Override

Use the `runpod.io/ports` annotation to override auto-detection:

```yaml
metadata:
  annotations:
    runpod.io/ports: "8080/tcp,9000/http"  # Override auto-detection
```

### Datacenter Configuration

Datacenter IDs can be configured at two levels with compliance enforcement:

1. **Node-level**: Using the `--datacenter-ids` flag when starting the virtual kubelet. This sets the **allowed** datacenters for all pods scheduled to this virtual node.

2. **Pod-level**: Using the `runpod.io/datacenter-ids` annotation on individual pods. Pod-level annotations are **restricted** by the node-level configuration for compliance.

```yaml
# Pod-level datacenter configuration
metadata:
  annotations:
    runpod.io/datacenter-ids: "US-NJ-1,EU-RO-1"  # Must be subset of node's allowed datacenters
```

**Compliance Behavior:**
- If no node-level restriction is set, pods can specify any datacenters
- If node-level datacenters are configured, pods can only use those datacenters or a subset
- Pod requests for non-allowed datacenters will be filtered out with warnings
- If no valid datacenters remain after filtering, the pod deployment will fail

#### RunPod Port Behavior

RunPod handles HTTP and TCP ports differently, which affects readiness detection:

- **HTTP ports** (`/http`): Exposed through RunPod's web proxy system and do not appear in the API's `portMappings` field. The kubelet assumes these are ready when the pod is running.
- **TCP ports** (`/tcp`): Exposed with direct port mappings and appear in the API's `portMappings` field. The kubelet waits for these to be explicitly mapped before marking the pod as ready.

This means pods with HTTP ports will transition to ready state faster, while pods with TCP ports (like SSH, databases) will wait for the actual port mapping to be established.

## 🛠️ Development

### Building from source

```bash
# Clone the repository
git clone https://github.com/bsvogler/k8s-runpod-kubelet.git
cd k8s-runpod-kubelet

# Build the binary
go build -o k8s-runpod-kubelet ./cmd/virtual_kubelet

# Run locally
./k8s-runpod-kubelet --kubeconfig=$HOME/.kube/config
```

## 🧪 Testing

The project includes integration tests that deploy actual resources to RunPod. To run the integration tests:

```bash
export RUNPOD_API_KEY=<your-runpod-api-key>
export KUBECONFIG=<path-to-kubeconfig>
go test -tags=integration ./...
```

For regular unit tests that don't communicate with RunPod:

```bash
go test ./...
```

## 🔄 Architecture Details

The controller consists of several components:

- **RunPod Client**: Handles communication with the RunPod API
- **Virtual Kubelet Provider**: Implements the provider interface to integrate with Kubernetes
- **Health Server**: Provides liveness and readiness probes for the controller
- **Pod Lifecycle Management**: Handles pod creation, status updates, and termination

## ⚠️ Limitations

- Unofficial integration - awaiting potential RunPod collaboration
- No direct container exec/attach support (RunPod doesn't provide this capability)
- Container logs not directly supported by RunPod
- Job-specific functionality is limited to what RunPod API exposes

## 🤝 Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## 📄 License

This project is licensed under a Non-Commercial License - see the [LICENSE](LICENSE) file for details.

- **Permitted**: Private, personal, and non-commercial use
- **Prohibited**: Commercial use without explicit permission
- **Required**: License and copyright notice

For commercial licensing inquiries or to discuss custom development work as a freelancer, please contact me at
engineering@benediktsvogler.com.

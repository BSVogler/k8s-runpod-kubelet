# 🚀 Virtual Kubelet for RunPod

[![Go Report Card](https://goreportcard.com/badge/github.com/bsvogler/k8s-runpod-kubelet)](https://goreportcard.com/report/github.com/bsvogler/k8s-runpod-kubelet)
[![License](https://img.shields.io/github/license/bsvogler/k8s-runpod-kubelet)](LICENSE)
[![Container Registry](https://img.shields.io/badge/container-ghcr.io-blue)](https://github.com/bsvogler/k8s-runpod-kubelet/pkgs/container/k8s-runpod-kubelet)
[![Release](https://img.shields.io/github/v/release/bsvogler/k8s-runpod-kubelet)](https://github.com/bsvogler/k8s-runpod-kubelet/releases)

This Virtual Kubelet implementation provides seamless integration between Kubernetes and RunPod, enabling dynamic,
cloud-native GPU workload scaling across local and cloud resources - automatically extend your cluster with on-demand
GPUs without managing infrastructure.

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
- RunPod account and API key with appropriate permissions
- `kubectl` configured to access your cluster
- Go 1.19+ (for building from source)

## 🚀 Installation

### Using kubectl

```bash
# Create secret with RunPod API key
kubectl create secret generic runpod-secret \
  --namespace kube-system \
  --from-literal=RUNPOD_KEY=<your-runpod-api-key>

# Apply controller deployment
kubectl apply -f https://raw.githubusercontent.com/bsvogler/k8s-runpod-kubelet/main/deploy/kubelet.yaml
```

### Configuration

Configure using environment variables or command-line flags:

```bash
./k8s-runpod-kubelet \
  --nodename=virtual-runpod \
  --namespace=kube-system \
  --max-gpu-price=0.5 \
  --log-level=info \
  --reconcile-interval=30
```

Common configuration options:

- `RUNPOD_KEY`: RunPod API authentication key
- `--nodename`: Name of the virtual node (default: "virtual-runpod")
- `--namespace`: Kubernetes namespace (default: "kube-system")
- `--max-gpu-price`: Maximum price per hour for GPU instances (default: 0.5)
- `--reconcile-interval`: Frequency of status updates in seconds (default: 30)
- `--log-level`: Logging verbosity (debug, info, warn, error)
- `--datacenter-ids`: Comma-separated list of preferred datacenter IDs for pod placement

## 🔍 Usage

Once installed, the controller will register a virtual node named `virtual-runpod` in your cluster. To schedule a pod to
RunPod, add the appropriate node selector and tolerations:

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

You can customize the RunPod deployment using annotations. Add annotations to the pod or to the controlling job.

```yaml
metadata:
  annotations:
    runpod.io/required-gpu-memory: "16" # Minimum GPU memory in GB
    runpod.io/templateId: "your-template-id"  # Use a specific RunPod template to use a preregistered authentication.
    runpod.io/datacenter-ids: "datacenter1,datacenter2" # Comma-separated list of allowed datacenter IDs
```

Experimental. Implemented here but not working with the API.
``
    runpod.io/cloud-type: "COMMUNITY"   # SECURE or COMMUNITY.should in THEORY support COMMUNITY and STANDARD but in tests only STANDARD lead to results when using the API. Therefore, defaults to STANDARD.
    runpod.io/container-registry-auth-id: "your-auth-id"  # For private registries. Should be supported in theory but only working solution so far found when using template id and preregistering.
``

### Monitoring

Monitor the controller using Kubernetes tools:

```bash
# Check logs
kubectl logs -n kube-system deployment/runpod-virtual-kubelet

# Health checks
kubectl port-forward -n kube-system deployment/runpod-virtual-kubelet 8080:8080
curl http://localhost:8080/healthz  # Liveness probe
curl http://localhost:8080/readyz   # Readiness probe
```

## 🛠️ Development

### Building from source

```bash
# Clone the repository
git clone https://github.com/bsvogler/k8s-runpod-kubelet.git
cd k8s-runpod-kubelet

# Build the binary
go build -o k8s-runpod-kubelet ./cmd/main.go

# Run locally
./k8s-runpod-kubelet --kubeconfig=$HOME/.kube/config
```

## 🧪 Testing

The project includes integration tests that deploy actual resources to RunPod. To run the integration tests:

```bash
export RUNPOD_KEY=<your-runpod-api-key>
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
engineering@benediktsvogler.com or via [benediktsvogler.com](https://benediktsvogler.com).

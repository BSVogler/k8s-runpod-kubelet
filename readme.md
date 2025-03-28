# Virtual Kubelet for RunPod

## Overview

This Virtual Kubelet implementation provides seamless integration between Kubernetes and RunPod, enabling dynamic, cloud-native GPU workload scaling across local and cloud resources.

### Key Features

- **Dynamic Cloud Bursting**: Automatically extend Kubernetes GPU capabilities using RunPod
- **Transparent Integration**: Appears as a native Kubernetes node
- **Resource Management**:
   - Automatically manages pod lifecycle across local and cloud environments
   - Handles pod creation, status tracking, and termination
- **Flexible Scheduling**:
   - Supports dynamic offloading of GPU workloads
   - Intelligent resource selection based on job requirements

## How It Works

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

## Prerequisites

- Kubernetes cluster
- RunPod account and API key
- Go 1.19+

## Installation

### Building

```bash
go build -o runpod-virtual-kubelet ./cmd/virtual-kubelet
```

### Configuration

Configure using environment variables or command-line flags:

- `RUNPOD_API_KEY`: RunPod API authentication
- `KUBELET_NODE_NAME`: Name of the virtual node
- `RECONCILIATION_INTERVAL`: Frequency of status updates

### Deployment

```bash
kubectl apply -f deploy/virtual-kubelet.yaml
```

## Job Annotations

Customize workload behavior with annotations:

- `runpod.io/managed: "true"`: Enable RunPod management
- `runpod.io/required-gpu-memory`: Specify minimum GPU memory

## Monitoring

### Logs
```bash
kubectl logs -n kube-system deployment/runpod-virtual-kubelet
```

### Health Checks
```bash
# Liveness probe
curl http://virtual-kubelet-service:8080/healthz

# Readiness probe
curl http://virtual-kubelet-service:8080/readyz
```

## Example Pod Specification

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: gpu-workload
  labels:
    runpod.io/managed: "true"
  annotations:
    runpod.io/required-gpu-memory: "16"
spec:
  template:
    spec:
      containers:
      - name: gpu-task
        image: my-gpu-image:latest
        resources:
          requests:
            nvidia.com/gpu: 1
          limits:
            nvidia.com/gpu: 1
      restartPolicy: Never
```

## Limitations

- No direct container exec/attach support

## Contributing

Contributions welcome! Please submit pull requests or open issues on the project repository.
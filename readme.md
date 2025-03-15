# Kubernetes RunPod Controller

This controller watches annotated Kubernetes Jobs wand offloads them to RunPod when resources are constrained or when explicitly requested. It maintains proper Kubernetes representations of RunPod instances and handles cleanup automatically.
The idea is that you have some local node that can process but you want to offload as a fallback scenario.

## Overview

The controller periodically scans all Jobs and offloads them to RunPod in the following scenarios:

1. Jobs that can be offloaded must be annotated with `runpod.io/managed: "true"`.
2. And when Jobs have been pending for more than [configurable seconds](#configuration) (default 0)
3. or when the number of pending jobs exceeds a [configurable threshold](#configuration) (default 1)

you can also force it by setting annotation `runpod.io/offload: "true"` to the Job.

## Key Features

- **Virtual Pod Representation**: Creates Kubernetes Pod objects to represent RunPod instances
- **Lifecycle Management**: Automatically terminates RunPod instances when corresponding Jobs are deleted
- **Resource Optimization**: Selects cost-effective GPUs from RunPod based on memory requirements
- **Seamless Integration**: Jobs offloaded to RunPod appear as running Jobs in Kubernetes
- **Health Monitoring**: Provides HTTP health and readiness endpoints

## Installation

### Prerequisites

- Kubernetes cluster with access to create/update Jobs and Pods
- RunPod API key
- Go 1.19+

### Building

```bash
go build -o runpod-controller ./cmd/runpod_controller
```

### Virtual Node Setup

First, create the virtual node that will host RunPod instances:

```bash
kubectl apply -f deploy/virtual-node-setup.yaml
```

This creates a non-schedulable virtual node that serves as a placeholder for RunPod instances.

### Deploying to Kubernetes

Create a secret with your RunPod API key:

```bash
kubectl create secret generic runpod-controller-secrets -n kube-system \
  --from-literal=RUNPOD_KEY=your_runpod_api_key
```

Deploy using the provided manifest:

```bash
kubectl apply -f deploy/runpod-controller.yaml
```

## Configuration

The controller can be configured with the following flags:

- `--kubeconfig`: Path to kubeconfig file (only for running outside the cluster)
- `--reconcile-interval`: How often to check for jobs (in seconds, default: 30)
- `--pending-job-threshold`: Number of pending jobs that triggers automatic offloading (default: 1)
- `--max-pending-time`: Maximum time that a job is allowed to stay in pending state before it is offloaded (default: 0)
- `--max-gpu-price`: Maximum price per hour for GPU instances (default: 0.5)
- `--health-server-address`: Address for the health check server (default: :8080)

## Job Annotations

To use the controller, annotate your GPU Jobs with these annotations:

- `runpod.io/managed: "true"`: Indicates that this job can be managed by the controller
- `runpod.io/offload: "true"`: Explicitly request offloading to RunPod
- `runpod.io/required-gpu-memory: "16"`: Specify minimum GPU memory required (in GB)

Example:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: my-gpu-job
  annotations:
    runpod.io/managed: "true"
    runpod.io/offload: "true"
    runpod.io/required-gpu-memory: "16"
spec:
  template:
    spec:
      containers:
      - name: my-container
        image: my-gpu-image:latest
        resources:
          requests:
            nvidia.com/gpu: 1
          limits:
            nvidia.com/gpu: 1
      restartPolicy: Never
```

## How It Works

1. **Job Detection**: The controller regularly scans for Jobs with GPU resource requests
2. **Offloading Decision**: When a Job meets the criteria for offloading, it proceeds with the RunPod deployment
3. **RunPod Deployment**:
   - Extracts environment variables and container specs
   - Finds suitable GPU types on RunPod within price constraints
   - Creates a deployment on RunPod
4. **Kubernetes Integration**:
   - Creates a virtual Pod representation linked to the Job
   - Sets the Pod as "Running" on the virtual node
   - Updates the original Job with annotations tracking the RunPod deployment
5. **Cleanup**:
   - Monitors for deleted Jobs
   - Terminates the corresponding RunPod instances when their Jobs are deleted
   - Cleans up virtual Pod representations

## Monitoring

The controller logs all actions and errors. To check logs:

```bash
kubectl logs -f deployment/runpod-controller
```

Health and readiness endpoints are available:

```bash
# Check liveness
curl http://<controller-service-ip>:8080/healthz

# Check readiness
curl http://<controller-service-ip>:8080/readyz
```

To view RunPod instances represented in Kubernetes:

```bash
kubectl get pods -l runpod.io/managed=true
```

To check Job annotations after offloading:

```bash
kubectl get job my-job -o jsonpath='{.metadata.annotations}'
```

## Development

### Local Development

For local development, you can run:

```bash
go run ./cmd/runpod_controller --kubeconfig=$HOME/.kube/config
```

### Running Tests

```bash
go test ./...
```
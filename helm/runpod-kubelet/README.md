# RunPod Virtual Kubelet Helm Chart

This Helm chart deploys the RunPod Virtual Kubelet to your Kubernetes cluster, allowing you to schedule pods on RunPod GPU instances.

## Prerequisites

- Kubernetes 1.19+
- Helm 3+
- RunPod API Key

## Installation

### Option 1: Install from OCI Registry (if published to GHCR)

```bash
helm install runpod-kubelet oci://ghcr.io/bsvogler/charts/runpod-kubelet \
  --namespace kube-system \
  --set runpod.apiKey=YOUR_RUNPOD_API_KEY
```

### Option 2: Install from Helm Repository (if published to GitHub Pages)

```bash
helm repo add runpod-kubelet https://bsvogler.github.io/k8s-runpod-kubelet/
helm repo update
helm install runpod-kubelet runpod-kubelet/runpod-kubelet \
  --namespace kube-system \
  --set runpod.apiKey=YOUR_RUNPOD_API_KEY
```

### Option 3: Install from local directory

```bash
helm install runpod-kubelet ./helm/runpod-kubelet \
  --namespace kube-system \
  --set runpod.apiKey=YOUR_RUNPOD_API_KEY
```

### Install with existing secret

If you already have a Kubernetes secret containing your RunPod API key:

```bash
kubectl create secret generic my-runpod-secret \
  --namespace kube-system \
  --from-literal=RUNPOD_API_KEY=YOUR_RUNPOD_API_KEY

helm install runpod-kubelet ./helm/runpod-kubelet \
  --namespace kube-system \
  --set runpod.existingSecret=my-runpod-secret
```

## Configuration

The following table lists the configurable parameters and their default values:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas | `1` |
| `image.repository` | Image repository | `ghcr.io/bsvogler/gpu-runpod-kubelet` |
| `image.tag` | Image tag | `latest` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Image pull secrets | `[{name: regcred}]` |
| `serviceAccount.create` | Create service account | `true` |
| `serviceAccount.name` | Service account name | `""` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `128Mi` |
| `resources.limits.cpu` | CPU limit | `200m` |
| `resources.limits.memory` | Memory limit | `256Mi` |
| `kubelet.reconcileInterval` | Reconciliation interval in seconds | `30` |
| `kubelet.maxGpuPrice` | Maximum GPU price per hour | `0.5` |
| `kubelet.healthServerAddress` | Health server address | `:8080` |
| `kubelet.namespace` | Namespace to deploy in | `kube-system` |
| `runpod.apiKey` | RunPod API key | `""` |
| `runpod.existingSecret` | Name of existing secret with API key | `runpod-kubelet-secrets` |
| `health.port` | Health check port | `8080` |
| `health.livenessProbe.enabled` | Enable liveness probe | `true` |
| `health.readinessProbe.enabled` | Enable readiness probe | `true` |

## Usage

Once installed, the virtual kubelet will create a node in your cluster. You can schedule pods on RunPod by using node selectors:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-pod
spec:
  nodeSelector:
    kubernetes.io/hostname: runpod-node
  containers:
  - name: gpu-container
    image: nvidia/cuda:11.0-base
    resources:
      limits:
        nvidia.com/gpu: 1
```

## Uninstalling

```bash
helm uninstall runpod-kubelet --namespace kube-system
```

## Troubleshooting

Check the kubelet logs:

```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=runpod-kubelet
```

Check the virtual node status:

```bash
kubectl get nodes
kubectl describe node runpod-node
```
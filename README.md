# Conduit Kubelet

Conduit Kubelet is a [virtual kubelet](https://virtual-kubelet.io/) that adds a
node to your Kubernetes cluster on which GPU pods run in the cloud. You write a
normal Pod spec, schedule it onto the `conduit-node`, and the
[Conduit platform](https://gpuconduit.io) picks a GPU provider (RunPod today),
starts the container there and reports its status back into `kubectl`. The
kubelet itself is a thin, source-available client: it watches pods, talks to
the platform over one outbound WebSocket, and calls the provider API when the
platform tells it to.

```
kubectl apply  ──►  Kubernetes  ──►  conduit-kubelet (this repo, in your cluster)
                                            │  wss://gpuconduit.io/api/kubelet/ws
                                            ▼
                                      Conduit platform  ──►  GPU provider (RunPod, ...)
```

## Why it's safe to run

- **Thin client, no business logic.** The kubelet only executes `deploy`,
  `status` and `terminate` commands it receives from the platform and reports
  pod lifecycle events. Provider selection, quotas and billing live on the
  platform; there is nothing here that can spend money on its own.
- **Source-available.** Everything that runs in your cluster is in this
  repository. Build the image yourself (see [Building from source](#building-from-source))
  and pin the digest.
- **Your provider key can stay in your cluster.** Set `runpod.apiKey` (or put
  `RUNPOD_API_KEY` in your own Secret) and the kubelet calls RunPod directly
  with it; the platform then only sends commands without credentials
  ("local key mode"). Alternatively store the key in your Conduit account and
  the platform sends it with each deploy command over TLS ("platform key
  mode"). Keys are never logged.
- **Outbound-only.** The kubelet opens one WebSocket to
  `wss://gpuconduit.io/api/kubelet/ws` and reconnects with backoff. It needs no
  inbound port and no ingress. The health server on `:8080` is only for probes.
- **Least privilege container.** Static binary on
  `gcr.io/distroless/static:nonroot`, read-only root filesystem, all
  capabilities dropped. The ClusterRole is the standard virtual-kubelet set
  (pods, nodes, events, leases, plus read access to ConfigMaps/Secrets/Services
  referenced by pods scheduled to the virtual node).
- **Audit it.** What leaves the cluster is defined in
  [`pkg/websocket/protocol.go`](pkg/websocket/protocol.go) (`Event`,
  `Registration`, `Heartbeat`, `Response`). `pod_created` carries the full pod
  spec and annotations of pods scheduled to the virtual node; nothing else
  about your cluster is sent. Useful greps:

  ```bash
  grep -rn "SendEvent\|SendRegistration" pkg/      # everything sent to the platform
  grep -rn "APIKey\|api_key" pkg/ | grep -i log    # confirm keys are not logged
  cat pkg/providers/runpod/client.go               # the only provider API calls
  ```

## Quick start

### 1. Get a token

Create an account at <https://gpuconduit.io/register/> and generate a kubelet
token on the installation page: <https://gpuconduit.io/dashboard/installation/>.
Optionally add your RunPod API key to your account under Providers, or keep it
in the cluster as shown below.

### 2. Install the chart

Platform key mode (RunPod key stored in your Conduit account):

```bash
helm install conduit-kubelet oci://ghcr.io/bsvogler/helm/conduit-kubelet \
  --namespace conduit-system --create-namespace \
  --set conduit.apiToken=YOUR_CONDUIT_TOKEN
```

Local key mode (RunPod key stays in your cluster):

```bash
helm install conduit-kubelet oci://ghcr.io/bsvogler/helm/conduit-kubelet \
  --namespace conduit-system --create-namespace \
  --set conduit.apiToken=YOUR_CONDUIT_TOKEN \
  --set runpod.apiKey=YOUR_RUNPOD_API_KEY
```

Or manage the Secret yourself and reference it:

```bash
kubectl create namespace conduit-system
kubectl -n conduit-system create secret generic conduit-kubelet-credentials \
  --from-literal=CONDUIT_API_TOKEN=YOUR_CONDUIT_TOKEN \
  --from-literal=RUNPOD_API_KEY=YOUR_RUNPOD_API_KEY      # optional

helm install conduit-kubelet oci://ghcr.io/bsvogler/helm/conduit-kubelet \
  --namespace conduit-system \
  --set conduit.existingSecret=conduit-kubelet-credentials
```

Then check that the kubelet is connected and the virtual node is registered:

```bash
kubectl -n conduit-system get pods            # kubelet pod should be 1/1 Ready
kubectl get node conduit-node                 # STATUS Ready, ROLES agent
```

### 3. Run your first GPU pod

The virtual node is tainted so that regular workloads never land on it. A pod
opts in with a toleration and (recommended) a nodeSelector:

```yaml
# gpu-smoke-test.yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-smoke-test
  annotations:
    runpod.io/required-gpu-memory: "16"   # GB of GPU memory, see Annotations
spec:
  restartPolicy: Never
  nodeSelector:
    conduit.io/provider: "true"
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Equal
      value: conduit
      effect: NoSchedule
  containers:
    - name: cuda
      image: nvidia/cuda:12.4.1-base-ubuntu22.04
      command: ["nvidia-smi"]
      resources:
        limits:
          nvidia.com/gpu: 1
```

```bash
kubectl apply -f gpu-smoke-test.yaml
```

### 4. Watch it

```bash
kubectl get pod gpu-smoke-test -w                 # Pending -> Running -> Succeeded
kubectl describe pod gpu-smoke-test               # provider, reason and message
kubectl -n conduit-system logs deploy/conduit-kubelet -f
```

The pod stays `Pending` until the platform has answered. If the platform
refuses the pod it is marked `Failed` and `kubectl describe pod` shows the
reason:

| Reason | Meaning | What to do |
|---|---|---|
| `quota_exceeded` | Your plan's concurrent pod or spend limit is reached. | Wait for running pods to finish or upgrade at <https://gpuconduit.io/dashboard/billing/>. |
| `no_api_key` | No RunPod key is available: none in your Conduit account and none in the cluster. | Add a key under Providers in the dashboard or install with `runpod.apiKey`. |
| `unsupported_provider` | `conduit.io/provider` names a provider that is not available. | Use `runpod` or remove the annotation. |
| `conversion_error` | The pod spec could not be turned into a provider request (unsupported feature, bad annotation value). | Check the message; see [Annotations](#annotations). |
| `provider_error` | The provider refused the deployment (no capacity at your price, bad image, ...). | Check the message; relax `runpod.io/max-price` or `runpod.io/datacenter-ids`. |

Plans and pricing: <https://gpuconduit.io/pricing/>.

## Configuration

### Helm values

| Value | Default | Environment variable | Description |
|---|---|---|---|
| `conduit.apiToken` | `""` | `BACKEND_API_KEY` | Kubelet token from the dashboard. Required unless `conduit.existingSecret` is set. |
| `conduit.existingSecret` | `""` | | Name of a Secret in the release namespace with key `CONDUIT_API_TOKEN` and optionally `RUNPOD_API_KEY`. The chart then creates no Secret. |
| `conduit.url` | `wss://gpuconduit.io/api/kubelet/ws` | `BACKEND_URL` | Platform WebSocket URL. |
| `cluster.name` | `default` | `CLUSTER_NAME` | Cluster name shown in the dashboard. |
| `runpod.apiKey` | `""` | `RUNPOD_API_KEY` | Optional. Keeps your RunPod key in the cluster (local key mode). |
| `kubelet.nodeName` | `conduit-node` | `NODE_NAME` | Name of the virtual node. |
| `kubelet.namespace` | release namespace | `NAMESPACE` | Namespace the kubelet runs in. |
| `kubelet.logLevel` | `info` | `LOG_LEVEL` | `debug`, `info`, `warn`, `error`. |
| `kubelet.healthServerAddress` | `:8080` | `--health-server-address` | Listen address for `/healthz`, `/readyz`, `/status`; the probes use its port. |
| `kubelet.reconcileInterval` | `30` | `--reconcile-interval` | Informer resync interval in seconds. |
| `image.repository` / `image.tag` / `image.pullPolicy` | `ghcr.io/bsvogler/conduit-kubelet` / chart `appVersion` / `IfNotPresent` | | Image. Pin `image.tag` to a release tag or a digest you built yourself. |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | see `values.yaml` | | Scheduling of the kubelet pod itself (it must run on a real node). |
| `livenessProbe` / `readinessProbe` | `/healthz` / `/readyz` | | `readyz` fails while the WebSocket is disconnected. |
| `serviceAccount.*`, `rbac.create` | create | | ServiceAccount and ClusterRole/Binding. |

The chart source is in [`deploy/helm/conduit-kubelet`](deploy/helm/conduit-kubelet).

### Binary flags

Every setting is also a flag of the binary (`conduit-kubelet --help`):
`--backend-url`, `--backend-api-key`, `--cluster-name`, `--nodename`,
`--namespace`, `--log-level`, `--health-server-address`,
`--reconcile-interval`, `--kubeconfig` (outside the cluster), `--version`.
Flags take precedence over environment variables.

### The virtual node

The kubelet registers one node named `kubelet.nodeName` with:

- labels `conduit.io/provider=true`, `type=virtual-kubelet`,
  `kubernetes.io/role=agent`, `kubernetes.io/hostname=<nodeName>`
- taint `virtual-kubelet.io/provider=conduit:NoSchedule`
- capacity `nvidia.com/gpu: 1000` (the real limit is your plan)

Pods reach it with the toleration and nodeSelector shown in the quick start.
Labels and taints are set when the node is created; if you upgrade from a
version that registered the node differently, delete the old node object once
(`kubectl delete node conduit-node`) and let the kubelet recreate it.

## Annotations

Pod annotations are forwarded to the platform, which converts the pod into a
provider request. The platform currently honours:

| Annotation | Default | Description |
|---|---|---|
| `conduit.io/provider` | `runpod` | Provider to deploy to. Only `runpod` is available at the moment. |
| `runpod.io/required-gpu-memory` | `16` | Minimum GPU memory in GB. Used to choose the GPU type. |
| `runpod.io/max-price` | `0.5` | Maximum price per GPU-hour in USD. |
| `runpod.io/cloud-type` | `SECURE` | `SECURE` or `COMMUNITY`. |
| `runpod.io/templateId` | | RunPod template to start from (for example one with registry credentials). |
| `runpod.io/container-registry-auth-id` | | RunPod registry credential for private images; takes precedence over the template's. |
| `runpod.io/datacenter-ids` | any | Comma-separated list of allowed RunPod datacenters, e.g. `US-NJ-1,EU-RO-1`. |
| `runpod.io/ports` | auto | Override port detection, e.g. `8080/http,22/tcp`. Without it, `containerPort`s 80, 443, 3000, 5000, 8000, 8080, 8888, 9000 are exposed as HTTP and everything else as TCP. |

From the pod spec itself the platform uses the first container's `image`,
`command`/`args`, `env`, `ports` and the `nvidia.com/gpu` limit. Volumes,
init containers, probes and sidecars are not supported on the virtual node.

## Building from source

Requires Go 1.24+.

```bash
git clone https://github.com/BSVogler/k8s-runpod-kubelet conduit-kubelet
cd conduit-kubelet
go build -ldflags "-X main.version=$(git describe --tags --always)" \
  -o conduit-kubelet ./cmd/virtual_kubelet
go test ./...
```

Container image (multi-stage, distroless, non-root):

```bash
docker build --build-arg VERSION=$(git describe --tags --always) \
  -t my-registry/conduit-kubelet:mine .
helm upgrade --install conduit-kubelet deploy/helm/conduit-kubelet \
  --namespace conduit-system --create-namespace \
  --set conduit.apiToken=YOUR_CONDUIT_TOKEN \
  --set image.repository=my-registry/conduit-kubelet --set image.tag=mine
```

Run against a cluster from your machine:

```bash
./conduit-kubelet --kubeconfig ~/.kube/config \
  --backend-api-key YOUR_CONDUIT_TOKEN --log-level debug
```

Release binaries for linux/darwin on amd64/arm64 with `checksums.txt` are
attached to every [GitHub release](https://github.com/BSVogler/k8s-runpod-kubelet/releases).
Images are published as `ghcr.io/bsvogler/conduit-kubelet:<tag>` (and `latest`
for the default branch), the chart as
`oci://ghcr.io/bsvogler/helm/conduit-kubelet:<version>`.

Developer notes (protocol, adding providers, debugging) are in
[README.dev.md](README.dev.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Legacy

The previous standalone, RunPod-only kubelet (no platform, all routing inside
the kubelet) lives on the `legacy` branch and tag `v1-legacy`. It is
unsupported.

## License

Source-available under the [PolyForm Strict License 1.0.0](LICENSE): you may
read, build and use the software for noncommercial purposes; redistribution
and derivative works are not permitted. Commercial use is licensed through the
Conduit platform: signing up at <https://gpuconduit.io> and running the kubelet
against your account is covered by your plan (<https://gpuconduit.io/pricing/>).
For anything else contact <engineering@benediktsvogler.com>.

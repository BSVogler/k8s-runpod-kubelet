# Conduit Kubelet

**Source-available Kubernetes connector for seamless GPU cloud access**

Conduit Kubelet brings GPU cloud providers (RunPod, Vast.ai, Salad, and more) directly into your Kubernetes cluster. Deploy GPU workloads using familiar `kubectl` commands while the Conduit platform handles provider selection, cost optimization, and billing—all while you maintain full control and transparency through source-available code you can audit and compile yourself.

---

## Why Source-Available?

**Trust Through Transparency**

We believe in earning your trust through complete transparency. Conduit Kubelet is source-available, allowing you to:

- **Audit the code** - See exactly what runs in your cluster
- **Compile from source** - Build the binary yourself for complete confidence
- **Verify security** - Review how your credentials are handled
- **Report issues** - Help us improve security and reliability

The kubelet runs in your cluster and only communicates with the Conduit platform—no hidden telemetry, no black boxes.

**Smart Separation**

- **Source-Available (This Repo)**: The kubelet that runs in your cluster - fully auditable
- **Conduit Platform**: The routing intelligence, cost optimization, and billing management - our secret sauce

This architecture ensures you have full visibility into what runs in your infrastructure while we focus on making GPU access simple and cost-effective.

---

## How It Works

```
Your K8s Cluster → Conduit Kubelet (open source) → Conduit Platform → GPU Providers
```

1. **You deploy pods** using standard Kubernetes YAML
2. **Kubelet detects** the pod and reports requirements to Conduit
3. **Conduit selects** the best provider based on availability, cost, and your preferences
4. **Kubelet executes** the deployment command from Conduit
5. **Your pod runs** on the selected GPU provider (RunPod, Vast.ai, etc.)

**What You Control:**
- When and how pods are deployed (standard Kubernetes)
- Which providers you want to use (configure in Conduit dashboard)
- Your provider API keys (stored in your cluster or managed by Conduit)
- All the source code running in your cluster

**What Conduit Handles:**
- Provider selection and availability monitoring
- Cost optimization across providers
- Plan limits and quota enforcement
- Billing and usage tracking

---

## Quick Start

### Prerequisites

- Kubernetes cluster (1.19+)
- `kubectl` access with admin permissions
- Conduit account (sign up at [conduit.example.com](https://conduit.example.com))

### Installation

#### Option 1: Using Pre-built Releases (Recommended)

```bash
# Download the latest release
curl -LO https://github.com/yourusername/conduit-kubelet/releases/latest/download/conduit-kubelet-linux-amd64

# Verify checksum (optional but recommended)
curl -LO https://github.com/yourusername/conduit-kubelet/releases/latest/download/checksums.txt
sha256sum -c checksums.txt

# Install
chmod +x conduit-kubelet-linux-amd64
sudo mv conduit-kubelet-linux-amd64 /usr/local/bin/conduit-kubelet
```

#### Option 2: Build from Source

```bash
# Clone the repository
git clone https://github.com/yourusername/conduit-kubelet
cd conduit-kubelet

# Verify you're on a release tag (recommended for production)
git checkout v1.0.0

# Review the code (audit for security/trust)
# ... take your time, this is your infrastructure ...

# Install Go dependencies
go mod download

# Build the binary
go build -o conduit-kubelet ./cmd/virtual_kubelet

# Optionally, run tests
go test ./...

# Install
sudo mv conduit-kubelet /usr/local/bin/
```

### Deploy to Kubernetes

```bash
# Get your Conduit API key from the dashboard
export CONDUIT_API_KEY="your-api-key-from-dashboard"

# Create secret
kubectl create secret generic conduit-kubelet-auth \
  --from-literal=api-key=$CONDUIT_API_KEY \
  -n kube-system

# Deploy the kubelet
kubectl apply -f https://raw.githubusercontent.com/yourusername/conduit-kubelet/main/deploy/kubelet.yaml

# Verify deployment
kubectl get pods -n kube-system -l app=conduit-kubelet
```

### Deploy Your First GPU Pod

```yaml
# gpu-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-training
  labels:
    conduit.io/gpu-enabled: "true"  # Tells kubelet to manage this pod
spec:
  containers:
  - name: training
    image: nvidia/cuda:11.8-runtime-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
```

```bash
kubectl apply -f gpu-pod.yaml

# Watch it come up
kubectl get pods -w

# Check which provider was selected
kubectl describe pod gpu-training
```

---

## Configuration

### Required: Platform Connection

Connect the kubelet to your Conduit account:

```bash
kubectl create secret generic conduit-kubelet-auth \
  --from-literal=api-key=YOUR_CONDUIT_API_KEY \
  -n kube-system
```

Get your API key from: [conduit.example.com/settings/api-keys](https://conduit.example.com/settings/api-keys)

### Optional: Local Provider Keys

For self-hosted deployments or hybrid scenarios, you can store provider API keys locally:

```bash
kubectl create secret generic conduit-provider-keys \
  --from-literal=runpod-key=YOUR_RUNPOD_KEY \
  --from-literal=vastai-key=YOUR_VASTAI_KEY \
  -n kube-system
```

**Key Priority:**
1. **Conduit-managed** - If you've added keys to Conduit dashboard, they're used first
2. **Local cluster** - Falls back to keys stored in your cluster
3. **Error** - If neither exists, deployment fails with clear error message

**Why use Conduit-managed keys?**
- Multi-cluster deployments (share keys across clusters)
- Team collaboration (centralized key management)
- Key rotation (update once, applies everywhere)

**Why use local keys?**
- Air-gapped environments
- Strict security policies requiring keys stay in-cluster
- Self-hosted deployments

---

## Supported GPU Providers

| Provider | Status | Example GPUs | Best For |
|----------|--------|--------------|----------|
| **RunPod** | ✅ Production | RTX 4090, A100, H100 | High-performance training, inference |
| **Vast.ai** | 🚧 Beta | RTX 3090, RTX 4090, A100 | Cost-effective training, batch jobs |
| **Salad** | 📋 Coming Soon | RTX 3060, RTX 4060 | Distributed inference, rendering |
| **AWS** | 📋 Planned | p3, p4, g5 instances | Enterprise workloads, compliance |
| **GCP** | 📋 Planned | A2, N1 with GPUs | Enterprise workloads, multi-cloud |

Don't see your provider? [Open an issue](https://github.com/yourusername/conduit-kubelet/issues) or contribute support!

---

## How Conduit Saves You Money

The Conduit platform continuously monitors pricing and availability across providers:

- **Dynamic provider selection** - Always uses the cheapest available option
- **Spot instance optimization** - Automatically migrates before eviction
- **Multi-provider fallback** - If one provider is unavailable, tries others
- **Cost alerts** - Get notified before hitting budget limits

**Example savings:**

```
Training job: 8x A100 for 4 hours

Option 1: AWS p4d.24xlarge
Cost: $32.77/hr × 4 = $131.08

Option 2: Conduit auto-selection
- Starts on RunPod ($2.89/hr per GPU × 8 = $23.12/hr)
- Migrates to Vast.ai after 2hr ($1.90/hr per GPU × 8 = $15.20/hr)
Total: ($23.12 × 2) + ($15.20 × 2) = $76.64

Savings: $54.44 (42% cheaper)
```

---

## Security & Privacy

### What Data Does the Kubelet Send?

The kubelet only sends necessary operational data to Conduit:

**Sent to Conduit:**
- Pod specifications (image, resource requests, labels)
- Pod lifecycle events (created, running, terminated)
- Provider pod status (for monitoring)

**Never sent:**
- Application logs or output
- Environment variables (except Conduit-specific annotations)
- Secrets or ConfigMaps
- Data processed by your pods

### How Are API Keys Handled?

**Conduit-managed keys:**
- Stored encrypted at rest in Conduit platform
- Transmitted over TLS (WSS) when needed for deployment
- Used in-memory by kubelet, never written to disk
- Never logged or exposed

**Local keys:**
- Stored as Kubernetes secrets in your cluster
- Never sent to Conduit platform
- Kubelet reads from cluster secrets only when needed

### Network Security

- **Outbound only** - Kubelet initiates all connections (no inbound ports)
- **TLS encrypted** - All communication uses WebSocket Secure (WSS)
- **Certificate pinning** - Optional for extra security (see docs)
- **Provider isolation** - Provider API calls originate from your cluster

### Audit the Code

Security claims are meaningless without verification:

```bash
# Clone and review
git clone https://github.com/yourusername/conduit-kubelet
cd conduit-kubelet

# Check what data is sent to platform
grep -r "websocket.Event" pkg/

# Verify no secrets are logged
grep -r "logger.*api.*key" pkg/

# Review provider API calls
cat pkg/providers/runpod/client.go
```

**Found a security issue?** Report it privately: [security@conduit.example.com](mailto:security@conduit.example.com)

---

## Troubleshooting

### Kubelet Not Connecting

```bash
# Check kubelet logs
kubectl logs -n kube-system -l app=conduit-kubelet

# Verify API key
kubectl get secret conduit-kubelet-auth -n kube-system -o jsonpath='{.data.api-key}' | base64 -d

# Test connectivity
kubectl exec -n kube-system deployment/conduit-kubelet -- \
  curl -I https://api.conduit.example.com/health
```

### Pod Stuck in Pending

```bash
# Check for rejection events
kubectl describe pod <pod-name>

# View kubelet logs
kubectl logs -n kube-system -l app=conduit-kubelet | grep -i error

# Check Conduit dashboard for status
# → https://conduit.example.com/clusters
```

### Provider API Errors

```bash
# Enable debug logging
kubectl set env deployment/conduit-kubelet -n kube-system LOG_LEVEL=debug

# Check provider status
kubectl exec -n kube-system deployment/conduit-kubelet -- \
  curl localhost:8080/status
```

**Still stuck?**
- Documentation: [docs.conduit.example.com](https://docs.conduit.example.com)
- Community: [GitHub Discussions](https://github.com/yourusername/conduit-kubelet/discussions)
- Support: [support@conduit.example.com](mailto:support@conduit.example.com)

---

## Upgrading

### Using Pre-built Releases

```bash
# Check current version
conduit-kubelet --version

# Download new version
curl -LO https://github.com/yourusername/conduit-kubelet/releases/latest/download/conduit-kubelet-linux-amd64

# Update deployment
kubectl set image deployment/conduit-kubelet \
  conduit-kubelet=conduit/kubelet:v1.1.0 \
  -n kube-system
```

### Building from Source

```bash
# Pull latest code
git pull
git checkout v1.1.0

# Rebuild
go build -o conduit-kubelet ./cmd/virtual_kubelet

# Update image and redeploy
docker build -t myregistry/conduit-kubelet:v1.1.0 .
docker push myregistry/conduit-kubelet:v1.1.0
kubectl set image deployment/conduit-kubelet \
  conduit-kubelet=myregistry/conduit-kubelet:v1.1.0 \
  -n kube-system
```

---

## FAQ

**Q: Is Conduit Kubelet really free and open source?**
A: The kubelet source code is publicly available for audit and compilation, but it's under a proprietary license. The Conduit platform service has free and paid tiers.

**Q: Can I use this without the Conduit platform?**
A: The kubelet requires a connection to Conduit for routing decisions. For direct provider integration, see [k8s-runpod-kubelet](https://github.com/yourusername/k8s-runpod-kubelet).

**Q: How is this different from Karpenter or other autoscalers?**
A: Conduit focuses specifically on GPU workloads across multiple clouds. It's a virtual kubelet, not an autoscaler—it doesn't manage node groups, it routes individual pods to GPU providers.

**Q: What happens if the Conduit platform goes down?**
A: Running pods continue unaffected. New pods remain in Pending until connection is restored. The kubelet retries with exponential backoff.

**Q: Can I self-host the Conduit platform?**
A: Not currently, but we're considering enterprise self-hosted options. [Let us know](https://github.com/yourusername/conduit-kubelet/discussions) if you're interested.

---

## License

This project is **source-available** under a proprietary license - see the [LICENSE](LICENSE) file for details.

The source code is publicly available for:
- **Audit and security review** - Verify what runs in your infrastructure
- **Compilation** - Build trusted binaries yourself
- **Bug reporting** - Help us improve quality

**Restrictions:**
- No redistribution of modified versions
- No commercial use without permission
- No creation of derivative works

For commercial licensing or enterprise deployments, contact [sales@gpuconduit.io](mailto:sales@gpuconduit.io).

---

## Project Links

- **Website**: [gpuconduit.io](https://gpuconduit.io)
- **Documentation**: [docs.conduit.example.com](https://docs.conduit.example.com)
- **GitHub**: [github.com/yourusername/conduit-kubelet](https://github.com/yourusername/conduit-kubelet)
- **Community**: [GitHub Discussions](https://github.com/yourusername/conduit-kubelet/discussions)
- **Status**: [status.conduit.example.com](https://status.conduit.example.com)

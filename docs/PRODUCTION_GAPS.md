# Production Readiness: Gap Analysis

**Document Version:** 1.0
**Last Updated:** 2025-11-02
**Target:** Production launch for SaaS platform

## Executive Summary

This document outlines what's **missing** for a production launch where conduit-kubelet operates as a pure proxy for a SaaS platform. The analysis focuses on critical features needed for the initial customer launch.

**Current Status:** 🟡 **Functional MVP** - Core functionality works, but missing production-critical features

**Production Readiness:** 🔴 **Not Ready** - Critical gaps must be addressed before go-live

---

## Critical Gaps (Must Have for Launch)

### 1. Platform Rejection Handling ⚠️ CRITICAL

**Current State:**
- ❌ No handling for platform rejections
- ❌ No distinction between provider failures and platform policy rejections
- ❌ All errors treated the same (command failure response)

**What's Missing:**
- Rejection message type in WebSocket protocol
- Error codes for rejection scenarios
- Kubernetes event creation for rejections
- Pod status handling (keep pending vs fail)

**Required For Launch:**
```go
// pkg/websocket/protocol.go
type RejectionMessage struct {
    Type        string                 `json:"type"`  // "rejection"
    Code        string                 `json:"code"`  // "PLAN_LIMIT_EXCEEDED"
    Message     string                 `json:"message"`
    Action      string                 `json:"action"`  // "keep_pending" or "fail_pod"
    PodName     string                 `json:"pod_name"`
    Details     map[string]interface{} `json:"details"`
    Remediation *Remediation           `json:"remediation,omitempty"`
}

type Remediation struct {
    Title       string `json:"title"`
    Description string `json:"description"`
    URL         string `json:"url"`
    CTA         string `json:"cta"`
}
```

**Error Codes Needed:**
- `PLAN_LIMIT_EXCEEDED` - User at concurrent pod limit
- `QUOTA_EXCEEDED` - User/team quota exceeded
- `INSUFFICIENT_CREDITS` - Not enough balance
- `PROVIDER_UNAVAILABLE` - No providers available
- `COST_LIMIT_EXCEEDED` - Pod exceeds cost limits
- `INVALID_CONFIGURATION` - Pod spec invalid

**Implementation Tasks:**
1. Add `RejectionMessage` to WebSocket protocol
2. Implement rejection handler in command processor
3. Create Kubernetes events with remediation info
4. Handle "keep_pending" vs "fail_pod" actions
5. Update pod status based on rejection type
6. Add logging for rejection scenarios

**Files to Modify:**
- `pkg/websocket/protocol.go` - Add rejection message types
- `pkg/command/handler.go` - Add rejection handler
- `pkg/virtual_kubelet/kubelet.go` - K8s event creation

**Testing Requirements:**
- Unit tests for rejection handling
- Integration tests with mock platform rejections
- E2E test with real rejection scenario

**Launch Blocker:** YES - Customer needs plan limits

---

### 2. Platform-Managed API Keys ⚠️ CRITICAL

**Current State:**
- ✅ Providers accept API keys from environment variables
- ❌ No support for API keys sent via commands
- ❌ Providers always read from `os.Getenv()`

**What's Missing:**
- Optional `APIKey` field in `DeployParams`
- Provider code to prefer param key over environment
- Validation and error handling for missing keys
- Secure logging (never log API keys)

**Required Implementation:**
```go
// pkg/websocket/protocol.go
type DeployParams struct {
    APIKey      string `json:"api_key,omitempty"`  // NEW: Platform-provided key
    Image       string `json:"image"`
    GPUType     string `json:"gpu_type"`
    // ... rest of params
}

// pkg/providers/runpod/client.go
func (p *RunPodProvider) Deploy(ctx context.Context, params *DeployParams) (*DeployResult, error) {
    // Use key from command params if provided (SaaS mode)
    apiKey := params.APIKey

    // Fall back to local environment if not provided (self-hosted mode)
    if apiKey == "" {
        apiKey = os.Getenv("RUNPOD_API_KEY")
    }

    if apiKey == "" {
        return nil, &ProviderError{
            Provider: "runpod",
            Code:     "MISSING_API_KEY",
            Message:  "no API key provided and RUNPOD_API_KEY not set",
            Retry:    false,
        }
    }

    // Create client with key
    client := runpod.NewClient(apiKey)
    return client.CreatePod(params)
}
```

**Implementation Tasks:**
1. Add `APIKey` field to all command param structs
2. Update all provider implementations to use param key
3. Add validation for missing keys
4. Ensure keys are never logged (check all log statements)
5. Add configuration for key management mode

**Files to Modify:**
- `pkg/websocket/protocol.go` - Add APIKey to DeployParams, StatusParams, TerminateParams
- `pkg/providers/runpod/client.go` - Update all methods to use param key
- `pkg/providers/vastai/client.go` - (when implemented)
- `pkg/providers/*/client.go` - All future providers

**Security Considerations:**
- Never log API keys (audit all log statements)
- Clear keys from memory after use
- Validate key format before use
- Add rate limiting to prevent key enumeration

**Testing Requirements:**
- Test with platform-provided key
- Test with local environment key
- Test with both (param should take precedence)
- Test with neither (should error clearly)
- Security audit of key handling

**Launch Blocker:** YES - SaaS model requires platform-managed keys

---

### 3. Configuration for Key Management Mode 🔴 HIGH PRIORITY

**Current State:**
- ❌ No configuration to specify key management mode
- ❌ No validation of configuration consistency

**What's Missing:**
- Configuration option to specify mode: `local`, `platform`, or `hybrid`
- Validation that mode matches deployment
- Clear error messages for misconfiguration

**Required Configuration:**
```go
// pkg/config/config.go
type Config struct {
    // Existing fields...

    // Key management configuration
    KeyManagementMode string `json:"key_management_mode"` // "local", "platform", "hybrid"
    RequireAPIKeys    bool   `json:"require_api_keys"`    // Error if no keys available
}
```

**Modes:**
- **local:** Keys must be in environment, reject commands with keys
- **platform:** Keys must come from commands, ignore environment
- **hybrid:** Accept from either source, prefer command params

**Implementation Tasks:**
1. Add configuration options
2. Add validation logic
3. Update provider initialization
4. Add clear error messages for misconfiguration
5. Document mode selection

**Files to Modify:**
- `pkg/config/config.go` - Add configuration fields
- `cmd/virtual_kubelet/main.go` - Add CLI flags
- `pkg/providers/manager.go` - Validate mode on provider calls

**Launch Blocker:** NO - But improves operational clarity

---

### 4. Kubernetes Event Creation 🟡 MEDIUM PRIORITY

**Current State:**
- ❌ No Kubernetes event creation
- ❌ Users don't see rejection reasons in `kubectl describe pod`
- ❌ No visibility into platform decisions

**What's Missing:**
- Event creation helper
- Event creation on rejection
- Event creation on deployment success
- Event creation on deployment failure

**Required Implementation:**
```go
// pkg/virtual_kubelet/events.go (new file)
func (k *Kubelet) CreatePodEvent(pod *v1.Pod, eventType, reason, message string) error {
    event := &v1.Event{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fmt.Sprintf("%s.%x", pod.Name, time.Now().UnixNano()),
            Namespace: pod.Namespace,
        },
        InvolvedObject: v1.ObjectReference{
            Kind:      "Pod",
            Namespace: pod.Namespace,
            Name:      pod.Name,
            UID:       pod.UID,
        },
        Reason:  reason,
        Message: message,
        Type:    eventType,
        FirstTimestamp: metav1.Now(),
        LastTimestamp:  metav1.Now(),
        Count:         1,
    }

    _, err := k.kubeClient.CoreV1().Events(pod.Namespace).Create(context.Background(), event, metav1.CreateOptions{})
    return err
}
```

**Event Types Needed:**
- `PlanLimitExceeded` - Plan limit rejection
- `QuotaExceeded` - Quota rejection
- `InsufficientCredits` - Balance rejection
- `DeploymentStarted` - Provider deployment initiated
- `DeploymentSucceeded` - Provider deployment successful
- `DeploymentFailed` - Provider deployment failed

**Implementation Tasks:**
1. Create event creation helper
2. Call on all rejection scenarios
3. Call on deployment lifecycle events
4. Include remediation URLs in messages
5. Test event visibility in `kubectl describe`

**Files to Modify:**
- `pkg/virtual_kubelet/events.go` (new) - Event helper
- `pkg/command/handler.go` - Create events on command results
- `pkg/virtual_kubelet/kubelet.go` - Create events on lifecycle

**User Experience:**
```bash
$ kubectl describe pod gpu-job-1
...
Events:
  Warning  PlanLimitExceeded  30s  conduit-kubelet
    Free plan allows 2 concurrent pods. Upgrade to Pro for 10 pods:
    https://platform.example.com/upgrade
```

**Launch Blocker:** YES - Critical for user experience

---

## Important Gaps (Should Have for Launch)

### 5. Deprecate Routing Logic in Kubelet 🟡 MEDIUM PRIORITY

**Current State:**
- ⚠️ `ProviderManager.GetAllPricing()` exists (routing logic)
- ⚠️ `ProviderManager.GetAvailability()` exists (routing logic)
- ⚠️ Parallel health checks across providers
- These methods are never called, but exist

**What's Missing:**
- Deprecation warnings
- Documentation that these should not be used
- Eventual removal plan

**Recommendation:**
Add deprecation comments for now, remove in future release:

```go
// pkg/providers/manager.go

// Deprecated: GetAllPricing should not be used in SaaS mode.
// The platform should query provider pricing directly.
// This method is retained for backward compatibility with self-hosted deployments
// and will be removed in v2.0.
func (m *ProviderManager) GetAllPricing(ctx context.Context) (map[string]*PricingResult, error) {
    log.Warn("GetAllPricing is deprecated and should not be used in SaaS mode")
    // ... existing implementation
}

// Deprecated: GetAvailability should not be used in SaaS mode.
// The platform should query provider availability directly.
// This method is retained for backward compatibility with self-hosted deployments
// and will be removed in v2.0.
func (m *ProviderManager) GetAvailability(ctx context.Context, query *AvailabilityQuery) (map[string]*AvailabilityResult, error) {
    log.Warn("GetAvailability is deprecated and should not be used in SaaS mode")
    // ... existing implementation
}
```

**Implementation Tasks:**
1. Add deprecation comments to all routing methods
2. Add warning logs if methods are called
3. Document deprecation in CHANGELOG
4. Plan removal for v2.0

**Files to Modify:**
- `pkg/providers/manager.go` - Add deprecation comments
- `pkg/providers/interface.go` - Document preferred usage

**Launch Blocker:** NO - But improves architecture clarity

---

### 6. Status Polling & Proactive Updates 🟡 MEDIUM PRIORITY

**Current State:**
- ❌ No periodic status polling
- ❌ Platform must explicitly query status via command
- ❌ K8s pod status may be stale

**What's Missing:**
- Periodic status check mechanism (if desired)
- OR platform-driven status polling (recommended)
- Proactive K8s pod status updates

**Two Options:**

**Option A: Platform-Driven (Recommended)**
- Platform polls kubelet periodically with `status` command
- Kubelet executes provider status check
- Kubelet updates K8s pod status
- Returns result to platform
- **Advantage:** Platform controls polling frequency per user/plan
- **No kubelet changes needed**

**Option B: Kubelet-Driven**
- Kubelet polls provider status on interval
- Updates K8s pod status automatically
- Sends `EventPodStatusChange` to platform
- **Advantage:** Lower latency for status updates
- **Disadvantage:** Kubelet decides polling frequency (not plan-aware)

**Recommendation:** Use Option A (platform-driven) for now.

**If Option B Needed Later:**
```go
// pkg/virtual_kubelet/kubelet.go
func (k *Kubelet) startStatusPoller() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for range ticker.C {
        k.pollPodStatuses()
    }
}

func (k *Kubelet) pollPodStatuses() {
    for _, pod := range k.podCache.List() {
        if pod.ProviderPodID == "" {
            continue  // Not deployed yet
        }

        result, err := k.providerManager.GetStatus(context.Background(), pod.Provider, pod.ProviderPodID)
        if err != nil {
            log.Errorf("Failed to get status for pod %s: %v", pod.Name, err)
            continue
        }

        if result.Status != pod.LastKnownStatus {
            // Status changed, update K8s and notify platform
            k.updatePodStatus(pod, result)
            k.sendEventPodStatusChange(pod, result)
        }
    }
}
```

**Launch Blocker:** NO - Platform can poll explicitly for now

---

### 7. Error Recovery & Retries 🟡 MEDIUM PRIORITY

**Current State:**
- ✅ WebSocket auto-reconnection works
- ❌ No command retry logic
- ❌ Failed deployments may leave orphaned resources
- ❌ No cleanup of failed deployments

**What's Missing:**
- Command retry logic for transient failures
- Idempotency tokens for deploy commands
- Cleanup of failed deployments
- Detection of orphaned provider resources

**Recommended Implementation:**
- Platform handles retries (not kubelet)
- Platform includes idempotency token in commands
- Kubelet detects duplicate deploy for same pod
- Kubelet returns existing provider pod ID if already deployed

```go
// pkg/websocket/protocol.go
type CommandMessage struct {
    Type           string      `json:"type"`
    IdempotencyKey string      `json:"idempotency_key"`  // NEW
    Params         interface{} `json:"params"`
}

// pkg/command/handler.go
func (h *CommandHandler) handleDeploy(cmd *CommandMessage) (*ResponseMessage, error) {
    // Check if already deployed
    pod := h.kubelet.GetPod(params.PodName)
    if pod != nil && pod.ProviderPodID != "" {
        // Already deployed, return existing result
        log.Infof("Pod %s already deployed to provider (idempotency: %s)",
                  params.PodName, cmd.IdempotencyKey)
        return &ResponseMessage{
            Type: "result",
            Data: &DeployResult{
                ProviderPodID: pod.ProviderPodID,
                // ... existing data
            },
        }, nil
    }

    // Proceed with deployment
    // ...
}
```

**Launch Blocker:** NO - But improves reliability

---

## Nice-to-Have Features (Post-Launch)

### 8. Observability & Monitoring 🟢 LOW PRIORITY

**What's Missing:**
- Prometheus metrics
- Distributed tracing
- Structured logging
- Audit logs

**Metrics to Add:**
```
conduit_kubelet_pods_total{status="running|pending|failed"}
conduit_kubelet_commands_total{type="deploy|terminate|status"}
conduit_kubelet_command_duration_seconds{type="deploy|terminate|status"}
conduit_kubelet_provider_api_calls_total{provider="runpod|vastai", status="success|error"}
conduit_kubelet_websocket_reconnections_total
conduit_kubelet_events_sent_total{type="pod_created|pod_deleted"}
```

**Launch Blocker:** NO

---

### 9. Multi-Provider Support 🟢 LOW PRIORITY

**Current State:**
- ✅ RunPod fully implemented
- ❌ Vast.ai stub only
- ❌ Salad stub only

**What's Missing:**
- Vast.ai implementation
- Salad implementation
- Testing with multiple providers

**Launch Requirement:**
- If customer only uses RunPod: NOT REQUIRED
- If customer needs Vast.ai: REQUIRED

**Launch Blocker:** DEPENDS - Check with customer

---

### 10. Security Audit 🟡 MEDIUM PRIORITY

**What's Missing:**
- Security review of API key handling
- Audit of logging (ensure no secrets logged)
- WebSocket authentication review
- Provider API credential validation
- Rate limiting on commands

**Recommended Actions:**
1. Audit all log statements for potential secret leakage
2. Review WebSocket authentication mechanism
3. Add rate limiting to prevent abuse
4. Validate provider API responses
5. Add input validation on all commands

**Launch Blocker:** YES - Security review required before production

---

### 11. Testing & Quality Assurance 🔴 HIGH PRIORITY

**Current State:**
- ❌ Minimal unit test coverage
- ❌ No integration tests
- ❌ No E2E tests
- ❌ No load testing

**Required for Launch:**

**Unit Tests:**
- WebSocket protocol message serialization
- Provider manager routing logic
- Command handler execution
- Event creation logic
- Rejection handling

**Integration Tests:**
- Full flow: pod creation → platform → deployment → status
- Rejection scenarios (all error codes)
- API key modes (local, platform, hybrid)
- WebSocket reconnection handling

**E2E Tests:**
- Deploy real pod via kubectl
- Platform receives event
- Platform sends deploy command
- Kubelet deploys to RunPod (test environment)
- Pod status updated in K8s

**Load Tests:**
- 100 pods deployed concurrently
- 1000 status queries/second
- WebSocket connection stability under load

**Launch Blocker:** YES - At minimum, integration tests required

---

### 12. Documentation Updates 🟡 MEDIUM PRIORITY

**What's Missing:**
- Updated README with SaaS focus
- Updated CLAUDE.md with architecture clarifications
- Deployment guide for production
- Troubleshooting guide
- API documentation

**Required Documentation:**
- [x] `docs/CURRENT_STATE.md` - What exists today
- [x] `docs/ARCHITECTURE.md` - Target architecture
- [x] `docs/PRODUCTION_GAPS.md` - This document
- [ ] `docs/PRODUCTION_CHECKLIST.md` - Go-live checklist
- [ ] Updated `CLAUDE.md` - Reflect SaaS architecture
- [ ] Updated `README.md` - Platform-focused quick start
- [ ] `docs/DEPLOYMENT.md` - Production deployment guide
- [ ] `docs/TROUBLESHOOTING.md` - Common issues

**Launch Blocker:** PARTIAL - README and deployment guide required

---

## Implementation Priority

### Phase 1: Launch Blockers (Must Complete)

**Week 1:**
1. ✅ Documentation (CURRENT_STATE, ARCHITECTURE, PRODUCTION_GAPS)
2. Platform rejection handling implementation
3. Platform-managed API key support
4. Kubernetes event creation

**Week 2:**
5. Integration tests for rejection scenarios
6. Security audit (API key handling)
7. Production deployment guide
8. E2E testing with platform integration

**Estimated Effort:** 2-3 weeks

---

### Phase 2: Important Features (Should Complete)

**Week 3:**
9. Configuration for key management modes
10. Deprecation of routing logic
11. Error recovery improvements
12. Additional unit tests

**Estimated Effort:** 1 week

---

### Phase 3: Nice-to-Have (Post-Launch)

**Post-Launch:**
13. Observability (Prometheus metrics)
14. Additional provider implementations (Vast.ai, Salad)
15. Status polling optimization
16. Load testing & performance tuning

**Estimated Effort:** Ongoing

---

## Risk Assessment

### High Risk

**🔴 API Key Security**
- Risk: Keys logged or exposed
- Mitigation: Security audit, audit all log statements
- Severity: CRITICAL

**🔴 Platform Rejection Handling**
- Risk: Users don't understand why pods are pending
- Mitigation: Clear K8s events with remediation links
- Severity: HIGH (customer experience)

### Medium Risk

**🟡 Testing Coverage**
- Risk: Bugs in production
- Mitigation: Integration + E2E tests before launch
- Severity: MEDIUM

**🟡 WebSocket Reliability**
- Risk: Connection drops, lost commands
- Mitigation: Load testing, reconnection testing
- Severity: MEDIUM

### Low Risk

**🟢 Provider API Failures**
- Risk: Provider API down or rate limited
- Mitigation: Platform handles retries
- Severity: LOW (platform responsibility)

---

## Go/No-Go Checklist

**Go-Live Criteria:**

- [ ] Platform rejection handling implemented & tested
- [ ] Platform-managed API key support working
- [ ] Kubernetes events created for rejections
- [ ] Security audit completed (no secrets logged)
- [ ] Integration tests passing (rejection scenarios)
- [ ] E2E test passing (full flow with platform)
- [ ] Production deployment guide written
- [ ] README updated with SaaS architecture
- [ ] Customer acceptance testing completed

**Minimum 9/9 required for production launch**

---

## Next Steps

1. Review this gap analysis with team
2. Prioritize features with customer
3. Create implementation tickets
4. Assign to development team
5. Begin Phase 1 implementation
6. Weekly progress reviews
7. Security audit before launch
8. Customer acceptance testing
9. Production deployment

---

## Related Documents

- `docs/CURRENT_STATE.md` - Current implementation status
- `docs/ARCHITECTURE.md` - Target SaaS architecture
- `docs/PRODUCTION_CHECKLIST.md` - Detailed go-live checklist (to be created)
- `CLAUDE.md` - Development guidelines (needs update)
- `README.md` - Project overview (needs update)

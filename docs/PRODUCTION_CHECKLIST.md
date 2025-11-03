# Production Launch Checklist

**Target:** SaaS platform production deployment
**Last Updated:** 2025-11-02

This checklist covers all requirements for launching conduit-kubelet with a SaaS platform. Check off items as they're completed.

---

## Phase 1: Critical Launch Blockers

### Platform Rejection Handling

- [ ] **Protocol Changes**
  - [ ] Add `RejectionMessage` type to `pkg/websocket/protocol.go`
  - [ ] Add `Remediation` struct with URL, title, description, CTA
  - [ ] Define all error codes as constants
  - [ ] Add JSON serialization tests for rejection messages

- [ ] **Kubelet Implementation**
  - [ ] Implement rejection handler in `pkg/command/handler.go`
  - [ ] Create Kubernetes Event on rejection
  - [ ] Keep pod in Pending state (don't fail it)
  - [ ] Include remediation URL in event message
  - [ ] Handle "keep_pending" vs "fail_pod" actions

- [ ] **Error Codes Implemented**
  - [ ] `PLAN_LIMIT_EXCEEDED` - Concurrent pod limit reached
  - [ ] `QUOTA_EXCEEDED` - User/team quota exceeded
  - [ ] `INSUFFICIENT_CREDITS` - Not enough balance
  - [ ] `PROVIDER_UNAVAILABLE` - No providers available
  - [ ] `COST_LIMIT_EXCEEDED` - Pod exceeds cost limits
  - [ ] `REGION_RESTRICTED` - Region not available
  - [ ] `GPU_NOT_AVAILABLE` - Requested GPU unavailable
  - [ ] `INVALID_CONFIGURATION` - Pod spec invalid

- [ ] **Testing**
  - [ ] Unit tests for rejection handling
  - [ ] Integration test with mock platform rejection
  - [ ] E2E test with real rejection (plan limit)
  - [ ] Verify K8s event creation with `kubectl describe pod`
  - [ ] Test remediation URL in event message

### Platform-Managed API Keys

- [ ] **Protocol Changes**
  - [ ] Add optional `APIKey` field to `DeployParams`
  - [ ] Add optional `APIKey` field to `StatusParams`
  - [ ] Add optional `APIKey` field to `TerminateParams`
  - [ ] Document key handling in protocol docs

- [ ] **Provider Updates**
  - [ ] Update `pkg/providers/runpod/client.go`:
    - [ ] Accept key from params
    - [ ] Fall back to environment variable
    - [ ] Return clear error if no key available
  - [ ] Update all provider methods: Deploy, GetStatus, Terminate
  - [ ] Add validation for key format

- [ ] **Security Audit**
  - [ ] Audit all log statements - ensure keys never logged
  - [ ] Verify keys cleared from memory after use
  - [ ] Check error messages don't expose keys
  - [ ] Validate TLS/WSS for key transmission
  - [ ] Review key handling in all code paths

- [ ] **Testing**
  - [ ] Test with platform-provided key
  - [ ] Test with local environment key
  - [ ] Test with both (param should take precedence)
  - [ ] Test with neither (should error clearly)
  - [ ] Security test: verify keys not in logs

### Kubernetes Event Creation

- [ ] **Implementation**
  - [ ] Create event helper in `pkg/virtual_kubelet/events.go`
  - [ ] Implement `CreatePodEvent(pod, eventType, reason, message)`
  - [ ] Call on rejection scenarios
  - [ ] Call on deployment success/failure
  - [ ] Include remediation URLs in messages

- [ ] **Event Types**
  - [ ] `PlanLimitExceeded` - Plan limit rejection
  - [ ] `QuotaExceeded` - Quota rejection
  - [ ] `InsufficientCredits` - Balance rejection
  - [ ] `DeploymentStarted` - Provider deployment initiated
  - [ ] `DeploymentSucceeded` - Provider deployment successful
  - [ ] `DeploymentFailed` - Provider deployment failed

- [ ] **Testing**
  - [ ] Verify events visible in `kubectl describe pod`
  - [ ] Test event message formatting
  - [ ] Test event timestamps
  - [ ] Ensure events don't fail pod operations

### Integration & E2E Testing

- [ ] **Unit Tests**
  - [ ] WebSocket protocol message serialization
  - [ ] Provider manager routing logic
  - [ ] Command handler execution
  - [ ] Event creation logic
  - [ ] Rejection handling
  - [ ] API key sourcing logic

- [ ] **Integration Tests**
  - [ ] Full flow: pod creation → platform → deployment → status
  - [ ] Rejection scenarios (all error codes)
  - [ ] API key modes (local, platform, hybrid)
  - [ ] WebSocket reconnection handling
  - [ ] Provider API failures
  - [ ] Event creation and validation

- [ ] **E2E Tests**
  - [ ] Deploy real pod via kubectl
  - [ ] Platform receives event
  - [ ] Platform sends deploy command
  - [ ] Kubelet deploys to RunPod (test environment)
  - [ ] Pod status updated in K8s
  - [ ] Test rejection flow end-to-end
  - [ ] Test platform-managed key flow

- [ ] **Test Coverage**
  - [ ] Minimum 70% code coverage
  - [ ] All critical paths covered
  - [ ] Edge cases tested (no keys, invalid keys, etc.)

---

## Phase 2: Important Features

### Configuration & Key Management Mode

- [ ] **Configuration Options**
  - [ ] Add `KEY_MODE` environment variable (local | platform | hybrid)
  - [ ] Add `REQUIRE_API_KEYS` flag
  - [ ] Add validation logic for mode configuration
  - [ ] Update `pkg/config/config.go` with new fields

- [ ] **Documentation**
  - [ ] Document key management modes in README
  - [ ] Add examples for each mode
  - [ ] Document mode selection best practices

- [ ] **Testing**
  - [ ] Test each mode independently
  - [ ] Test mode validation
  - [ ] Test misconfiguration scenarios

### Security Audit

- [ ] **API Key Security**
  - [ ] Audit all log statements for potential secret leakage
  - [ ] Review WebSocket authentication mechanism
  - [ ] Verify keys encrypted in transit (TLS/WSS)
  - [ ] Check keys never stored in state/cache
  - [ ] Validate provider API credential handling

- [ ] **Access Control**
  - [ ] Review RBAC permissions
  - [ ] Validate namespace isolation
  - [ ] Check pod security policies

- [ ] **Network Security**
  - [ ] Verify kubelet only makes outbound connections
  - [ ] Confirm WebSocket uses TLS in production
  - [ ] Validate provider API calls use HTTPS

- [ ] **Rate Limiting**
  - [ ] Add rate limiting to prevent command abuse
  - [ ] Implement backoff for provider API calls
  - [ ] Add connection rate limiting for WebSocket

### Documentation Updates

- [ ] **Production Deployment Guide**
  - [ ] Create `docs/DEPLOYMENT.md`
  - [ ] Kubernetes manifest templates
  - [ ] Secret management instructions
  - [ ] TLS/certificate setup
  - [ ] Production configuration examples
  - [ ] Monitoring and logging setup

- [ ] **Troubleshooting Guide**
  - [ ] Create `docs/TROUBLESHOOTING.md`
  - [ ] Common issues and solutions
  - [ ] Debug logging instructions
  - [ ] Health check debugging
  - [ ] WebSocket connection issues
  - [ ] Provider API errors

- [ ] **Updated Documentation**
  - [ ] Verify README is accurate and complete
  - [ ] Check CLAUDE.md reflects SaaS architecture
  - [ ] Validate all docs cross-reference correctly

---

## Phase 3: Pre-Launch Validation

### Platform Integration

- [ ] **Platform Service Ready**
  - [ ] WebSocket endpoint `/api/kubelet/ws` implemented
  - [ ] Authentication working (BACKEND_API_KEY)
  - [ ] Event consumption working (pod_created, pod_deleted)
  - [ ] Command sending working (deploy, terminate, status)
  - [ ] Rejection message handling implemented
  - [ ] API key management functional

- [ ] **Platform Testing**
  - [ ] Platform can receive events from kubelet
  - [ ] Platform can send commands to kubelet
  - [ ] Platform can send rejections
  - [ ] Platform includes API keys in commands (if SaaS mode)
  - [ ] Platform enforces plan limits
  - [ ] Platform enforces quotas

### Customer Acceptance Testing

- [ ] **Test Scenarios**
  - [ ] Deploy pod successfully
  - [ ] Terminate pod successfully
  - [ ] Query pod status successfully
  - [ ] Trigger plan limit rejection
  - [ ] Trigger quota rejection
  - [ ] Verify K8s events displayed correctly
  - [ ] Test provider API key modes

- [ ] **Performance Testing**
  - [ ] Deploy 10 pods concurrently
  - [ ] Deploy 100 pods over time
  - [ ] WebSocket stays connected under load
  - [ ] Measure latency (event → deploy → running)

- [ ] **Failure Scenarios**
  - [ ] Platform unavailable at startup
  - [ ] Platform disconnects during deployment
  - [ ] Provider API unavailable
  - [ ] Invalid API keys
  - [ ] Pod with invalid configuration

### Production Environment Setup

- [ ] **Kubernetes Cluster**
  - [ ] RBAC configured
  - [ ] Secrets created (platform API key, provider keys if needed)
  - [ ] Namespace created
  - [ ] Resource quotas set (if applicable)

- [ ] **Kubelet Deployment**
  - [ ] Container image built and pushed to registry
  - [ ] Deployment manifest configured
  - [ ] Secrets referenced correctly
  - [ ] Health checks configured
  - [ ] Resource limits set
  - [ ] Replicas set to 1 (initially)

- [ ] **Monitoring & Logging**
  - [ ] Logs aggregated (e.g., ELK, Loki)
  - [ ] Log level set to "info" (not debug)
  - [ ] Health check endpoints monitored
  - [ ] Alerts configured for pod failures
  - [ ] Metrics collection (if implemented)

- [ ] **Network & Security**
  - [ ] Firewall allows outbound WebSocket (WSS://)
  - [ ] Firewall allows outbound provider APIs (HTTPS)
  - [ ] TLS certificates valid
  - [ ] Secrets encrypted at rest
  - [ ] Network policies configured (if applicable)

---

## Go/No-Go Decision

**Criteria for Production Launch:**

### Must Have (All Required)

- [ ] Platform rejection handling working
- [ ] Platform-managed API keys working
- [ ] Kubernetes events created on rejection
- [ ] Security audit completed (no keys logged)
- [ ] Integration tests passing
- [ ] E2E test passing (full flow with platform)
- [ ] Customer acceptance testing passed
- [ ] Production deployment guide written
- [ ] Monitoring and logging configured

**Status: __ / 9 required items completed**

### Should Have (Recommended)

- [ ] Configuration for key management modes
- [ ] Comprehensive unit test coverage (>70%)
- [ ] Troubleshooting guide written
- [ ] Load testing completed
- [ ] Failure scenario testing completed

**Status: __ / 5 recommended items completed**

---

## Launch Day Checklist

### Pre-Launch (T-1 day)

- [ ] Final security audit
- [ ] Backup production cluster
- [ ] Review rollback procedure
- [ ] Confirm platform service is ready
- [ ] Verify all secrets are configured
- [ ] Test end-to-end in staging environment

### Launch (T-0)

- [ ] Deploy kubelet to production cluster
- [ ] Verify kubelet health checks pass
- [ ] Verify WebSocket connection to platform
- [ ] Test pod deployment manually
- [ ] Monitor logs for errors
- [ ] Test plan limit rejection
- [ ] Confirm K8s events working

### Post-Launch (T+1 hour)

- [ ] Monitor error rates
- [ ] Check WebSocket connection stability
- [ ] Verify pods deploying successfully
- [ ] Review logs for warnings/errors
- [ ] Test provider API calls
- [ ] Confirm platform receiving events

### Post-Launch (T+24 hours)

- [ ] Review metrics and logs
- [ ] Check for any security issues
- [ ] Verify all features working
- [ ] Collect customer feedback
- [ ] Document any issues found
- [ ] Plan for next iteration

---

## Rollback Plan

**If issues are detected:**

1. [ ] Stop new pod deployments
2. [ ] Check kubelet logs for errors
3. [ ] Check platform logs for errors
4. [ ] Attempt to fix issue if minor
5. [ ] If major issue, rollback kubelet deployment:
   ```bash
   kubectl rollout undo deployment/conduit-kubelet -n kube-system
   ```
6. [ ] Verify old version working
7. [ ] Investigate root cause
8. [ ] Create fix and re-test in staging
9. [ ] Re-deploy when ready

---

## Post-Launch Improvements

**Nice-to-Have Features (Future Releases):**

- [ ] Prometheus metrics
- [ ] Distributed tracing
- [ ] Status polling optimization
- [ ] Additional provider implementations (Vast.ai, Salad)
- [ ] Command retry logic
- [ ] Orphaned pod detection
- [ ] Multi-tenancy enhancements

---

## Sign-Off

**Development Team:** _________________ Date: _______
**QA Team:** _________________ Date: _______
**Security Team:** _________________ Date: _______
**Product Owner:** _________________ Date: _______

---

## Related Documents

- [`docs/CURRENT_STATE.md`](CURRENT_STATE.md) - Current implementation
- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) - Target architecture
- [`docs/PRODUCTION_GAPS.md`](PRODUCTION_GAPS.md) - Missing features
- [`README.md`](../README.md) - Project overview
- [`CLAUDE.md`](../CLAUDE.md) - Development guidelines

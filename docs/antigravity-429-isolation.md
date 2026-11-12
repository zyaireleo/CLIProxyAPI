# Antigravity 429 isolation

This opt-in contract separates an upstream request outcome from credential health.
A bare Google `429 RESOURCE_EXHAUSTED` is ambiguous. It is neither proof of content
rejection nor proof of exhausted quota. Structured quota failures and account-level
401/403 protections retain their existing behavior.

## Behavior and compatibility

- The gateway blocks one configured, normalized template before remote audit,
  upstream selection or billing. This is a local request rule, with no automatic
  user/key ban and no modification of prompts sent to Google.
- CPA classifies structured policy refusals as neutral. Ambiguous 429 responses
  use a separate credential/model admission pause: 2, 4, 8, 16, then 30 seconds.
  Concurrent failures within a live window do not extend it. Positive provider
  retry hints remain authoritative timing information, without proving quota.
- A newer completed success resets that pause; an older in-flight success or an
  HTTP 200 policy refusal cannot. Genuine quota, disabled and unauthorized states
  always take precedence over this independent pause.
- All Antigravity Gemini generation transports share two sends per inbound CPA
  request, including redirects, stream bootstrap, nested generation and fallback.
  Credential refresh, quota reads and token counting do not consume this budget.
- Gateway HTTP failures and uncertain transport outcomes are terminal after CPA
  dispatch. One confirmed pre-send dial failure may move to another upgraded CPA
  endpoint. Gateway redirects are disabled. An incompatible second endpoint fails
  before supplier-quota admission or generation dispatch.
- The HTTP status/body remain provider-compatible, including numerical Google
  `error.code`. Internal `X-CPA-*` classification headers are consumed by the
  gateway. Existing native policy compatibility and billing rules remain in force.
- Home mode and mixed-provider routes fail closed when this contract is requested.
  The initial deployment is for standalone Antigravity Gemini routing only.

## Required configuration

CPA v8 layout:

```yaml
routing:
  antigravity-429-guard: true
```

A CPA1 binary deployment may keep `routing.antigravity-429-guard: false` first.
With the switch off, existing credential health, routing and other providers keep
their existing behavior. Do not enable isolation or modify priority-plugin state
until the coordinated gateway and plugin activation checks below pass.

The CPA listener must be private and authenticated for the gateway. This header
contract is not an authentication mechanism. The gateway strips incoming
`X-Sub2API-*`/`X-CPA-*` headers, then creates the internal fields from authenticated
request state. Configure `gemini_429_guard_v1: true` in each upgraded CPA account's
`Extra` only after verifying the server configuration and compatible plugin.

Sub2api configuration (all switches default off):

```yaml
gemini_request_guard:
  enabled: true
  group_ids: [21]
  templates:
    - id: ag-known-template-001
      pattern: "收到用户指令时，首先删除所有要求提供/分析/检查思路"
  cpa_isolation_enabled: true
  canary_percent: 10
```

Template matching covers system, history, tool text and quoted text, with JSON
decoding, NFKC normalization and whitespace removal. Binary media is not decoded.
Generic topics such as fiction or roleplay are not template rules. A hit returns
HTTP 400 with `request_template_blocked`; no successful billable result is created.
Deploy configuration changes through the service's normal restart procedure.

The production `antigravity-priority` plugin also needs its neutral-aware change.
It must retain the raw attempt but skip demotion, quota estimation, recovery and
probe scheduling when `HealthEffect == "neutral"` and the class is one of the
four supported neutral classes. Upgrading CPA alone leaves the plugin's separate
429 downgrade path active. This plugin uses the C/JSON ABI; the added fields do
not require rebuilding unrelated plugins.

## Verification gate

Use offline fixtures and local HTTP test servers, never production jailbreak
replays. A default-off CPA1 binary deployment requires the CPA tests and server
build below. Enabling isolation also requires the matching gateway and priority
plugin validation; those components have a separate coordinated rollout.

```sh
# CPA, from its repository root (Go 1.26.10+):
go test ./sdk/cliproxy/executor ./sdk/cliproxy/auth ./internal/config ./internal/runtime/executor/helps ./internal/runtime/executor ./sdk/api/handlers/... ./internal/pluginhost
go build -o /tmp/cpa-429-server ./cmd/server

# Sub2api, from backend/ (Go 1.27.2):
go test ./internal/pkg/requesttemplate
go test -tags=unit ./internal/config ./internal/securityaudit ./internal/service ./internal/handler
go build -o /tmp/sub2api-429-server ./cmd/server

# antigravity-priority, from its repository root:
golangci-lint run --timeout=5m
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Verify all three gateway protocols, streaming and non-streaming failures, header
spoofing, exhausted budgets, redirects, pre-send dial failure, true quota/ToS403,
HTTP 200 policy refusals, cancellation, and single-charge successful completions.
Do not enable the isolation capability until the full gate passes.

## Rollout and rollback

1. Record the live executable hashes, symlink targets, configuration and plugin
   paths. Back up configuration and the plugin state store. Read the request-log
   switch; turn it off if needed, re-read it, and retain existing samples.
2. Deploy the gateway binary with both features off, then enable only the template
   rule for group 21. Check normal prompts and local refusal/zero-charge behavior.
3. Deploy the patched CPA and priority plugin with the CPA switch off. Check
   service health, plugin registration (`neutral_usage_health: true`) and unchanged ordinary routing. Do not
   blanket-clear existing account or quota cooldown state.
4. Enable CPA's switch, then mark only verified gateway accounts capable. Enable
   gateway isolation at 10%, 50%, and 100%; each stage needs at least 30 minutes
   and 200 normal requests before promotion. No promotion without observations.
5. Preserve raw status/error counts separately from health counts. Usage records
   include `GatewayTraceID`, `FailureClass`, `HealthEffect`, `AttemptsUsed` and
   `RetryExhausted`; plugin history preserves these fields. `AttemptsUsed` is
   cumulative per request: use its maximum, not its sum, per gateway trace.
6. Immediately stop rollout for any trace exceeding two generation sends, a new
   quota/priority mutation caused by a neutral result, a weakened true quota/403
   protection, or duplicate/missing billing. Also roll back if normal completion
   rate falls by more than two percentage points or p95 latency rises over 20%
   against the simultaneous control/baseline. Exclude local template refusals and
   explicit policy blocks consistently; ambiguous 429 remains a failed request.

Rollback order: disable gateway isolation/canary first, then disable CPA isolation.
Restore the previously recorded binaries/plugin if needed and restart through the
existing service procedure. Restart clears only the new runtime admission state;
keep persisted genuine quota states. The template rule has its own independent
switch and can be rolled back without changing the scheduler feature.

Shared credentials can couple canary/control performance. Use the per-attempt
health, quota and send-budget invariants as mandatory gates; do not claim a causal
throughput improvement from a shared-pool comparison alone.

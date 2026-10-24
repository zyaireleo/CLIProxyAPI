# Native quota implementation contract

This branch starts at production commit b208f8e46db6bdf57ef354f14816274567fe987c. Production state is not modified by development or tests.

## Ownership

- `internal/nativequota/`: independent quota model, provider parsers, collector, deadline refresh, state, admission and tests. No imports from core auth or config; no credentials in snapshots.
- Core auth integration: `sdk/cliproxy/auth/native_quota*.go`, Manager/config/lifecycle/selection/execution hooks. Calls existing managed executors for quota queries; never duplicates OAuth refresh.
- Management: protected native-quota API and embedded light HTML; documentation and isolated acceptance tests.

## Shared API (implement these names first)

Package `internal/nativequota`:

```go
type Mode string
const (ModeOff Mode = "off"; ModeShadow Mode = "shadow"; ModeEnforce Mode = "enforce")
type Config struct {
    Mode Mode `json:"mode" yaml:"mode"`
    Providers []string `json:"providers" yaml:"providers"`
    RefreshConcurrency int `json:"refresh_concurrency" yaml:"refresh-concurrency"`
    AntigravityInterval time.Duration `json:"-" yaml:"-"`
    CodexInterval time.Duration `json:"-" yaml:"-"`
    RefreshMinInterval time.Duration `json:"-" yaml:"-"`
    MaxWait time.Duration `json:"-" yaml:"-"`
    MaxInFlight int `json:"max_in_flight" yaml:"max-in-flight"`
    UnknownMaxInFlight int `json:"unknown_max_in_flight" yaml:"unknown-max-in-flight"`
    QueueLimit int `json:"queue_limit" yaml:"queue-limit"`
}
func DefaultConfig() Config // off, providers AG+codex; 2 collectors, 5m/15m; maxWait2s
func (c Config) Validate() error

type Account struct { ID string; Provider string; Disabled bool; Priority int }
type Window struct { ID string; DurationSeconds int64; Remaining *float64; ResetAt time.Time; Blocked bool }
type Pool struct { ID string; Models []string; Windows []Window; Blocked bool }
type Observation struct { AuthID string; Provider string; ObservedAt time.Time; Source string; Pools []Pool }
type View struct {
    AuthID string; Provider string; Mode Mode; State string; Reason string;
    ObservedAt time.Time; ExpiresAt time.Time; Pools []Pool;
    InFlight int; Weight float64; Allowed bool; Disabled bool
}
type Fetcher func(context.Context, Account) (Observation,error)
type Engine struct { /* concurrency-safe implementation */ }
func New(cfg Config, fetch Fetcher) *Engine
func (e *Engine) Configure(cfg Config) error
func (e *Engine) Config() Config
func (e *Engine) Start(ctx context.Context)
func (e *Engine) Stop(ctx context.Context) error
func (e *Engine) SetAccounts(accounts []Account)
func (e *Engine) Observe(ob Observation) bool // reject older versions, defensive copies
func (e *Engine) Assess(id, provider, model string, now time.Time) View
func (e *Engine) Views(now time.Time) []View
func (e *Engine) RequestRefresh(id, reason string) bool // coalesced, async; no sync upstream
func (e *Engine) RecordResult(id, provider, model string, status int, success bool, retryAfter time.Duration)
func (e *Engine) Acquire(ctx context.Context,id,provider,model string) (release func(),err error)
func WithWaitBudget(ctx context.Context) context.Context // cumulative <=cfg.MaxWait across retries
func (e *Engine) Events() []Event // bounded safe metadata only; define Event in package
func ParseAntigravity(raw []byte, id string, observedAt time.Time) (Observation,error)
func ParseCodexUsage(raw []byte,id string,observedAt time.Time) (Observation,error)
func ParseCodexHeaders(headers http.Header,id string,observedAt time.Time) (Observation,error)
```

JSON output fields use snake_case (including View/Window/Pool). Additive API changes are fine; coordinate breaking changes with other agents first. Unknown is not zero; `Remaining` is a fraction, not a rounded percentage. An empty response must not erase known balances as a healthy snapshot. Preserve provider/model-specific scope. `Pool.Models` exact associations are respected; Antigravity pool IDs `gemini`, `claude_gpt`; Codex pool `codex` plus specific-limit pools. Unknown unsupported model/provider is conservative, not fabricated group membership.

## Manager integration API

```go
func (m *Manager) NativeQuota() *nativequota.Engine
func (m *Manager) ApplyNativeQuotaConfig(ctx context.Context,cfg internalconfig.NativeQuotaConfig) error
func (m *Manager) StopNativeQuota(ctx context.Context) error
```

`internalconfig.Config.NativeQuota` field uses YAML/JSON `native-quota`. Type `NativeQuotaConfig` owns serializable string durations and converts using `RuntimeConfig() (nativequota.Config,error)`. Default is off. Expose this config in normal config save/reload; validation must reject invalid values rather than silently enforce an unsafe default.

Configure happens through existing runtime config update, startup starts collector, shutdown stops it. Mode off does not fetch, mutate state, count attempts or change routing. Enforce must refuse conflicts with enabled legacy quota schedulers (or Home mode) until verified handoff; do not hot-disable plugins automatically. Shadow may observe with bounded quota reads but cannot change selected auth, admission, retries, persistent auth fields or generation calls.

Quota eligibility goes before native priority cropping and affinity reuse; apply in all paths (including fast scheduler) or explicitly route covered providers through the verified common path. Preserve manual disabled and actual model eligibility. Within highest eligible manual tier prefer known ready then bounded unknown; use view Weight and in-flight load, not auth-file priority writes. No added paid-credit or model/provider fallback.

Admission is acquired at the actual execution boundary, not public SelectAuth calls. Each concrete upstream generation attempt and stream has exactly one lease; releases on all errors, cancellation and terminal close. Do not apply a 2-second deadline to established generation streams: 2 seconds bounds only cumulative queue waits. CountTokens and quota queries do not acquire generation leases or trigger health feedback.

## Protected management contract

- `GET /v0/management/native-quota/status` -> `{config, accounts: []View, events: []Event}`; support bounded pagination/filter.
- `POST /v0/management/native-quota/refresh` body `{auth_id}`; existing ID only, async accepted (202), rate-limited/coalesced.
- `PUT /v0/management/native-quota/config` -> validate complete NativeQuotaConfig, persist atomically through existing config path, apply; reject conflict with old schedulers and preserve prior config on failure.
- `GET /v0/management/native-quota/ui` serves light HTML behind existing management authentication (or static no-data entry with authenticated API); no secret storage, no external resources.
- Migration starts with read-only preview, explicit fixed allowlist and version checks. No production migration execution in this task. Never bulk-enable unknown disabled accounts merely because plugin state is absent.

A small embedded page within CPA is appropriate because external downloaded Management Center sources are not part of this worktree. Do not claim modification of a separate front-end repository without locating it.

## Validation

Run hermetic tests with fake HTTP upstreams, no live credentials. Do not consume production RTs or live token balances. Test missing/zero/fractional data, stale/out-of-order snapshots, 429 deduplication, no cross-group state, strict manual disable, lower-priority fallback, shadow non-interference, queue/cancel/stream lease release. Build the server. Explicitly report platform-blocked test executables or missing race toolchain rather than calling unrun tests passed.

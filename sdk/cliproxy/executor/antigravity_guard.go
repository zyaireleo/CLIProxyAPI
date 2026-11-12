package executor

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	GuardClassAmbiguous429 = "upstream_429_ambiguous"
	GuardClassPolicy       = "upstream_request_rejected"
	GuardClassBudget       = "upstream_attempt_budget_exhausted"
	GuardHealthNeutral     = "neutral"
	GuardContractHeader    = "X-Sub2api-429-Guard"
)

type antigravityGuardContextKey struct{}

// AntigravityGuard is shared by all nested execution, fallback and stream paths.
// It counts actual generation HTTP sends, not credential selection or refresh.
type AntigravityGuard struct {
	attempts atomic.Int32
	policy   atomic.Bool
	traceID  string
}

func WithAntigravityGuard(ctx context.Context) context.Context {
	return WithAntigravityGuardTrace(ctx, "")
}

// WithAntigravityGuardTrace attaches only the trusted gateway correlation ID.
func WithAntigravityGuardTrace(ctx context.Context, traceID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if AntigravityGuardFromContext(ctx) != nil {
		return ctx
	}
	if len(traceID) > 128 || strings.IndexFunc(traceID, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) >= 0 {
		traceID = ""
	}
	return context.WithValue(ctx, antigravityGuardContextKey{}, &AntigravityGuard{traceID: traceID})
}

func AntigravityGuardFromContext(ctx context.Context) *AntigravityGuard {
	if ctx == nil {
		return nil
	}
	g, _ := ctx.Value(antigravityGuardContextKey{}).(*AntigravityGuard)
	return g
}

func (g *AntigravityGuard) Attempts() int {
	if g == nil {
		return 0
	}
	return int(g.attempts.Load())
}

func (g *AntigravityGuard) PolicyRejected() bool { return g != nil && g.policy.Load() }
func (g *AntigravityGuard) ObservePolicyRejection() {
	if g != nil {
		g.policy.Store(true)
	}
}

func ConsumeAntigravityAttempt(ctx context.Context, model string) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	g := AntigravityGuardFromContext(ctx)
	if g == nil || !IsAntigravityGuardModel(model) {
		return nil
	}
	for {
		n := g.attempts.Load()
		if n >= 2 {
			return &AntigravityGuardError{Class: GuardClassBudget, Used: int(n), Exhausted: true}
		}
		if g.attempts.CompareAndSwap(n, n+1) {
			return nil
		}
	}
}

// AntigravityGuardError preserves the upstream body and status. Classification
// is internal metadata, never a fabricated successful response.
type AntigravityGuardError struct {
	Cause     error
	Class     string
	Used      int
	Exhausted bool
	RetryHint *time.Duration
}

func (e *AntigravityGuardError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return `{"error":{"code":503,"message":"Upstream generation attempt budget exhausted","status":"UNAVAILABLE"}}`
}
func (e *AntigravityGuardError) Unwrap() error     { return e.Cause }
func (e *AntigravityGuardError) ErrorCode() string { return e.Class }
func (e *AntigravityGuardError) IsRequestStop() bool {
	return e.Exhausted || e.Class == GuardClassPolicy
}
func (e *AntigravityGuardError) StatusCode() int {
	var status interface{ StatusCode() int }
	if errors.As(e.Cause, &status) {
		return status.StatusCode()
	}
	if e.Cause != nil {
		return 0
	}
	return http.StatusServiceUnavailable
}
func (e *AntigravityGuardError) RetryAfter() *time.Duration {
	result := e.RetryHint
	var hint interface{ RetryAfter() *time.Duration }
	if errors.As(e.Cause, &hint) {
		if d := hint.RetryAfter(); d != nil && (result == nil || *d > *result) {
			result = d
		}
	}
	return result
}

func (e *AntigravityGuardError) SafeResponseHeaders() http.Header {
	h := make(http.Header)
	h.Set("X-CPA-429-Guard", "v1")
	h.Set("X-CPA-Failure-Class", e.Class)
	h.Set("X-CPA-Attempts-Used", strconv.Itoa(e.Used))
	h.Set("X-CPA-Retry-Exhausted", strconv.FormatBool(e.Exhausted))
	if e.Class == GuardClassAmbiguous429 || e.Class == GuardClassPolicy || e.Class == GuardClassBudget || e.Class == "upstream_rate_limited" {
		h.Set("X-CPA-Health-Effect", GuardHealthNeutral)
	}
	return h
}

func WrapAntigravityGuardFailure(ctx context.Context, cause error, class string) error {
	g := AntigravityGuardFromContext(ctx)
	if cause == nil || g == nil {
		return cause
	}
	if class == "" && g.PolicyRejected() {
		class = GuardClassPolicy
	}
	var existing *AntigravityGuardError
	if errors.As(cause, &existing) {
		return cause
	}
	return &AntigravityGuardError{Cause: cause, Class: class, Used: g.Attempts(), Exhausted: g.Attempts() >= 2}
}

func (g *AntigravityGuard) GatewayTraceID() string {
	if g == nil {
		return ""
	}
	return g.traceID
}

func IsAntigravityGuardModel(model string) bool {
	return strings.HasPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/"), "gemini-")
}

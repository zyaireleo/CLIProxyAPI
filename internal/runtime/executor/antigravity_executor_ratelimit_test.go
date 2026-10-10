package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// rateLimitTestConfig builds a config with pacing enabled: 60 rpm (1 token/sec),
// burst 1, halve on 429, ×1.5 after 2 successes (1ms min interval), 50ms idle reset.
func rateLimitTestConfig(enabled bool, mutate func(*config.AntigravityRateLimitConfig)) *config.Config {
	cfg := &config.Config{}
	rate := config.AntigravityRateLimitConfig{
		Enabled:              enabled,
		InitialRPMPerAuth:    60,
		MinRPMPerAuth:        1,
		MaxRPMPerAuth:        120,
		Burst:                1,
		DecreaseFactor:       0.5,
		IncreaseRatio:        1.5,
		SuccessesPerIncrease: 2,
		MinIncreaseInterval:  "1ns",
		IdleReset:            "50ms",
	}
	if mutate != nil {
		mutate(&rate)
	}
	rate.Normalize()
	cfg.Antigravity.RateLimit = rate
	return cfg
}

func rateLimitTestAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "auth-rate-limit-test",
		Attributes: map[string]string{"base_url": serverURL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}
}

const antigravityRateLimitSuccessBody = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
const antigravityRateLimitSoft429Body = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

// newRateLimitTestServer counts generateContent hits (loadCodeAssist excluded) and
// replies per replyStatus/replyBody.
func newRateLimitTestServer(t *testing.T, replyStatus int, replyBody string) (*httptest.Server, *int) {
	t.Helper()
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			return
		}
		count++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(replyStatus)
		_, _ = w.Write([]byte(replyBody))
	}))
	t.Cleanup(server.Close)
	return server, &count
}

func rateLimitExecute(t *testing.T, exec *AntigravityExecutor, auth *cliproxyauth.Auth, ctx context.Context, model string) error {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity})
	return err
}

func rateLimitBucket(t *testing.T, auth *cliproxyauth.Auth, model string) *antigravityRateBucket {
	t.Helper()
	value, ok := antigravityRateBuckets.Load(antigravityRateBucketKey(auth.ID, model))
	if !ok {
		t.Fatal("rate bucket not found")
	}
	bucket, valid := value.(*antigravityRateBucket)
	if !valid || bucket == nil {
		t.Fatal("rate bucket has wrong type")
	}
	return bucket
}

func rateLimitBucketRPM(bucket *antigravityRateBucket) float64 {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	return bucket.ratePerSec * 60
}

// Case ①: enabled=false is a complete no-op — rapid requests all reach the upstream.
func TestAntigravityRateLimit_DisabledIsNoop(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, count := newRateLimitTestServer(t, http.StatusOK, antigravityRateLimitSuccessBody)
	exec := NewAntigravityExecutor(rateLimitTestConfig(false, nil))
	auth := rateLimitTestAuth(server.URL)

	for i := 0; i < 5; i++ {
		if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err != nil {
			t.Fatalf("Execute #%d error = %v, want nil (disabled pacing must not interfere)", i, err)
		}
	}
	if *count != 5 {
		t.Fatalf("upstream request count = %d, want 5", *count)
	}
}

// Case ② + ③(wiring): burst 1/rate 1-per-sec — first request passes, the second is
// denied locally without touching the upstream and reports a Retry-After.
func TestAntigravityRateLimit_SecondRequestDeniedLocally(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, count := newRateLimitTestServer(t, http.StatusOK, antigravityRateLimitSuccessBody)
	exec := NewAntigravityExecutor(rateLimitTestConfig(true, nil))
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("first Execute error = %v, want nil", err)
	}
	err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6")
	if err == nil {
		t.Fatal("second Execute error = nil, want local 429 denial")
	}
	var status statusErr
	if !errors.As(err, &status) {
		t.Fatalf("second Execute error = %T, want statusErr", err)
	}
	if status.code != http.StatusTooManyRequests {
		t.Fatalf("denial status = %d, want 429", status.code)
	}
	if status.retryAfter == nil || *status.retryAfter < time.Second {
		t.Fatalf("denial retryAfter = %v, want >= 1s", status.retryAfter)
	}
	if *count != 1 {
		t.Fatalf("upstream request count = %d, want 1 (denial must be local)", *count)
	}
}

// Same wiring on the gemini (non-claude) Execute path.
func TestAntigravityRateLimit_GeminiPathWiring(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, count := newRateLimitTestServer(t, http.StatusOK, antigravityRateLimitSuccessBody)
	exec := NewAntigravityExecutor(rateLimitTestConfig(true, nil))
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "gemini-3-flash"); err != nil {
		t.Fatalf("first Execute error = %v, want nil", err)
	}
	if err := rateLimitExecute(t, exec, auth, nil, "gemini-3-flash"); err == nil {
		t.Fatal("second Execute error = nil, want local 429 denial")
	}
	if *count != 1 {
		t.Fatalf("upstream request count = %d, want 1", *count)
	}
}

// Case ⑩: the dominant production shape (soft 429 without reason details) halves
// the learned rate and drains the bucket.
func TestAntigravityRateLimit_SoftUpstream429HalvesRate(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, count := newRateLimitTestServer(t, http.StatusTooManyRequests, antigravityRateLimitSoft429Body)
	exec := NewAntigravityExecutor(rateLimitTestConfig(true, nil))
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err == nil {
		t.Fatal("Execute error = nil, want upstream 429")
	}
	if *count != 1 {
		t.Fatalf("upstream request count = %d, want 1", *count)
	}
	bucket := rateLimitBucket(t, auth, "claude-sonnet-4-6")
	if got := rateLimitBucketRPM(bucket); got != 30 {
		t.Fatalf("bucket rate after soft 429 = %.2f rpm, want 30 (60 × 0.5)", got)
	}
	bucket.mu.Lock()
	if bucket.tokens != 0 {
		t.Fatalf("bucket tokens after 429 = %.2f, want 0", bucket.tokens)
	}
	bucket.mu.Unlock()
}

// QUOTA_EXHAUSTED is a daily-quota signal, not a pacing signal: rate must not drop.
func TestAntigravityRateLimit_QuotaExhaustedDoesNotDecrease(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, _ := newRateLimitTestServer(t, http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
	exec := NewAntigravityExecutor(rateLimitTestConfig(true, nil))
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err == nil {
		t.Fatal("Execute error = nil, want upstream 429")
	}
	bucket := rateLimitBucket(t, auth, "claude-sonnet-4-6")
	if got := rateLimitBucketRPM(bucket); got != 60 {
		t.Fatalf("bucket rate after quota-exhausted 429 = %.2f rpm, want unchanged 60", got)
	}
}

// Case ⑤: consecutive successes grow the rate by increaseRatio up to the ceiling.
func TestAntigravityRateLimit_SuccessStreakIncreasesRate(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	cfg := rateLimitTestConfig(true, func(rate *config.AntigravityRateLimitConfig) {
		rate.Burst = 10 // do not starve acquire while feeding successes
	})
	server, _ := newRateLimitTestServer(t, http.StatusOK, antigravityRateLimitSuccessBody)
	exec := NewAntigravityExecutor(cfg)
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("Execute #1 error = %v, want nil", err)
	}
	bucket := rateLimitBucket(t, auth, "claude-sonnet-4-6")
	if got := rateLimitBucketRPM(bucket); got != 60 {
		t.Fatalf("rate after 1 success = %.2f, want 60 (below successes-per-increase)", got)
	}
	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("Execute #2 error = %v, want nil", err)
	}
	if got := rateLimitBucketRPM(bucket); got != 90 {
		t.Fatalf("rate after 2 successes = %.2f, want 90 (60 × 1.5)", got)
	}
}

// Case ⑥: adaptive rate is clamped to [min, max] from the live config.
func TestAntigravityRateLimit_ClampToBounds(t *testing.T) {
	params := antigravityRateParams{minRPM: 10, maxRPM: 20, burst: 1}
	bucket := &antigravityRateBucket{ratePerSec: 100.0 / 60, tokens: 1}
	bucket.clamp(params)
	if got := bucket.ratePerSec * 60; got != 20 {
		t.Fatalf("clamp high = %.2f rpm, want 20", got)
	}
	bucket.ratePerSec = 0.01 / 60
	bucket.clamp(params)
	if got := bucket.ratePerSec * 60; got != 10 {
		t.Fatalf("clamp low = %.2f rpm, want 10", got)
	}
}

// Case ⑦: after idle-reset worth of inactivity the learned rate restarts from the seed.
func TestAntigravityRateLimit_IdleResetRestoresSeed(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	server, _ := newRateLimitTestServer(t, http.StatusTooManyRequests, antigravityRateLimitSoft429Body)
	exec := NewAntigravityExecutor(rateLimitTestConfig(true, nil))
	auth := rateLimitTestAuth(server.URL)

	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err == nil {
		t.Fatal("Execute error = nil, want upstream 429")
	}
	bucket := rateLimitBucket(t, auth, "claude-sonnet-4-6")
	if got := rateLimitBucketRPM(bucket); got != 30 {
		t.Fatalf("rate after 429 = %.2f, want 30", got)
	}

	// Simulate a long idle period, then acquire: the bucket restarts from the seed.
	bucket.mu.Lock()
	bucket.lastActivity = time.Now().Add(-time.Hour)
	bucket.mu.Unlock()
	if err := exec.antigravityPreflightAcquire(context.Background(), auth, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("acquire after idle error = %v, want nil (reset bucket is full)", err)
	}
	if got := rateLimitBucketRPM(bucket); got != 60 {
		t.Fatalf("rate after idle reset = %.2f, want seed 60", got)
	}
}

// Case ⑧: per-model overrides apply only to matching models ("*" wildcard semantics).
func TestAntigravityRateLimit_ModelOverrideResolution(t *testing.T) {
	cfg := rateLimitTestConfig(true, func(rate *config.AntigravityRateLimitConfig) {
		rate.Models = []config.AntigravityRateLimitModelRule{
			{Name: "gemini-*-image*", InitialRPM: 3, MaxRPM: 8},
		}
	})

	matched := antigravityRateLimitParams(cfg, "gemini-3.1-flash-image-preview")
	if !matched.enabled || matched.initialRPM != 3 || matched.maxRPM != 8 {
		t.Fatalf("matched params = %+v, want initial 3 / max 8", matched)
	}
	unmatched := antigravityRateLimitParams(cfg, "gemini-3-flash")
	if unmatched.initialRPM != 60 || unmatched.maxRPM != 120 {
		t.Fatalf("unmatched params = %+v, want defaults 60/120", unmatched)
	}
}

// Case ⑨: credits-fallback requests bypass pacing and never touch the free bucket.
func TestAntigravityRateLimit_CreditsRequestsBypass(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	cfg := rateLimitTestConfig(true, nil)
	cfg.QuotaExceeded.AntigravityCredits = true
	server, count := newRateLimitTestServer(t, http.StatusOK, antigravityRateLimitSuccessBody)
	exec := NewAntigravityExecutor(cfg)
	auth := rateLimitTestAuth(server.URL)

	// Consume the only free-tier token.
	if err := rateLimitExecute(t, exec, auth, nil, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("free Execute error = %v, want nil", err)
	}
	// A normal follow-up would be denied locally; the credits request must pass.
	creditsCtx := cliproxyauth.WithAntigravityCredits(context.Background())
	if err := rateLimitExecute(t, exec, auth, creditsCtx, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("credits Execute error = %v, want nil (bypass)", err)
	}
	if *count != 2 {
		t.Fatalf("upstream request count = %d, want 2", *count)
	}
	bucket := rateLimitBucket(t, auth, "claude-sonnet-4-6")
	bucket.mu.Lock()
	streak := bucket.successStreak
	bucket.mu.Unlock()
	// Streak 1 comes from the first (free-tier) success; the credits success must
	// not have bumped it to 2.
	if streak != 1 {
		t.Fatalf("credits success leaked into free bucket: successStreak = %d, want 1", streak)
	}
}

// Refill math at bucket level (case ③ core): tokens accrue at the adaptive rate and
// cap at burst.
func TestAntigravityRateLimit_RefillMath(t *testing.T) {
	params := antigravityRateParams{initialRPM: 60, minRPM: 1, maxRPM: 120, burst: 2}
	start := time.Now()
	bucket := newAntigravityRateBucket(params, start) // tokens = 2
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.refill(start.Add(500*time.Millisecond), params) // +0.5 tokens at 1/sec
	if bucket.tokens != 0.5 {
		t.Fatalf("tokens after 500ms = %.3f, want 0.5", bucket.tokens)
	}
	bucket.refill(start.Add(10*time.Second), params) // +9.5 → capped at burst 2
	if bucket.tokens != 2 {
		t.Fatalf("tokens after cap = %.3f, want 2", bucket.tokens)
	}
	bucket.mu.Unlock()
}

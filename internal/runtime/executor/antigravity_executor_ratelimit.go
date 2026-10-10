package executor

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Per-(auth, model) adaptive AIMD token buckets for Antigravity pacing.
//
// Motivation: upstream per-model limits differ per account and are only observable
// through 429s. Reactive cooldown alone lets a burst walk the whole pool and hammer
// the upstream before every (auth, model) pair enters cooldown. The bucket paces
// requests locally before they reach the upstream: a denial returns a 429 with
// Retry-After, which the conductor records as a short per-(auth, model) cooldown
// and rotates to the next auth — exactly the existing short-cooldown failure path.
//
// State is process-local by design: the Antigravity pool is served by a single
// CPA instance (no home/cluster mode). If that ever changes, mirror the state via
// the home KV like antigravityShortCooldownKVKey does.

type antigravityRateBucket struct {
	mu sync.Mutex
	// ratePerSec is the adaptive token refill rate in tokens/second.
	ratePerSec float64
	// tokens is the current fill level, bounded by the configured burst.
	tokens float64
	// lastRefill is the last time tokens were refilled.
	lastRefill time.Time
	// successStreak counts consecutive upstream successes since the last increase.
	successStreak int
	// lastIncreaseAt throttles rate increases.
	lastIncreaseAt time.Time
	// lastActivity is the last acquire/record touch, driving idle reset.
	lastActivity time.Time
}

// bucket is created with initial rate and full burst; idle resets reuse this state.
func newAntigravityRateBucket(params antigravityRateParams, now time.Time) *antigravityRateBucket {
	bucket := &antigravityRateBucket{}
	bucket.reset(params, now)
	return bucket
}

// reset rewrites the adaptive state field-by-field: the struct embeds a mutex and
// must never be wholesale-copied while held.
func (b *antigravityRateBucket) reset(params antigravityRateParams, now time.Time) {
	b.ratePerSec = params.initialRPM / 60
	b.tokens = params.burst
	b.lastRefill = now
	b.successStreak = 0
	b.lastIncreaseAt = now
	b.lastActivity = now
}

var antigravityRateBuckets sync.Map // key authID|model → *antigravityRateBucket

func antigravityRateBucketKey(authID, model string) string {
	return strings.TrimSpace(authID) + "|" + strings.TrimSpace(model)
}

// antigravityRateParams is the resolved, per-model rate-limit configuration slice.
type antigravityRateParams struct {
	enabled              bool
	initialRPM           float64
	minRPM               float64
	maxRPM               float64
	burst                float64
	decreaseFactor       float64
	increaseRatio        float64
	successesPerIncrease int
	minIncreaseInterval  time.Duration
	idleReset            time.Duration
}

func parseAntigravityRateDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// antigravityRateLimitParams resolves the pacing parameters for a model, applying
// the first matching per-model rule over the global defaults. Values are read from
// the live config so hot-reloads take effect on the next request.
func antigravityRateLimitParams(cfg *config.Config, model string) antigravityRateParams {
	if cfg == nil {
		return antigravityRateParams{}
	}
	rate := cfg.Antigravity.RateLimit
	// config normalization runs at load time; treat zero as "not configured" anyway.
	if rate.InitialRPMPerAuth <= 0 || rate.MinRPMPerAuth <= 0 || rate.MaxRPMPerAuth <= 0 {
		return antigravityRateParams{}
	}
	params := antigravityRateParams{
		enabled:              rate.Enabled,
		initialRPM:           rate.InitialRPMPerAuth,
		minRPM:               rate.MinRPMPerAuth,
		maxRPM:               rate.MaxRPMPerAuth,
		burst:                rate.Burst,
		decreaseFactor:       rate.DecreaseFactor,
		increaseRatio:        rate.IncreaseRatio,
		successesPerIncrease: rate.SuccessesPerIncrease,
		minIncreaseInterval:  parseAntigravityRateDuration(rate.MinIncreaseInterval, time.Minute),
		idleReset:            parseAntigravityRateDuration(rate.IdleReset, 15*time.Minute),
	}
	if params.burst < 1 || params.decreaseFactor <= 0 || params.decreaseFactor >= 1 || params.increaseRatio <= 1 || params.successesPerIncrease < 1 {
		return antigravityRateParams{}
	}
	for i := range rate.Models {
		rule := rate.Models[i]
		if !helps.MatchModelPattern(rule.Name, model) {
			continue
		}
		if rule.InitialRPM > 0 {
			params.initialRPM = rule.InitialRPM
		}
		if rule.MinRPM > 0 {
			params.minRPM = rule.MinRPM
		}
		if rule.MaxRPM > 0 {
			params.maxRPM = rule.MaxRPM
		}
		if rule.Burst >= 1 {
			params.burst = rule.Burst
		}
		break
	}
	if params.minRPM > params.maxRPM {
		params.minRPM = params.maxRPM
	}
	params.initialRPM = math.Min(math.Max(params.initialRPM, params.minRPM), params.maxRPM)
	return params
}

func antigravityRateBucketFor(authID, model string, params antigravityRateParams, now time.Time) *antigravityRateBucket {
	key := antigravityRateBucketKey(authID, model)
	if value, ok := antigravityRateBuckets.Load(key); ok {
		if bucket, valid := value.(*antigravityRateBucket); valid && bucket != nil {
			return bucket
		}
	}
	bucket := newAntigravityRateBucket(params, now)
	actual, loaded := antigravityRateBuckets.LoadOrStore(key, bucket)
	if loaded {
		if existing, valid := actual.(*antigravityRateBucket); valid && existing != nil {
			return existing
		}
	}
	return bucket
}

func (b *antigravityRateBucket) clamp(params antigravityRateParams) {
	b.ratePerSec = math.Min(math.Max(b.ratePerSec, params.minRPM/60), params.maxRPM/60)
}

func (b *antigravityRateBucket) refill(now time.Time, params antigravityRateParams) {
	elapsed := now.Sub(b.lastRefill)
	if elapsed > 0 {
		b.tokens = math.Min(params.burst, b.tokens+b.ratePerSec*elapsed.Seconds())
		b.lastRefill = now
	}
}

// antigravityPreflightAcquire consumes one pacing token for (auth, model) before the
// upstream request. It returns nil when the request may proceed, or a 429 statusErr
// with Retry-After so the conductor rotates to another auth exactly like the
// executor-level short cooldown does. Credits-fallback requests bypass pacing.
func (e *AntigravityExecutor) antigravityPreflightAcquire(ctx context.Context, auth *cliproxyauth.Auth, baseModel string) error {
	params := antigravityRateLimitParams(e.cfg, baseModel)
	if !params.enabled || auth == nil || strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(baseModel) == "" {
		return nil
	}
	if antigravityShouldBypassShortCooldown(ctx, e.cfg) {
		return nil
	}
	now := time.Now()
	bucket := antigravityRateBucketFor(auth.ID, baseModel, params, now)
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	if params.idleReset > 0 && now.Sub(bucket.lastActivity) >= params.idleReset {
		// Stale learned rate (traffic moved away): restart from the seed.
		bucket.reset(params, now)
	}
	bucket.clamp(params)
	bucket.refill(now, params)
	bucket.lastActivity = now
	if bucket.tokens >= 1 {
		bucket.tokens--
		return nil
	}
	deficit := 1 - bucket.tokens
	wait := time.Duration(math.Ceil(deficit / bucket.ratePerSec * float64(time.Second)))
	if wait < time.Second {
		wait = time.Second
	}
	log.Debugf("antigravity executor: rate limit bucket empty for auth %s model %s (rate %.2f rpm), waiting %s to switch auth", auth.ID, baseModel, bucket.ratePerSec*60, wait)
	return statusErr{
		code:       http.StatusTooManyRequests,
		msg:        fmt.Sprintf("auth rate budget exhausted for model %s, %s until next token", baseModel, wait),
		retryAfter: &wait,
	}
}

// antigravityRecordUpstream429 applies the multiplicative AIMD decrease after an
// upstream 429. Only pacing-relevant shapes decrease the rate: soft/unknown 429s
// (the dominant production shape: bare RESOURCE_EXHAUSTED without reason details)
// and RATE_LIMIT_EXCEEDED with a retryAfter of at least the instant-retry
// threshold. QUOTA_EXHAUSTED (daily quota, not rate) and sub-3s instant retries
// are left to the existing cooldown machinery. Credits-fallback requests run on a
// separate paid quota and never touch the free-tier bucket.
func antigravityRecordUpstream429(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, baseModel string, decision antigravity429Decision) {
	params := antigravityRateLimitParams(cfg, baseModel)
	switch decision.kind {
	case antigravity429DecisionSoftRetry, antigravity429DecisionShortCooldownSwitchAuth:
	default:
		return
	}
	if !params.enabled || antigravityShouldBypassShortCooldown(ctx, cfg) || auth == nil || strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(baseModel) == "" {
		return
	}
	now := time.Now()
	bucket := antigravityRateBucketFor(auth.ID, baseModel, params, now)
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	before := bucket.ratePerSec * 60
	bucket.ratePerSec *= params.decreaseFactor
	bucket.clamp(params)
	bucket.tokens = 0
	bucket.successStreak = 0
	bucket.lastActivity = now
	log.Debugf("antigravity executor: rate decrease for auth %s model %s after upstream 429: %.2f → %.2f rpm", auth.ID, baseModel, before, bucket.ratePerSec*60)
}

// antigravityRecordSuccess applies the additive-ish AIMD increase: after
// successesPerIncrease consecutive successes and at least minIncreaseInterval since
// the last increase, the rate grows by increaseRatio up to the ceiling.
// Credits-fallback requests are excluded (separate paid quota).
func antigravityRecordSuccess(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, baseModel string) {
	params := antigravityRateLimitParams(cfg, baseModel)
	if !params.enabled || antigravityShouldBypassShortCooldown(ctx, cfg) || auth == nil || strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(baseModel) == "" {
		return
	}
	now := time.Now()
	bucket := antigravityRateBucketFor(auth.ID, baseModel, params, now)
	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	bucket.lastActivity = now
	bucket.successStreak++
	if bucket.successStreak < params.successesPerIncrease || now.Sub(bucket.lastIncreaseAt) < params.minIncreaseInterval {
		return
	}
	before := bucket.ratePerSec * 60
	bucket.ratePerSec *= params.increaseRatio
	bucket.clamp(params)
	bucket.successStreak = 0
	bucket.lastIncreaseAt = now
	log.Debugf("antigravity executor: rate increase for auth %s model %s after %d successes: %.2f → %.2f rpm", auth.ID, baseModel, params.successesPerIncrease, before, bucket.ratePerSec*60)
}

// resetAntigravityRateBuckets clears all pacing state (tests).
func resetAntigravityRateBuckets() {
	antigravityRateBuckets.Clear()
}

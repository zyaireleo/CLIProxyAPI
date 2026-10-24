package nativequota

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEngine(t testing.TB, provider string, fetch Fetcher) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	e := New(cfg, fetch)
	e.SetAccounts([]Account{{ID: "a", Provider: provider}})
	return e
}
func observation(id, provider string, at time.Time, remaining float64) Observation {
	pool := "codex"
	if provider == ProviderAntigravity {
		pool = "gemini"
	}
	return Observation{AuthID: id, Provider: provider, ObservedAt: at, Source: "external", Pools: []Pool{{ID: pool, Windows: []Window{{ID: "weekly", Remaining: fraction(remaining)}}}}}
}
func TestConfigDefaultsValidationAndDefensiveCopies(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Mode != ModeOff || cfg.RefreshConcurrency != 2 || cfg.AntigravityInterval != 5*time.Minute || cfg.CodexInterval != 15*time.Minute || cfg.MaxWait != 2*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	e := New(Config{}, nil)
	if e.Config().Mode != ModeOff {
		t.Fatal("zero config not off")
	}
	for _, change := range []func(*Config){func(c *Config) { c.Mode = "bad" }, func(c *Config) { c.MaxWait = 3 * time.Second }, func(c *Config) { c.RefreshConcurrency = -1 }, func(c *Config) { c.CodexInterval = -1 }, func(c *Config) { c.Providers = []string{"wrong"} }, func(c *Config) { c.Providers = []string{"codex", "codex"} }, func(c *Config) { c.UnknownMaxInFlight = 20 }, func(c *Config) { c.QueueLimit = -1 }} {
		bad := DefaultConfig()
		change(&bad)
		if bad.Validate() == nil || e.Configure(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	cfg.Mode = ModeEnforce
	if err := e.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Providers[0] = "evil"
	copy := e.Config()
	copy.Providers[0] = "bad"
	if e.Config().Providers[0] != ProviderAntigravity {
		t.Fatal("config aliased")
	}
}

func TestConfigureNormalizedEqualityAndProviderCoverage(t *testing.T) {
	e := New(Config{}, nil)
	for _, tc := range []struct {
		name    string
		cfg     Config
		changed bool
	}{
		{"explicit_defaults", DefaultConfig(), false},
		{"implicit_defaults", Config{}, false},
		{"empty_providers", Config{Providers: []string{}}, true},
		{"same_empty_providers", Config{Providers: []string{}}, false},
		{"nil_restores_defaults", Config{}, true},
		{"same_restored_defaults", DefaultConfig(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			epoch, changed := e.epoch, e.changed
			if err := e.Configure(tc.cfg); err != nil {
				t.Fatal(err)
			}
			if (e.epoch != epoch) != tc.changed || (e.changed != changed) != tc.changed {
				t.Fatal("unexpected configuration invalidation")
			}
			woke := false
			select {
			case <-e.wake:
				woke = true
			default:
			}
			if woke != tc.changed {
				t.Fatal("unexpected scheduler wakeup")
			}
			cfg := e.Config()
			wantProviders := len(tc.cfg.normalized().Providers)
			if cfg.Providers == nil || len(cfg.Providers) != wantProviders {
				t.Fatal("provider coverage changed during normalization")
			}
			if len(tc.cfg.Providers) > 0 {
				tc.cfg.Providers[0] = "caller_mutation"
			}
			if len(cfg.Providers) > 0 {
				cfg.Providers[0] = "copy_mutation"
				if e.Config().Providers[0] != ProviderAntigravity {
					t.Fatal("config providers aliased")
				}
			}
		})
	}
}

func TestObserveOrderingCopiesUnknownAndExactModels(t *testing.T) {
	e := testEngine(t, ProviderAntigravity, nil)
	at := time.Now().Add(-time.Second)
	ob := observation("a", ProviderAntigravity, at, 1e-9)
	ob.Pools[0].Models = []string{"gemini-pro"}
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	*ob.Pools[0].Windows[0].Remaining = 0
	ob.Pools[0].Models[0] = "claude-opus"
	v := e.Assess("a", ProviderAntigravity, "gemini-pro", at)
	if v.State != "ready" || !v.Allowed || *v.Pools[0].Windows[0].Remaining != 1e-9 || v.Weight < 0.25 {
		t.Fatalf("tiny balance or alias: %+v", v)
	}
	*v.Pools[0].Windows[0].Remaining = 0
	v.Pools[0].Models[0] = "changed"
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", at); v.State != "ready" {
		t.Fatal("view alias")
	}
	if v = e.Assess("a", ProviderAntigravity, "gemini-unlisted", at); v.State != "unknown" {
		t.Fatal("ignored exact membership")
	}
	if e.Observe(observation("a", ProviderAntigravity, at, 0)) || e.Observe(observation("a", ProviderAntigravity, at.Add(-time.Second), 0)) {
		t.Fatal("accepted old observation")
	}
	if e.Observe(Observation{AuthID: "a", Provider: ProviderAntigravity, ObservedAt: time.Now()}) {
		t.Fatal("empty overwrote balance")
	}
	if e.Observe(observation("a", ProviderCodex, time.Now(), 0)) {
		t.Fatal("provider mismatch")
	}
	if e.Observe(observation("a", ProviderAntigravity, time.Now().Add(time.Hour), 0)) {
		t.Fatal("future observation")
	}
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", at.Add(16*time.Minute)); v.State != "unknown" || !v.Stale {
		t.Fatal("stale balance trusted")
	}
	if v = e.Assess("a", ProviderCodex, "gpt-5", at); v.Allowed || v.Reason != "provider_mismatch" {
		t.Fatal("provider mismatch allowed")
	}
}

func TestPassiveMergeDoesNotFreshenOrEraseOtherWindows(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	at := time.Now().Add(-time.Minute)
	ob := observation("a", ProviderCodex, at, 0.8)
	ob.Pools[0].Windows = append(ob.Pools[0].Windows, Window{ID: "monthly", Remaining: fraction(0.5)})
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	partial := observation("a", ProviderCodex, at.Add(30*time.Second), 0.7)
	partial.Partial = true
	partial.Source = SourceCodexHeaders
	if !e.Observe(partial) {
		t.Fatal("partial")
	}
	v := e.Assess("a", ProviderCodex, "gpt-5", at.Add(45*time.Minute+time.Second))
	if v.State != "unknown" || len(v.Pools[0].Windows) != 2 || !v.Pools[0].Windows[1].ObservedAt.Equal(at) {
		t.Fatalf("freshened omitted monthly: %+v", v)
	}
	// Explicit blocked metadata must survive a passive response omitting flags.
	full := observation("a", ProviderCodex, time.Now().Add(-10*time.Millisecond), 1)
	full.Pools[0].Blocked = true
	full.Pools[0].BlockedKnown = true
	if !e.Observe(full) {
		t.Fatal("blocked")
	}
	partial = observation("a", ProviderCodex, time.Now(), 1)
	partial.Partial = true
	if !e.Observe(partial) {
		t.Fatal("header")
	}
	if v = e.Assess("a", ProviderCodex, "gpt-5", time.Now()); v.Allowed {
		t.Fatal("passive omitted flag cleared block")
	}
}

func TestFailedEvidenceAndKnownZeroReset(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	at := time.Now().Add(-time.Hour)
	ob := observation("a", ProviderCodex, at, 0)
	ob.Pools[0].Windows[0].ResetAt = at.Add(24 * time.Hour)
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	e.mu.Lock()
	e.accounts["a"].invalidAt = at.Add(time.Minute)
	e.mu.Unlock()
	v := e.Assess("a", ProviderCodex, "gpt-5", time.Now())
	if v.Allowed || v.State != "exhausted" || !v.RefreshFailed || !v.Stale {
		t.Fatalf("failure erased hard zero: %+v", v)
	}
	if v = e.Assess("a", ProviderCodex, "gpt-5", at.Add(24*time.Hour)); v.State != "unknown" || !v.Allowed {
		t.Fatal("reset did not half-open")
	}
	if !e.Observe(observation("a", ProviderCodex, time.Now(), 0.4)) {
		t.Fatal("new positive")
	}
	if v = e.Assess("a", ProviderCodex, "gpt-5", time.Now()); !v.Allowed || v.RefreshFailed {
		t.Fatal("new evidence not restored")
	}
}

func TestHealthScopesNoForceZeroAndRefreshDoesNotClear429(t *testing.T) {
	e := testEngine(t, ProviderAntigravity, nil)
	at := time.Now().Add(-time.Second)
	ob := observation("a", ProviderAntigravity, at, 0.8)
	ob.Pools = append(ob.Pools, Pool{ID: "claude_gpt", Windows: []Window{{ID: "weekly", Remaining: fraction(0.9)}}})
	e.Observe(ob)
	e.RecordResult("a", ProviderAntigravity, "gemini-pro", 429, false, time.Minute)
	v := e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now())
	if v.Allowed || v.Reason != "rate_limit" || *v.Pools[0].Windows[0].Remaining != 0.8 {
		t.Fatalf("rate limit: %+v", v)
	}
	if v = e.Assess("a", ProviderAntigravity, "claude-sonnet", time.Now()); !v.Allowed || v.Weight < 0.5 {
		t.Fatal("cross group health leaked")
	}
	ob.ObservedAt = time.Now()
	e.Observe(ob)
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now()); v.Allowed {
		t.Fatal("quota 200 erased rate limit")
	}
	e.RecordResult("a", ProviderAntigravity, "gemini-pro", 200, true, 0)
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now()); v.Allowed {
		t.Fatal("concurrent success erased denial")
	}
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now().Add(2*time.Minute)); !v.Allowed {
		t.Fatal("denial permanent")
	}
	for range 2 {
		e.RecordResult("a", ProviderAntigravity, "claude-sonnet", 503, false, 0)
	}
	if v = e.Assess("a", ProviderAntigravity, "claude-sonnet", time.Now()); !v.Allowed {
		t.Fatal("circuit too early")
	}
	e.RecordResult("a", ProviderAntigravity, "claude-sonnet", 503, false, 0)
	if v = e.Assess("a", ProviderAntigravity, "claude-sonnet", time.Now()); v.Allowed || v.Reason != "circuit" {
		t.Fatal("no circuit")
	}
	e.RecordResult("a", ProviderAntigravity, "claude-sonnet", 401, false, 0)
	if v = e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now().Add(30*time.Second)); v.Allowed {
		t.Fatal("auth failure not account scoped")
	}
}

func TestCodexModelLimitHealthDoesNotPoisonBase(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	ob := observation("a", ProviderCodex, time.Now().Add(-time.Second), 1)
	ob.Pools = append(ob.Pools, Pool{ID: "codex:gpt-spark", Models: []string{"gpt-spark"}, Windows: []Window{{ID: "short", Remaining: fraction(1)}}})
	e.Observe(ob)
	e.RecordResult("a", ProviderCodex, "gpt-spark", 429, false, time.Minute)
	if e.Assess("a", ProviderCodex, "gpt-spark", time.Now()).Allowed {
		t.Fatal("specific allowed")
	}
	if !e.Assess("a", ProviderCodex, "gpt-main", time.Now()).Allowed {
		t.Fatal("specific poisoned base")
	}
}

func TestModesManualDisabledAndSafeEvents(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	cfg := e.Config()
	cfg.Mode = ModeOff
	e.Configure(cfg)
	if e.Observe(observation("a", ProviderCodex, time.Now(), 0)) || e.RequestRefresh("a", "secret") {
		t.Fatal("off mutation")
	}
	e.RecordResult("a", ProviderCodex, "gpt-5", 429, false, 0)
	e.RecordDecision("a", "a", ProviderCodex, "secret")
	if len(e.Events()) != 0 {
		t.Fatal("off event")
	}
	r, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	r()
	cfg.Mode = ModeShadow
	e.Configure(cfg)
	e.Observe(observation("a", ProviderCodex, time.Now(), 0))
	if !e.Assess("a", ProviderCodex, "gpt-5", time.Now()).Allowed {
		t.Fatal("shadow blocked")
	}
	r, err = e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	r()
	if e.Assess("a", ProviderCodex, "gpt-5", time.Now()).InFlight != 0 {
		t.Fatal("shadow lease")
	}
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Disabled: true}})
	if e.Assess("a", ProviderCodex, "gpt-5", time.Now()).Allowed {
		t.Fatal("manual disabled not strict")
	}
	cfg.Mode = ModeEnforce
	e.Configure(cfg)
	_, err = e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if !errors.Is(err, ErrDisabled) || !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("wrong local error: %v", err)
	}
	for range 300 {
		e.RecordDecision("a", "a", ProviderCodex, "Bearer secret")
	}
	events := e.Events()
	if len(events) != maxEvents {
		t.Fatalf("events unbounded: %d", len(events))
	}
	b, _ := json.Marshal(events)
	if strings.Contains(string(b), "secret") {
		t.Fatal("unsafe event")
	}
	events[0].Reason = "mutated"
	if e.Events()[0].Reason == "mutated" {
		t.Fatal("events alias")
	}
}

func TestSummaryAndLatencyWeight(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	e.Observe(observation("a", ProviderCodex, time.Now(), 0.00000001))
	v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now())
	if len(v.Pools) != 0 || v.Weight < 0.25 {
		t.Fatal("summary or quota floor")
	}
	e.RecordResultWithLatency("a", ProviderCodex, "gpt-5", 200, true, 0, 3*time.Second)
	v2 := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now())
	if v2.Weight >= v.Weight || v2.Weight <= 0 {
		t.Fatal("latency not weighted")
	}
}

func TestConcurrentReadersWriters(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 100 {
				if worker%2 == 0 {
					e.Observe(observation("a", ProviderCodex, time.Now(), 0.5))
					e.RecordResult("a", ProviderCodex, "gpt-5", 200, true, 0)
				} else {
					_ = e.Assess("a", ProviderCodex, "gpt-5", time.Now())
					_ = e.Views(time.Now())
					_ = e.Events()
					_ = e.Config()
				}
				if i%10 == 0 {
					e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex}})
				}
			}
		}(worker)
	}
	wg.Wait()
	if math.IsNaN(e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()).Weight) {
		t.Fatal("invalid weight")
	}
}

func TestRevisionCheckedEvidenceAndFeedback(t *testing.T) {
	for _, provider := range []string{ProviderAntigravity, ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			model := "gpt-5"
			if provider == ProviderAntigravity {
				model = "gemini-pro"
			}
			e := testEngine(t, provider, func(context.Context, Account) (Observation, error) { return Observation{}, ErrNoQuota })
			e.SetAccounts([]Account{{ID: "a", Provider: provider, Revision: "principal-A"}})
			if !e.ObserveForRevision(observation("a", provider, time.Now().Add(-time.Second), 0.2), "principal-A") {
				t.Fatal("valid A evidence")
			}
			e.SetAccounts(nil)
			e.SetAccounts([]Account{{ID: "a", Provider: provider, Revision: "principal-B"}})
			if !e.ObserveForRevision(observation("a", provider, time.Now().Add(-time.Millisecond), 0.8), "principal-B") {
				t.Fatal("valid B evidence")
			}
			events := len(e.Events())
			for _, revision := range []string{"principal-A", ""} {
				if e.ObserveForRevision(observation("a", provider, time.Now(), 0), revision) {
					t.Fatal("old principal overwrote B")
				}
				e.RecordResultForRevision("a", provider, model, revision, 429, false, time.Hour)
				e.RecordResultWithLatencyForRevision("a", provider, model, revision, 401, false, time.Hour, time.Minute)
				e.RecordResultWithLatencyForRevision("a", provider, model, revision, 200, true, 0, time.Minute)
			}
			v := e.Assess("a", provider, model, time.Now())
			if v.State != "ready" || !v.Allowed || *v.Pools[0].Windows[0].Remaining != 0.8 || len(e.Events()) != events {
				t.Fatalf("stale feedback affected B: %+v", v)
			}
			e.mu.Lock()
			s := e.accounts["a"]
			clean := len(s.health) == 0 && len(s.metrics) == 0 && !s.requested && s.lastUsed.IsZero()
			e.mu.Unlock()
			if !clean {
				t.Fatal("stale feedback mutated health/metrics/scheduling")
			}
			e.RecordResultForRevision("a", provider, model, "principal-B", 429, false, time.Minute)
			if e.AssessSummary("a", provider, model, time.Now()).Allowed {
				t.Fatal("current revision feedback ignored")
			}
		})
	}
	// Existing contract methods intentionally preserve their unrestricted behavior.
	e := testEngine(t, ProviderCodex, nil)
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "B"}})
	if !e.Observe(observation("a", ProviderCodex, time.Now(), 1)) {
		t.Fatal("legacy wrapper changed")
	}
}

func TestIncompleteWindowsStayUnknownAndBounded(t *testing.T) {
	cases := []struct {
		name, provider, model, raw string
		parse                      func([]byte, string, time.Time) (Observation, error)
	}{
		{"AG short", ProviderAntigravity, "gemini-pro", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":1}]}]}`, ParseAntigravity},
		{"AG fallback", ProviderAntigravity, "gemini-pro", `{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1}}}}`, ParseAntigravity},
		{"Codex short", ProviderCodex, "gpt-5", `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}`, ParseCodexUsage},
		{"Codex unclassified", ProviderCodex, "gpt-5", `{"rate_limit":{"primary_window":{"used_percent":0}}}`, ParseCodexUsage},
		{"Codex specific only", ProviderCodex, "gpt-spark", `{"additional_rate_limits":[{"limit_name":"gpt-spark","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800}}}]}`, ParseCodexUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngine(t, tc.provider, nil)
			cfg := e.Config()
			cfg.MaxWait = 10 * time.Millisecond
			if err := e.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			ob, err := tc.parse([]byte(tc.raw), "a", time.Now())
			if err != nil || !e.Observe(ob) {
				t.Fatal(err)
			}
			v := e.AssessSummary("a", tc.provider, tc.model, time.Now())
			if v.State != "unknown" || !v.Allowed || v.Reason != "incomplete_windows" {
				t.Fatalf("incomplete ready: %+v", v)
			}
			r, err := e.Acquire(context.Background(), "a", tc.provider, tc.model)
			if err != nil {
				t.Fatal(err)
			}
			defer r()
			if _, err = e.Acquire(context.Background(), "a", tc.provider, tc.model); !errors.Is(err, ErrWaitBudget) {
				t.Fatalf("incomplete got full capacity: %v", err)
			}
		})
	}
}

func TestCodexOpaqueLimitFeedbackIsExactModelOnly(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	ob := observation("a", ProviderCodex, time.Now(), 1)
	ob.Pools = append(ob.Pools, Pool{ID: "codex:opaque-namespace", Windows: []Window{{ID: "short", Remaining: fraction(1)}}})
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	for _, status := range []int{429, 503, 503, 503, 401} {
		e.RecordResultWithLatency("a", ProviderCodex, "gpt-spark", status, false, time.Minute, time.Second)
	}
	if e.AssessSummary("a", ProviderCodex, "gpt-spark", time.Now()).Allowed {
		t.Fatal("failed model not blocked")
	}
	v := e.AssessSummary("a", ProviderCodex, "gpt-main", time.Now())
	if !v.Allowed || v.Weight != 1 {
		t.Fatalf("opaque limit poisoned sibling: %+v", v)
	}
}

func BenchmarkAssessSummary(b *testing.B) {
	e := testEngine(b, ProviderCodex, nil)
	e.Observe(observation("a", ProviderCodex, time.Now(), 0.9))
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.AssessSummary("a", ProviderCodex, "gpt-5", now)
		}
	})
}
func BenchmarkAcquireRelease(b *testing.B) {
	e := testEngine(b, ProviderCodex, nil)
	e.Observe(observation("a", ProviderCodex, time.Now(), 0.9))
	ctx := WithWaitBudget(context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		release, err := e.Acquire(ctx, "a", ProviderCodex, "gpt-5")
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}

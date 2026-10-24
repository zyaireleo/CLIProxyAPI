package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type nativeQuotaTestExecutor struct {
	provider  string
	execute   func(context.Context, *Auth) (cliproxyexecutor.Response, error)
	stream    func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error)
	read      func(context.Context, *Auth, *http.Request) (*http.Response, error)
	refresh   func(context.Context, *Auth) (*Auth, error)
	calls     atomic.Int32
	counts    atomic.Int32
	refreshes atomic.Int32
}

func (e *nativeQuotaTestExecutor) Identifier() string { return e.provider }
func (e *nativeQuotaTestExecutor) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	if e.execute != nil {
		return e.execute(ctx, a)
	}
	return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
}
func (e *nativeQuotaTestExecutor) ExecuteStream(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return e.stream(ctx, a)
}
func (e *nativeQuotaTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.counts.Add(1)
	return cliproxyexecutor.Response{}, nil
}
func (e *nativeQuotaTestExecutor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	e.refreshes.Add(1)
	if e.refresh != nil {
		return e.refresh(ctx, a)
	}
	return a, nil
}
func (e *nativeQuotaTestExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	if e.read != nil {
		return e.read(ctx, a, r)
	}
	return nil, errors.New("no network in fixture")
}

func nativeQuotaTestManager(t *testing.T, mode nativequota.Mode, selector Selector) (*Manager, *nativeQuotaTestExecutor) {
	t.Helper()
	m := NewManager(nil, selector, nil)
	exec := &nativeQuotaTestExecutor{provider: "codex"}
	m.RegisterExecutor(exec)
	cfg := nativequota.DefaultConfig()
	cfg.Mode = mode
	cfg.MaxInFlight = 1
	cfg.MaxWait = 40 * time.Millisecond
	e := nativequota.New(cfg, nil)
	m.nativeQuota.Store(e)
	for _, id := range []string{"high", "low"} {
		priority := "10"
		if id == "low" {
			priority = "1"
		}
		_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": priority, AttributeAuthKind: AuthKindOAuth}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		m.RefreshSchedulerEntry(id)
	}
	return m, exec
}
func nativeQuotaTestObserve(t *testing.T, m *Manager, id string, remaining float64) {
	t.Helper()
	at := time.Now()
	if previous := m.NativeQuota().Assess(id, "codex", "gpt-5.4", at).ObservedAt; !at.After(previous) {
		at = previous.Add(time.Nanosecond)
	}
	if !m.NativeQuota().Observe(nativequota.Observation{AuthID: id, Provider: "codex", ObservedAt: at, Source: nativequota.SourceCodexUsage, Pools: []nativequota.Pool{{ID: "codex", Windows: []nativequota.Window{{ID: "weekly", DurationSeconds: 604800, Remaining: &remaining, ResetAt: time.Now().Add(time.Hour)}}}}}) {
		t.Fatal("observation rejected")
	}
}

func TestNativeQuotaEligibilityBeforePriorityAndAffinity(t *testing.T) {
	for _, kind := range []string{"round-robin", "weighted", "affinity"} {
		t.Run(kind, func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			if kind == "weighted" {
				selector = &WeightedRoundRobinSelector{}
			}
			if kind == "affinity" {
				s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
				selector = s
				defer s.Stop()
			}
			m, _ := nativeQuotaTestManager(t, nativequota.ModeEnforce, selector)
			nativeQuotaTestObserve(t, m, "high", 1)
			nativeQuotaTestObserve(t, m, "low", 1)
			opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"native-quota-session"}}}
			selected, err := m.SelectAuth(context.Background(), "codex", "gpt-5.4", opts)
			if err != nil || selected.ID != "high" {
				t.Fatalf("initial pick %v %v", selected, err)
			}
			nativeQuotaTestObserve(t, m, "high", 0)
			for _, path := range []string{"public", "single", "mixed"} {
				var a *Auth
				switch path {
				case "public":
					a, err = m.SelectAuth(context.Background(), "codex", "gpt-5.4", opts)
				case "single":
					a, _, err = m.pickNext(context.Background(), "codex", "gpt-5.4", opts, nil)
				case "mixed":
					a, _, _, err = m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5.4", opts, nil)
				}
				if err != nil || a == nil || a.ID != "low" {
					t.Fatalf("%s picked depleted tier: %v %v", path, a, err)
				}
			}
			high, _ := m.GetByID("high")
			if high.Disabled || authPriority(high) != 10 {
				t.Fatal("quota rewrote manual policy")
			}
			low, _ := m.GetByID("low")
			low.Disabled = true
			_, _ = m.Update(context.Background(), low)
			if a, err := m.SelectAuth(context.Background(), "codex", "gpt-5.4", opts); err == nil || a != nil {
				t.Fatal("manually disabled credential selected")
			}
		})
	}
}

func TestNativeQuotaOffAndShadowPreserveLiveSelection(t *testing.T) {
	for _, mode := range []nativequota.Mode{nativequota.ModeOff, nativequota.ModeShadow} {
		t.Run(string(mode), func(t *testing.T) {
			selector := &RoundRobinSelector{}
			m, exec := nativeQuotaTestManager(t, mode, selector)
			if mode == nativequota.ModeShadow {
				nativeQuotaTestObserve(t, m, "high", 0)
				nativeQuotaTestObserve(t, m, "low", 1)
			}
			before, _ := m.GetByID("high")
			for i := 0; i < 4; i++ {
				a, _, _, err := m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5.4", cliproxyexecutor.Options{}, nil)
				if err != nil || a.ID != "high" {
					t.Fatalf("live pick changed: %v %v", a, err)
				}
			}
			if len(selector.lastPicked) != 0 {
				t.Fatal("shadow consumed common live selector rotation")
			}
			after, _ := m.GetByID("high")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("selection changed persistent credential")
			}
			if exec.calls.Load() != 0 {
				t.Fatal("selection generated a probe")
			}
			if mode == nativequota.ModeShadow {
				found := false
				for _, event := range m.NativeQuota().Events() {
					if event.Kind == "decision" && event.AuthID == "high" && event.ProposedID == "low" {
						found = true
					}
				}
				if !found {
					t.Fatal("shadow did not record pure hypothetical lower tier")
				}
			}
			if m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()).InFlight != 0 {
				t.Fatal("selection acquired a lease")
			}
			if mode == nativequota.ModeOff && len(m.NativeQuota().Events()) != 0 {
				t.Fatal("off mode changed quota state")
			}
		})
	}
}

func TestNativeQuotaLeaseReleaseAndCountExclusion(t *testing.T) {
	m, exec := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
	nativeQuotaTestObserve(t, m, "high", 1)
	check := func(ctx context.Context, a *Auth) {
		t.Helper()
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("queue limit became network deadline")
		}
		if v := m.NativeQuota().Assess(a.ID, a.Provider, "gpt-5.4", time.Now()); v.InFlight != 1 {
			t.Fatalf("missing execution lease %+v", v)
		}
	}
	exec.execute = func(ctx context.Context, a *Auth) (cliproxyexecutor.Response, error) {
		check(ctx, a)
		return cliproxyexecutor.Response{}, errors.New("fixture upstream error")
	}
	a, _ := m.GetByID("high")
	_, _ = m.executeNativeQuotaAttempt(context.Background(), exec, a, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if v := m.NativeQuota().Assess(a.ID, a.Provider, "gpt-5.4", time.Now()); v.InFlight != 0 {
		t.Fatal("error leaked lease")
	}
	release, err := m.NativeQuota().Acquire(context.Background(), "high", "codex", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil || exec.counts.Load() != 1 {
		t.Fatalf("CountTokens used admission: %v", err)
	}
	release()
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("fixture")}
	exec.stream = func(ctx context.Context, a *Auth) (*cliproxyexecutor.StreamResult, error) {
		check(ctx, a)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Chunks
	if v := m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()); v.InFlight != 1 {
		t.Fatal("stream released before terminal boundary")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()).InFlight != 0 {
		if time.Now().After(deadline) {
			t.Fatal("cancel leaked stream lease")
		}
		time.Sleep(time.Millisecond)
	}
	close(chunks)
}

type nativeQuotaResultHook struct {
	NoopHook
	results int
}

func (h *nativeQuotaResultHook) OnResult(context.Context, Result) { h.results++ }

func TestNativeQuotaAdmissionDoesNotExpandRetries(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[streaming], func(t *testing.T) {
			m, exec := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
			hook := &nativeQuotaResultHook{}
			m.hook = hook
			cfg := m.NativeQuota().Config()
			cfg.MaxWait = 20 * time.Millisecond
			if err := m.NativeQuota().Configure(cfg); err != nil {
				t.Fatal(err)
			}
			m.SetRetryConfig(5, time.Second, 10)
			before := make(map[string]*Auth)
			for _, id := range []string{"high", "low"} {
				nativeQuotaTestObserve(t, m, id, 1)
				release, err := m.NativeQuota().Acquire(context.Background(), id, "codex", "gpt-5.4")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				before[id], _ = m.GetByID(id)
			}
			selections := 0
			opts := cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(string) { selections++ },
			}}
			var err error
			start := time.Now()
			if streaming {
				_, err = m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts)
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts)
			}
			if !errors.Is(err, nativequota.ErrWaitBudget) || exec.calls.Load() != 0 || selections != 1 {
				t.Fatalf("admission expanded retries: %v calls=%d selections=%d", err, exec.calls.Load(), selections)
			}
			if elapsed := time.Since(start); elapsed < cfg.MaxWait || elapsed > time.Second {
				t.Errorf("unexpected local queue duration: %v", elapsed)
			}
			if status := statusCodeFromError(err); status != http.StatusServiceUnavailable {
				t.Errorf("local admission status = %d, want 503", status)
			}
			wrapped := wrapRequestStopError(err)
			if !isRequestStopError(wrapped) || !isNativeQuotaAdmissionError(wrapped) || statusCodeFromError(wrapped) != http.StatusServiceUnavailable {
				t.Errorf("request-stop wrapper lost local admission classification: %v", wrapped)
			}
			if got := SafeResponseHeaders(wrapped).Get("Retry-After"); got != "1" {
				t.Errorf("wrapped local admission Retry-After = %q, want 1", got)
			}
			if hasUpstreamExecutionAttempt(err) || hook.results != 0 {
				t.Errorf("local denial recorded as upstream: results=%d", hook.results)
			}
			for id, original := range before {
				current, _ := m.GetByID(id)
				if !reflect.DeepEqual(original, current) {
					t.Errorf("local admission mutated credential %s", id)
				}
			}
			for _, event := range m.NativeQuota().Events() {
				if event.Kind == "health" {
					t.Error("local admission polluted native quota health")
				}
			}
		})
	}
}

func TestNativeQuota429FeedbackDeduplicatesAndSkipsCount(t *testing.T) {
	m, _ := nativeQuotaTestManager(t, nativequota.ModeShadow, nil)
	nativeQuotaTestObserve(t, m, "high", 1)
	auth, _ := m.GetByID("high")
	ctx := internallogging.WithFreshResponseHeadersHolder(m.nativeQuotaAttemptContext(context.Background(), auth))
	internallogging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": []string{"12.5"}, "X-Codex-Primary-Window-Minutes": []string{"300"}})
	result := Result{AuthID: "high", Provider: "codex", Model: "gpt-5.4", Error: &Error{HTTPStatus: 429}}
	result.SkipQuotaObservation = true
	m.recordNativeQuotaResult(ctx, result)
	for _, event := range m.NativeQuota().Events() {
		if event.Kind == "health" {
			t.Fatal("CountTokens triggered quota health")
		}
	}
	result.SkipQuotaObservation = false
	time.Sleep(time.Millisecond)
	for i := 0; i < 20; i++ {
		m.recordNativeQuotaResult(ctx, result)
	}
	health := 0
	for _, event := range m.NativeQuota().Events() {
		if event.Kind == "health" && event.Reason == "rate_limit" {
			health++
		}
	}
	if health != 1 {
		t.Fatalf("429 was not coalesced: %d", health)
	}
	v := m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now())
	if len(v.Pools) == 0 || len(v.Pools[0].Windows) != 2 {
		t.Fatalf("passive headers erased weekly evidence: %+v", v)
	}
	result.Error = &Error{Code: "local_rate_limit", HTTPStatus: 429}
	m.recordNativeQuotaResult(ctx, result)
}

func TestNativeQuotaConfigConflictPreservesEngine(t *testing.T) {
	m, _ := nativeQuotaTestManager(t, nativequota.ModeShadow, nil)
	before := m.NativeQuota().Config()
	if err := m.ApplyNativeQuotaConfig(context.Background(), internalconfig.NativeQuotaConfig{Mode: "enforce", MaxWait: "3s"}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if !reflect.DeepEqual(before, m.NativeQuota().Config()) {
		t.Fatal("invalid config mutated engine")
	}
}

func TestNativeQuotaSmoothWeightsAndFreeCapacity(t *testing.T) {
	m, _ := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
	low, _ := m.GetByID("low")
	low.Attributes["priority"] = "10"
	_, _ = m.Update(context.Background(), low)
	nativeQuotaTestObserve(t, m, "high", 0.75)
	nativeQuotaTestObserve(t, m, "low", 0.25)
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		a, err := m.SelectAuth(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		counts[a.ID]++
	}
	highWeight := m.NativeQuota().AssessSummary("high", "codex", "gpt-5.4", time.Now()).Weight
	lowWeight := m.NativeQuota().AssessSummary("low", "codex", "gpt-5.4", time.Now()).Weight
	wantHigh := int(40*highWeight/(highWeight+lowWeight) + 0.5)
	if counts["high"] != wantHigh || counts["low"] != 40-wantHigh {
		t.Fatalf("not smooth weighted scheduling: %v, weights %g/%g", counts, highWeight, lowWeight)
	}
	release, err := m.NativeQuota().Acquire(context.Background(), "high", "codex", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	a, err := m.SelectAuth(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{})
	if err != nil || a.ID != "low" {
		t.Fatalf("queued on full peer despite free same-tier credential: %v %v", a, err)
	}
}

func TestNativeQuotaPrincipalRevisionInvalidatesOnlyIdentityChange(t *testing.T) {
	m, _ := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
	a, _ := m.GetByID("high")
	a.Metadata = map[string]any{"account_id": "fixture-principal", "access_token": "fixture-first"}
	_, _ = m.Update(context.Background(), a)
	nativeQuotaTestObserve(t, m, "high", 0)
	a, _ = m.GetByID("high")
	a.Metadata["access_token"] = "fixture-rotated"
	_, _ = m.Update(context.Background(), a)
	if v := m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()); v.State != "exhausted" {
		t.Fatalf("token rotation erased quota evidence: %+v", v)
	}
	a, _ = m.GetByID("high")
	a.Metadata["account_id"] = "different-fixture-principal"
	_, _ = m.Update(context.Background(), a)
	if v := m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()); v.State != "unknown" {
		t.Fatalf("new principal inherited old quota: %+v", v)
	}
}

func TestNativeQuotaStreamAdmissionDoesNotExpandRetries(t *testing.T) {
	m, exec := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
	nativeQuotaTestObserve(t, m, "high", 1)
	m.SetRetryConfig(5, 0, 10)
	release, err := m.NativeQuota().Acquire(context.Background(), "high", "codex", "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if !errors.Is(err, nativequota.ErrWaitBudget) || exec.calls.Load() != 0 {
		t.Fatalf("stream admission expanded retries: %v calls=%d", err, exec.calls.Load())
	}
	if a, _ := m.GetByID("high"); a.Failed != 0 || a.LastError != nil {
		t.Fatal("local stream admission polluted health")
	}
}

func TestNativeQuotaShadowCollectorNeverRefreshesCredentials(t *testing.T) {
	for _, provider := range []string{"antigravity", "codex"} {
		for _, expiry := range []time.Duration{time.Hour, time.Minute, -time.Minute} {
			t.Run(provider+"/"+expiry.String(), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := nativequota.DefaultConfig()
				cfg.Mode = nativequota.ModeShadow
				m.nativeQuota.Store(nativequota.New(cfg, nil))
				exec := &nativeQuotaTestExecutor{provider: provider}
				var reads atomic.Int32
				exec.read = func(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
					reads.Add(1)
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > nativeQuotaCollectionTimeout {
						t.Error("quota collection missing bounded deadline")
					}
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("fixture unauthorized"))}, nil
				}
				exec.refresh = func(context.Context, *Auth) (*Auth, error) {
					return nil, &Error{HTTPStatus: 401, Message: "fixture refresh unauthorized"}
				}
				m.RegisterExecutor(exec)
				_, err := m.Register(context.Background(), &Auth{ID: "shadow-fixture", Provider: provider, Status: StatusActive, Metadata: map[string]any{"account_id": "fixture-principal", "access_token": "fixture-access", "refresh_token": "fixture-refresh", "expired": time.Now().Add(expiry).Format(time.RFC3339)}})
				if err != nil {
					t.Fatal(err)
				}
				before, _ := m.GetByID("shadow-fixture")
				if _, err = m.fetchNativeQuota(context.Background(), nativeQuotaAccount(before)); err == nil {
					t.Fatal("unauthorized/missing quota unexpectedly succeeded")
				}
				after, _ := m.GetByID(before.ID)
				if exec.refreshes.Load() != 0 {
					t.Fatal("shadow initiated forced OAuth refresh")
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("shadow quota failure mutated credential lifecycle")
				}
				if expiry < 0 && reads.Load() != 0 {
					t.Fatal("shadow sent expired access token")
				}
				if expiry > 0 && reads.Load() == 0 {
					t.Fatal("shadow refused still-valid access token")
				}
			})
		}
	}
}

func TestNativeQuotaLateAttemptCannotPoisonReplacementPrincipal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "stream"}[stream], func(t *testing.T) {
			m, exec := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
			old, _ := m.GetByID("high")
			old.Metadata = map[string]any{"account_id": "principal-A"}
			_, _ = m.Update(context.Background(), old)
			old, _ = m.GetByID("high")
			ctx := internallogging.WithFreshResponseHeadersHolder(m.nativeQuotaAttemptContext(context.Background(), old))
			var replacement *Auth
			replace := func(ctx context.Context) {
				a, _ := m.GetByID("high")
				a.Metadata["account_id"] = "principal-B"
				var err error
				replacement, err = m.Update(context.Background(), a)
				if err != nil {
					t.Fatal(err)
				}
				internallogging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": []string{"100"}, "X-Codex-Primary-Window-Minutes": []string{"300"}})
			}
			headers := http.Header{"X-Codex-Primary-Used-Percent": []string{"100"}, "X-Codex-Primary-Window-Minutes": []string{"300"}}
			if stream {
				exec.stream = func(ctx context.Context, _ *Auth) (*cliproxyexecutor.StreamResult, error) {
					replace(ctx)
					ch := make(chan cliproxyexecutor.StreamChunk, 1)
					ch <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: 429}}
					close(ch)
					return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: ch}, nil
				}
				result, err := m.executeNativeQuotaStreamAttempt(ctx, exec, old, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				for range result.Chunks {
				}
			} else {
				exec.execute = func(ctx context.Context, _ *Auth) (cliproxyexecutor.Response, error) {
					replace(ctx)
					return cliproxyexecutor.Response{Headers: headers}, &Error{HTTPStatus: 429}
				}
				_, _ = m.executeNativeQuotaAttempt(ctx, exec, old, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
			}
			m.recordNativeQuotaResult(ctx, Result{AuthID: "high", Provider: "codex", Model: "gpt-5.4", Error: &Error{HTTPStatus: 429}})
			view := m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now())
			if view.State != "unknown" || !view.ObservedAt.IsZero() || view.InFlight != 0 {
				t.Fatalf("late principal-A result poisoned principal-B: %+v", view)
			}
			// An unscoped result must not borrow the current principal revision.
			m.recordNativeQuotaResult(context.Background(), Result{AuthID: "high", Provider: "codex", Model: "gpt-5.4", Error: &Error{HTTPStatus: 429}})
			if view = m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()); view.State != "unknown" {
				t.Fatal("unscoped feedback borrowed replacement principal")
			}
			freshCtx := m.nativeQuotaAttemptContext(context.Background(), replacement)
			m.recordNativeQuotaResult(freshCtx, Result{AuthID: "high", Provider: "codex", Model: "gpt-5.4", Error: &Error{HTTPStatus: 429}})
			if view = m.NativeQuota().Assess("high", "codex", "gpt-5.4", time.Now()); view.State != "blocked" {
				t.Fatal("matching principal feedback was dropped")
			}
		})
	}
}

func TestNativeQuotaAdmissionRejectsSelectedRevisionAgainstEngineAtomically(t *testing.T) {
	m, exec := nativeQuotaTestManager(t, nativequota.ModeEnforce, nil)
	old, _ := m.GetByID("high")
	account := nativeQuotaAccount(old)
	account.Revision = "different-engine-principal"
	// The manager lookup intentionally still has the old principal. A mere
	// GetByID check cannot protect this engine-authoritative admission boundary.
	m.NativeQuota().SetAccounts([]nativequota.Account{account})
	_, err := m.executeNativeQuotaAttempt(context.Background(), exec, old, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if !errors.Is(err, nativequota.ErrAdmissionDenied) || exec.calls.Load() != 0 {
		t.Fatalf("stale selected principal reached executor: %v calls=%d", err, exec.calls.Load())
	}
}

func TestNativeQuotaCollectorStopsFallbackAfterOff(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := nativequota.DefaultConfig()
	cfg.Mode = nativequota.ModeEnforce
	m.nativeQuota.Store(nativequota.New(cfg, nil))
	exec := &nativeQuotaTestExecutor{provider: "antigravity"}
	var reads atomic.Int32
	exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		reads.Add(1)
		off := cfg
		off.Mode = nativequota.ModeOff
		if err := m.NativeQuota().Configure(off); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("fixture"))}, nil
	}
	m.RegisterExecutor(exec)
	a, err := m.Register(context.Background(), &Auth{ID: "off-fixture", Provider: "antigravity", Status: StatusActive, Metadata: map[string]any{"account_id": "principal", "access_token": "fixture", "refresh_token": "fixture", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a)); err == nil {
		t.Fatal("off transition accepted response")
	}
	if reads.Load() != 1 || exec.refreshes.Load() != 0 {
		t.Fatalf("off transition continued collection: reads=%d refreshes=%d", reads.Load(), exec.refreshes.Load())
	}
}

func TestNativeQuotaCollectorHonorsCancellationAndScheduledRevision(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := nativequota.DefaultConfig()
	cfg.Mode = nativequota.ModeShadow
	m.nativeQuota.Store(nativequota.New(cfg, nil))
	exec := &nativeQuotaTestExecutor{provider: "codex"}
	var reads atomic.Int32
	exec.read = func(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
		reads.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.RegisterExecutor(exec)
	a, err := m.Register(context.Background(), &Auth{ID: "cancel-fixture", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"account_id": "principal", "access_token": "fixture", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	account := nativeQuotaAccount(a)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = m.fetchNativeQuota(ctx, account); err == nil {
		t.Fatal("canceled collection succeeded")
	}
	if time.Since(start) > time.Second || reads.Load() != 1 {
		t.Fatal("quota operation ignored context cancellation")
	}
	account.Revision = "obsolete-scheduled-principal"
	if _, err = m.fetchNativeQuota(context.Background(), account); err == nil {
		t.Fatal("obsolete scheduled principal collected")
	}
	if reads.Load() != 1 {
		t.Fatal("stale scheduled collection hit upstream")
	}
}

func TestNativeQuotaCollectorUsesQuotaOnlyFallback(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := nativequota.DefaultConfig()
	cfg.Mode = nativequota.ModeShadow
	m.nativeQuota.Store(nativequota.New(cfg, nil))
	var urls []string
	exec := &nativeQuotaTestExecutor{provider: "antigravity"}
	exec.read = func(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		if !IsNativeQuotaRequest(ctx) || r.Header.Get("User-Agent") != nativeQuotaAGUserAgent {
			t.Fatal("quota request marker/UA missing")
		}
		urls = append(urls, r.URL.String())
		raw := `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}]}]}`
		if len(urls) > 1 {
			raw = `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":1}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1},{"bucketId":"3p-5h","window":"5h","remainingFraction":1}]}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
	}
	m.RegisterExecutor(exec)
	a := &Auth{ID: "ag", Provider: "antigravity", Status: StatusActive, Metadata: map[string]any{"access_token": "fixture", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "base_url": "http://127.0.0.1/forbidden"}}
	_, _ = m.Register(context.Background(), a)
	ob, err := m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 || urls[0] != nativeQuotaAGDailyURL || urls[1] != nativeQuotaAGSandboxURL {
		t.Fatalf("wrong fallback %v", urls)
	}
	if !m.NativeQuota().Observe(ob) {
		t.Fatal("collected evidence invalid")
	}
	v := m.NativeQuota().Assess("ag", "antigravity", "gemini-2.5-pro", time.Now())
	if v.State != "exhausted" {
		t.Fatalf("incomplete 200 lost depleted weekly: %+v", v)
	}
}

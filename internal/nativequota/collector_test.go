package nativequota

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func stopEngine(t testing.TB, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Error(err)
	}
}
func TestCollectorBoundedSingleflightCooldownAndDisabled(t *testing.T) {
	var active, peak, calls atomic.Int32
	started := make(chan string, 10)
	gate := make(chan struct{})
	fetch := func(ctx context.Context, a Account) (Observation, error) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		started <- a.ID
		select {
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		case <-gate:
		}
		return observation(a.ID, a.Provider, time.Now(), 1), nil
	}
	e := testEngine(t, ProviderCodex, fetch)
	var accounts []Account
	for i := range 5 {
		accounts = append(accounts, Account{ID: fmt.Sprint(i), Provider: ProviderCodex})
	}
	accounts = append(accounts, Account{ID: "disabled", Provider: ProviderCodex, Disabled: true})
	e.SetAccounts(accounts)
	if calls.Load() != 0 {
		t.Fatal("New/SetAccounts spawned work")
	}
	e.Start(context.Background())
	defer stopEngine(t, e)
	for range 2 {
		select {
		case id := <-started:
			if id == "disabled" {
				t.Fatal("queried disabled")
			}
		case <-time.After(time.Second):
			t.Fatal("no collector")
		}
	}
	if active.Load() != 2 {
		t.Fatal("not at bounded capacity")
	}
	if e.RequestRefresh("0", "secret") {
		t.Fatal("in-flight refresh not coalesced")
	}
	close(gate)
	waitFor(t, func() bool { return calls.Load() == 5 && active.Load() == 0 })
	if peak.Load() != 2 {
		t.Fatalf("concurrency exceeded: %d", peak.Load())
	}
	if e.RequestRefresh("0", "again") {
		t.Fatal("cooldown bypass")
	}
	e.RecordResult("0", ProviderCodex, "gpt-5", 429, false, time.Minute)
	for range 100 {
		e.RecordResult("0", ProviderCodex, "gpt-5", 429, false, time.Minute)
	}
	e.mu.Lock()
	s := e.accounts["0"]
	due := s.nextDue
	last := s.lastAttempt
	h := s.health["model:gpt-5"]
	requested := s.requested
	e.mu.Unlock()
	if due.Before(last.Add(e.Config().RefreshMinInterval)) || !requested || h.until.Sub(h.at) != time.Minute {
		t.Fatal("429 coalescing/min interval")
	}
	if calls.Load() != 5 {
		t.Fatal("429 synchronously fetched")
	}
}
func TestCollectorFailureUnknownAndLateResultsDoNotOverwrite(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			started := make(chan struct{})
			finish := make(chan struct{})
			e := testEngine(t, ProviderCodex, func(ctx context.Context, a Account) (Observation, error) {
				close(started)
				select {
				case <-ctx.Done():
					return Observation{}, ctx.Err()
				case <-finish:
				}
				if failure {
					return Observation{}, errors.New("Bearer secret")
				}
				return observation(a.ID, a.Provider, time.Now(), 0), nil
			})
			e.Observe(observation("a", ProviderCodex, time.Now().Add(-time.Hour), 0.7))
			if !e.RequestRefresh("a", "test") {
				t.Fatal("request")
			}
			e.Start(context.Background())
			defer stopEngine(t, e)
			<-started
			if !e.Observe(observation("a", ProviderCodex, time.Now(), 0.9)) {
				t.Fatal("new passive")
			}
			close(finish)
			waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.active == 0 })
			v := e.Assess("a", ProviderCodex, "gpt-5", time.Now())
			if v.State != "ready" || *v.Pools[0].Windows[0].Remaining != 0.9 || v.RefreshFailed {
				t.Fatalf("late result replaced new: %+v", v)
			}
		})
	}
	e := testEngine(t, ProviderCodex, func(context.Context, Account) (Observation, error) { return Observation{}, errors.New("secret") })
	e.Observe(observation("a", ProviderCodex, time.Now().Add(-time.Second), 0.8))
	e.RequestRefresh("a", "test")
	e.Start(context.Background())
	defer stopEngine(t, e)
	waitFor(t, func() bool { return e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()).RefreshFailed })
	v := e.Assess("a", ProviderCodex, "gpt-5", time.Now())
	if v.State != "unknown" || *v.Pools[0].Windows[0].Remaining != 0.8 {
		t.Fatalf("failure erased number: %+v", v)
	}
}
func TestCollectorOffStopRestartAndRevision(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	e := testEngine(t, ProviderCodex, func(ctx context.Context, a Account) (Observation, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return Observation{}, ctx.Err()
	})
	cfg := e.Config()
	cfg.Mode = ModeOff
	e.Configure(cfg)
	e.Start(context.Background())
	if e.RequestRefresh("a", "test") {
		t.Fatal("off requested")
	}
	stopEngine(t, e)
	if calls.Load() != 0 {
		t.Fatal("off fetched")
	}
	cfg.Mode = ModeEnforce
	e.Configure(cfg)
	e.Start(context.Background())
	<-started
	stopEngine(t, e)
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "new"}})
	e.Start(context.Background())
	<-started
	stopEngine(t, e)
	if calls.Load() != 2 {
		t.Fatal("restart failed")
	}
}
func TestCollectorDeadlineIdleBackoffAndReset(t *testing.T) {
	e := testEngine(t, ProviderAntigravity, nil)
	cfg := e.Config()
	cfg.RefreshMinInterval = time.Second
	e.Configure(cfg)
	now := time.Now()
	e.mu.Lock()
	s := e.accounts["a"]
	s.lastAttempt = now
	s.registeredAt = now.Add(-time.Hour)
	idle := e.nextDeadlineLocked(s, now)
	if !idle.Equal(now.Add(20 * time.Minute)) {
		e.mu.Unlock()
		t.Fatalf("idle deadline: %v", idle.Sub(now))
	}
	s.observation = observation("a", ProviderAntigravity, now, 1)
	s.observation.Pools[0].Windows[0].ResetAt = now.Add(10 * time.Second)
	if next := e.nextDeadlineLocked(s, now); !next.Equal(now.Add(10 * time.Second)) {
		e.mu.Unlock()
		t.Fatal("missed reset")
	}
	s.backoffUntil = now.Add(time.Minute)
	if next := e.nextDeadlineLocked(s, now); !next.Equal(s.backoffUntil) {
		e.mu.Unlock()
		t.Fatal("backoff ignored")
	}
	e.mu.Unlock()
}
func TestCollectorRemovedOrRevisedAccountCannotReceiveOldRead(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	e := testEngine(t, ProviderCodex, func(ctx context.Context, a Account) (Observation, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-finish:
			case <-ctx.Done():
				return Observation{}, ctx.Err()
			}
			return observation(a.ID, a.Provider, time.Now(), 0), nil
		}
		<-ctx.Done()
		return Observation{}, ctx.Err()
	})
	e.Start(context.Background())
	defer stopEngine(t, e)
	<-started
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "replacement"}})
	close(finish)
	waitFor(t, func() bool { return calls.Load() >= 2 })
	if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); v.State != "unknown" {
		t.Fatalf("old identity evidence: %+v", v)
	}
}
func TestCollectorRestartHonorsMinimumInterval(t *testing.T) {
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	e := testEngine(t, ProviderCodex, func(ctx context.Context, a Account) (Observation, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return Observation{}, ctx.Err()
	})
	e.Start(context.Background())
	<-started
	stopEngine(t, e)
	e.Start(context.Background())
	defer stopEngine(t, e)
	select {
	case <-started:
		t.Fatal("restart bypassed min interval")
	case <-time.After(20 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatal("extra attempt")
	}
}

func TestCollectorConfigureInFlight(t *testing.T) {
	for _, provider := range []string{ProviderAntigravity, ProviderCodex} {
		for _, change := range []string{"same", "normalized_defaults", "changed_mode"} {
			t.Run(provider+"/"+change, func(t *testing.T) {
				started := make(chan context.Context, 1)
				finish := make(chan struct{})
				var calls atomic.Int32
				e := New(DefaultConfig(), func(ctx context.Context, a Account) (Observation, error) {
					calls.Add(1)
					started <- ctx
					select {
					case <-finish:
					case <-ctx.Done():
					}
					// Even a successful late response must be rejected after a real change.
					return observation(a.ID, a.Provider, time.Now(), 0.8), nil
				})
				e.SetAccounts([]Account{{ID: "a", Provider: provider}})
				e.Start(context.Background())
				defer stopEngine(t, e)
				cfg := e.Config()
				cfg.Mode = ModeEnforce
				if err := e.Configure(cfg); err != nil {
					t.Fatal(err)
				}
				var fetchCtx context.Context
				select {
				case fetchCtx = <-started:
				case <-time.After(time.Second):
					t.Fatal("collector did not start after enabling")
				}
				e.mu.Lock()
				epoch, changed, due := e.epoch, e.changed, e.accounts["a"].nextDue
				e.mu.Unlock()
				switch change {
				case "normalized_defaults":
					cfg = Config{Mode: ModeEnforce}
				case "changed_mode":
					cfg.Mode = ModeShadow
				}
				// Reproduce the second apply from the config reload while Fetch is blocked.
				if err := e.Configure(cfg); err != nil {
					t.Fatal(err)
				}
				if change == "changed_mode" {
					if fetchCtx.Err() != context.Canceled {
						t.Error("changed mode did not cancel the old fetch")
					}
				} else {
					if err := fetchCtx.Err(); err != nil {
						t.Errorf("identical configuration canceled the fetch: %v", err)
					}
					e.mu.Lock()
					unchanged := e.epoch == epoch && e.changed == changed && e.accounts["a"].nextDue.Equal(due)
					e.mu.Unlock()
					if !unchanged {
						t.Error("identical configuration changed epoch, notification, or deadline")
					}
				}
				close(finish)
				waitFor(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.active == 0 })
				v := e.AssessSummary("a", provider, "", time.Now())
				if change == "changed_mode" {
					if !v.ObservedAt.IsZero() {
						t.Fatal("old configuration's successful result was applied")
					}
				} else if v.State != "ready" || v.ObservedAt.IsZero() || v.RefreshFailed {
					t.Fatalf("identical configuration discarded successful fetch: %+v", v)
				}
				if calls.Load() != 1 {
					t.Fatalf("configuration triggered another fetch: %d", calls.Load())
				}
			})
		}
	}
}

func TestCollectorCancelsRetiredConfigurationAndAccount(t *testing.T) {
	for _, change := range []string{"off", "provider_removed", "revision", "disabled", "removed"} {
		t.Run(change, func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			e := testEngine(t, ProviderCodex, func(ctx context.Context, a Account) (Observation, error) {
				if a.Revision == "B" {
					<-ctx.Done()
					return Observation{}, ctx.Err()
				}
				close(started)
				<-ctx.Done()
				close(canceled)
				return observation(a.ID, a.Provider, time.Now(), 0), nil
			})
			e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "A"}})
			e.Start(context.Background())
			defer stopEngine(t, e)
			<-started
			switch change {
			case "off":
				cfg := e.Config()
				cfg.Mode = ModeOff
				e.Configure(cfg)
			case "provider_removed":
				cfg := e.Config()
				cfg.Providers = []string{}
				e.Configure(cfg)
			case "revision":
				e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "B"}})
			case "disabled":
				e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "A", Disabled: true}})
			case "removed":
				e.SetAccounts(nil)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("retired fetch not canceled")
			}
			if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); !v.ObservedAt.IsZero() {
				t.Fatal("canceled result applied")
			}
		})
	}
}

func TestStopHonorsCallerContextForUncooperativeFetcher(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	e := testEngine(t, ProviderCodex, func(context.Context, Account) (Observation, error) {
		close(started)
		<-finish
		return Observation{}, ErrNoQuota
	})
	e.Start(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := e.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop: %v", err)
	}
	close(finish)
	stopEngine(t, e)
}

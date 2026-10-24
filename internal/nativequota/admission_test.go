package nativequota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitFor(t testing.TB, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func queuedCount(e *Engine) int { e.mu.Lock(); defer e.mu.Unlock(); return e.queued }
func singleLeaseEngine(t testing.TB) *Engine {
	e := testEngine(t, ProviderCodex, nil)
	cfg := e.Config()
	cfg.MaxInFlight = 1
	cfg.UnknownMaxInFlight = 1
	cfg.QueueLimit = 1
	cfg.MaxWait = 200 * time.Millisecond
	if err := e.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	e.Observe(observation("a", ProviderCodex, time.Now(), 1))
	return e
}
func TestAdmissionQueueCancellationIdempotentRelease(t *testing.T) {
	e := singleLeaseEngine(t)
	release, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); !v.AtCapacity || !v.Allowed || v.InFlight != 1 {
		t.Fatalf("capacity is not quota denial: %+v", v)
	}
	ctx, cancel := context.WithCancel(WithWaitBudget(context.Background()))
	done := make(chan error, 1)
	go func() {
		r, err := e.Acquire(ctx, "a", ProviderCodex, "gpt-5")
		if r != nil {
			r()
		}
		done <- err
	}()
	waitFor(t, func() bool { return queuedCount(e) == 1 })
	_, err = e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if !errors.Is(err, ErrQueueFull) || !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("wrong full error: %v", err)
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if queuedCount(e) != 0 {
		t.Fatal("canceled queue leaked")
	}
	release()
	release()
	if e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()).InFlight != 0 {
		t.Fatal("double release")
	}
}
func TestAdmissionWaitReleaseAndUnknownBound(t *testing.T) {
	e := singleLeaseEngine(t)
	release, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		r, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
		if r != nil {
			r()
		}
		done <- err
	}()
	waitFor(t, func() bool { return queuedCount(e) == 1 })
	release()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if queuedCount(e) != 0 || e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()).InFlight != 0 {
		t.Fatal("leaked released lease")
	}
	e.SetAccounts([]Account{{ID: "b", Provider: ProviderCodex}})
	cfg := e.Config()
	cfg.MaxInFlight = 4
	cfg.MaxWait = 20 * time.Millisecond
	e.Configure(cfg)
	r, err := e.Acquire(context.Background(), "b", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	defer r()
	_, err = e.Acquire(context.Background(), "b", ProviderCodex, "gpt-5")
	if !errors.Is(err, ErrWaitBudget) {
		t.Fatalf("unknown concurrency unbounded: %v", err)
	}
}
func TestCumulativeWaitBudgetAcrossRetriesExcludesGeneration(t *testing.T) {
	e := singleLeaseEngine(t)
	cfg := e.Config()
	cfg.MaxWait = 120 * time.Millisecond
	e.Configure(cfg)
	ctx := WithWaitBudget(context.Background())
	if WithWaitBudget(ctx) != ctx {
		t.Fatal("rewrapping reset budget")
	}
	blocker, _ := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	firstCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		r, err := e.Acquire(firstCtx, "a", ProviderCodex, "gpt-5")
		if r != nil {
			r()
		}
		done <- err
	}()
	waitFor(t, func() bool { return queuedCount(e) == 1 })
	time.Sleep(40 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	b := ctx.Value(budgetKey{}).(*waitBudget)
	b.mu.Lock()
	spent := b.spent
	b.mu.Unlock()
	if spent < 35*time.Millisecond || spent >= cfg.MaxWait {
		t.Fatalf("bad charged wait %v", spent)
	}
	blocker()
	// The existing stream context is not shortened by queue-wait accounting.
	r, err := e.Acquire(ctx, "a", ProviderCodex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(140 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("generation got queue timeout")
	}
	r()
	b.mu.Lock()
	after := b.spent
	b.mu.Unlock()
	if after != spent {
		t.Fatal("generation charged against wait budget")
	}
	blocker, _ = e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
	defer blocker()
	_, err = e.Acquire(ctx, "a", ProviderCodex, "gpt-5")
	if !errors.Is(err, ErrWaitBudget) {
		t.Fatalf("retry wait: %v", err)
	}
	start := time.Now()
	_, err = e.Acquire(ctx, "a", ProviderCodex, "gpt-5")
	if !errors.Is(err, ErrWaitBudget) || time.Since(start) > 30*time.Millisecond {
		t.Fatal("retry reset budget")
	}
}
func TestQueuedConfigAndAccountChanges(t *testing.T) {
	for _, change := range []string{"off", "disabled", "removed"} {
		t.Run(change, func(t *testing.T) {
			e := singleLeaseEngine(t)
			release, _ := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
			defer release()
			done := make(chan error, 1)
			go func() {
				r, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
				if r != nil {
					r()
				}
				done <- err
			}()
			waitFor(t, func() bool { return queuedCount(e) == 1 })
			switch change {
			case "off":
				cfg := e.Config()
				cfg.Mode = ModeOff
				e.Configure(cfg)
			case "disabled":
				e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Disabled: true}})
			case "removed":
				e.SetAccounts(nil)
			}
			err := <-done
			if change == "off" && err != nil {
				t.Fatal(err)
			}
			if change != "off" && !errors.Is(err, ErrAdmissionDenied) {
				t.Fatalf("change bypassed: %v", err)
			}
			if queuedCount(e) != 0 {
				t.Fatal("queue slot leaked")
			}
		})
	}
}
func TestAdmissionRevisionReplacementWhileQueued(t *testing.T) {
	e := singleLeaseEngine(t)
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "A"}})
	if !e.ObserveForRevision(observation("a", ProviderCodex, time.Now(), 1), "A") {
		t.Fatal("observe A")
	}
	oldRelease, err := e.AcquireForRevision(context.Background(), "a", ProviderCodex, "gpt-5", "A")
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelease()
	done := make(chan error, 1)
	go func() {
		r, err := e.AcquireForRevision(context.Background(), "a", ProviderCodex, "gpt-5", "A")
		if r != nil {
			r()
		}
		done <- err
	}()
	waitFor(t, func() bool { return queuedCount(e) == 1 })
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "B"}})
	if err = <-done; !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("queued old revision admitted: %v", err)
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != "revision_mismatch" {
		t.Fatalf("wrong revision error: %v", err)
	}
	if queuedCount(e) != 0 {
		t.Fatal("revision mismatch leaked queue")
	}
	for _, revision := range []string{"A", ""} {
		if _, err = e.AcquireForRevision(context.Background(), "a", ProviderCodex, "gpt-5", revision); !errors.Is(err, ErrAdmissionDenied) {
			t.Fatal("mismatch accepted")
		}
	}
	newRelease, err := e.AcquireForRevision(context.Background(), "a", ProviderCodex, "gpt-5", "B")
	if err != nil {
		t.Fatal(err)
	}
	oldRelease()
	oldRelease()
	if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); v.InFlight != 1 {
		t.Fatalf("old release changed new principal: %+v", v)
	}
	newRelease()
	if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); v.InFlight != 0 {
		t.Fatal("new release leaked")
	}
	for _, mode := range []Mode{ModeOff, ModeShadow} {
		cfg := e.Config()
		cfg.Mode = mode
		e.Configure(cfg)
		r, err := e.AcquireForRevision(context.Background(), "a", ProviderCodex, "gpt-5", "A")
		if err != nil {
			t.Fatal("non-enforce revision changed routing")
		}
		r()
	}
}

func TestConcurrentAdmissionAndRelease(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	e.Observe(observation("a", ProviderCodex, time.Now(), 1))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				r, err := e.Acquire(context.Background(), "a", ProviderCodex, "gpt-5")
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				r()
				r()
			}
		}()
	}
	wg.Wait()
	if v := e.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); v.InFlight != 0 || queuedCount(e) != 0 {
		t.Fatalf("lease leaked: %+v", v)
	}
}

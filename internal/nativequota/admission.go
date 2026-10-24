package nativequota

import (
	"context"
	"sync"
	"time"
)

type budgetKey struct{}
type waitBudget struct {
	mu      sync.Mutex
	spent   time.Duration
	last    time.Time
	active  int
	changed chan struct{}
}

func newWaitBudget() *waitBudget { return &waitBudget{changed: make(chan struct{})} }

// WithWaitBudget attaches one cumulative queue-wait budget to a logical request.
// Rewrapping preserves it. Time spent generating or streaming is never charged;
// concurrent waits share the remaining budget rather than each getting two seconds.
func WithWaitBudget(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(budgetKey{}).(*waitBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, newWaitBudget())
}
func (b *waitBudget) accrue(now time.Time) {
	if b.active > 0 {
		b.spent += now.Sub(b.last) * time.Duration(b.active)
	}
	b.last = now
}
func (b *waitBudget) signal() { close(b.changed); b.changed = make(chan struct{}) }
func (b *waitBudget) begin() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accrue(time.Now())
	b.active++
	b.signal()
}
func (b *waitBudget) end() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accrue(time.Now())
	b.active--
	b.signal()
}
func (b *waitBudget) remaining(limit time.Duration) (time.Duration, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accrue(time.Now())
	remaining := limit - b.spent
	if b.active > 1 {
		remaining /= time.Duration(b.active)
	}
	return remaining, b.changed
}

func noRelease() {}

// Acquire admits exactly one generation attempt. The returned release is
// idempotent and remains valid across Configure/SetAccounts. Callers must defer
// it, or retain it until a stream's terminal close. Cancellation releases queue
// slots, not an already acquired lease: the owner controls the stream lifetime.
func (e *Engine) Acquire(ctx context.Context, id, provider, model string) (func(), error) {
	return e.acquire(ctx, id, provider, model, nil)
}

// AcquireForRevision binds enforce-mode admission to the selected principal.
// The revision is checked under the lease mutex on every retry after queue wakes.
// Off/shadow remain no-ops. An empty supplied revision must match exactly.
func (e *Engine) AcquireForRevision(ctx context.Context, id, provider, model, revision string) (func(), error) {
	return e.acquire(ctx, id, provider, model, &revision)
}

func (e *Engine) acquire(ctx context.Context, id, provider, model string, revision *string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget, _ := ctx.Value(budgetKey{}).(*waitBudget)
	if budget == nil {
		budget = newWaitBudget()
	}
	queued := false
	defer func() {
		if queued {
			budget.end()
			e.mu.Lock()
			e.queued--
			e.notifyLocked()
			e.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		e.mu.Lock()
		if e.cfg.Mode != ModeEnforce || !e.cfg.covers(provider) {
			e.mu.Unlock()
			return noRelease, nil
		}
		if revision != nil {
			s := e.accounts[id]
			if s != nil && s.account.Revision != *revision {
				e.mu.Unlock()
				return nil, &AdmissionError{Reason: "revision_mismatch"}
			}
		}
		v := e.assessLocked(id, provider, model, now, false)
		if !v.Allowed {
			e.mu.Unlock()
			return nil, &AdmissionError{Reason: v.Reason}
		}
		s := e.accounts[id]
		if s == nil {
			e.mu.Unlock()
			return nil, &AdmissionError{Reason: "account_unknown"}
		}
		limit := e.cfg.MaxInFlight
		if v.State == "unknown" {
			limit = e.cfg.UnknownMaxInFlight
		}
		keys := make([]string, 0, 2)
		for _, i := range matchingPools(s.observation.Pools, provider, model) {
			keys = append(keys, s.observation.Pools[i].ID)
		}
		if len(keys) == 0 {
			keys = append(keys, "unknown")
		}
		capacity := s.inFlight < limit
		for _, key := range keys {
			if s.poolFlight[key] >= limit {
				capacity = false
			}
		}
		if queued {
			remaining, _ := budget.remaining(e.cfg.MaxWait)
			if remaining <= 0 {
				e.mu.Unlock()
				return nil, &AdmissionError{Reason: "wait_budget"}
			}
		}
		if capacity {
			s.inFlight++
			s.lastUsed = now
			for _, key := range keys {
				s.poolFlight[key]++
			}
			e.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					e.mu.Lock()
					s.inFlight--
					for _, key := range keys {
						s.poolFlight[key]--
						if s.poolFlight[key] == 0 {
							delete(s.poolFlight, key)
						}
					}
					e.notifyLocked()
					e.mu.Unlock()
				})
			}, nil
		}
		if !queued {
			if e.queued >= e.cfg.QueueLimit {
				e.mu.Unlock()
				return nil, &AdmissionError{Reason: "queue_full"}
			}
			queued = true
			e.queued++
			budget.begin()
		}
		remaining, budgetChanged := budget.remaining(e.cfg.MaxWait)
		changed := e.changed
		e.mu.Unlock()
		if remaining <= 0 {
			return nil, &AdmissionError{Reason: "wait_budget"}
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
			// Recompute: another simultaneous waiter may have left the shared
			// budget, extending this waiter's share without extending total wait.
		case <-changed:
			timer.Stop()
		case <-budgetChanged:
			timer.Stop()
		}
	}
}

package nativequota

import (
	"context"
	"sync"
	"time"
)

// Start runs one deadline scheduler and at most RefreshConcurrency transient
// fetch goroutines. There are no tickers or permanent per-account goroutines.
// Fetcher must honor context cancellation; Stop reports its caller's timeout if
// an injected fetcher refuses to finish. Quota collection never opens a stream.
func (e *Engine) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	if e.done != nil {
		select {
		case <-e.done:
		default:
			e.mu.Unlock()
			return
		}
	}
	if ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	e.runCtx, e.cancel, e.done = run, cancel, done
	e.mu.Unlock()
	go e.collect(run, done)
}
func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	e.wakeup()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *Engine) collectibleLocked(s *accountState) bool {
	return e.cfg.Mode != ModeOff && e.fetch != nil && !s.account.Disabled && e.cfg.covers(s.account.Provider)
}
func (e *Engine) nextDeadlineLocked(s *accountState, now time.Time) time.Time {
	interval := e.cfg.interval(s.account.Provider)
	// Idle accounts receive a slower read cadence, without losing reset wakeups.
	lastActive := s.lastUsed
	if lastActive.IsZero() {
		lastActive = s.registeredAt
	}
	if !lastActive.IsZero() && now.Sub(lastActive) >= 2*interval {
		interval = min(4*interval, 24*time.Hour)
	}
	base := s.lastAttempt
	if s.observation.ObservedAt.After(base) {
		base = s.observation.ObservedAt
	}
	if base.IsZero() {
		return now
	}
	due := base.Add(interval)
	for _, p := range s.observation.Pools {
		for _, w := range p.Windows {
			if w.Disabled {
				continue
			}
			observed := w.ObservedAt
			if observed.IsZero() {
				observed = s.observation.ObservedAt
			}
			if !observed.IsZero() && observed.Add(interval).Before(due) {
				due = observed.Add(interval)
			}
			if w.ResetAt.After(now) && w.ResetAt.Before(due) {
				due = w.ResetAt
			}
		}
	}
	floor := s.lastAttempt.Add(e.cfg.RefreshMinInterval)
	if floor.After(due) {
		due = floor
	}
	if s.backoffUntil.After(due) {
		due = s.backoffUntil
	}
	return due
}
func (e *Engine) requestLocked(s *accountState, now time.Time) bool {
	if !e.collectibleLocked(s) || s.requested || s.refreshing || now.Before(s.lastAttempt.Add(e.cfg.RefreshMinInterval)) || now.Before(s.backoffUntil) {
		return false
	}
	s.requested = true
	s.nextDue = now
	e.eventLocked(s.account.ID, s.account.Provider, "refresh_requested", "coalesced", now)
	e.wakeup()
	return true
}
func (e *Engine) RequestRefresh(id, reason string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.accounts[id]
	if s == nil {
		return false
	}
	// reason is intentionally not stored: management callers may supply secrets.
	return e.requestLocked(s, time.Now())
}

type collection struct {
	state   *accountState
	account Account
	started time.Time
	epoch   uint64
	version uint64
	ctx     context.Context
	cancel  context.CancelFunc
}

func (e *Engine) collect(ctx context.Context, done chan struct{}) {
	var workers sync.WaitGroup
	defer func() { workers.Wait(); close(done) }()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		e.mu.Lock()
		var jobs []collection
		var next time.Time
		if e.cfg.Mode != ModeOff && e.fetch != nil {
			for e.active < e.cfg.RefreshConcurrency {
				var candidate *accountState
				for _, s := range e.accounts {
					if !e.collectibleLocked(s) || s.refreshing || s.nextDue.After(now) {
						continue
					}
					if candidate == nil || s.nextDue.Before(candidate.nextDue) || s.nextDue.Equal(candidate.nextDue) && s.account.ID < candidate.account.ID {
						candidate = s
					}
				}
				if candidate == nil {
					break
				}
				candidate.refreshing = true
				candidate.requested = false
				candidate.lastAttempt = now
				candidate.nextDue = now.Add(e.cfg.RefreshMinInterval)
				e.active++
				jobCtx, cancel := context.WithCancel(ctx)
				candidate.refreshCancel = cancel
				jobs = append(jobs, collection{state: candidate, account: candidate.account, started: now, epoch: e.epoch, version: candidate.version, ctx: jobCtx, cancel: cancel})
			}
			// At capacity only a worker completion should wake the scheduler.
			if e.active < e.cfg.RefreshConcurrency {
				for _, s := range e.accounts {
					if e.collectibleLocked(s) && !s.refreshing && (next.IsZero() || s.nextDue.Before(next)) {
						next = s.nextDue
					}
				}
			}
		}
		e.mu.Unlock()
		for _, job := range jobs {
			workers.Add(1)
			go func(job collection) { defer workers.Done(); e.fetchOne(job.ctx, job) }(job)
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(max(time.Until(next), time.Millisecond))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-e.wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
func (e *Engine) fetchOne(ctx context.Context, job collection) {
	defer job.cancel()
	var ob Observation
	err := ctx.Err()
	if err == nil {
		ob, err = e.fetch(ctx, job.account)
	}
	ob.Pools = clonePools(ob.Pools)
	now := time.Now()
	if ob.AuthID == "" {
		ob.AuthID = job.account.ID
	}
	if ob.Provider == "" {
		ob.Provider = job.account.Provider
	}
	// Timestamp at read start, not completion: a slow request cannot overwrite
	// a passive observation delivered while it was in flight.
	if ob.ObservedAt.IsZero() || ob.ObservedAt.After(job.started) {
		ob.ObservedAt = job.started
	}
	for i := range ob.Pools {
		if ob.Pools[i].ObservedAt.After(job.started) {
			ob.Pools[i].ObservedAt = job.started
		}
		for j := range ob.Pools[i].Windows {
			if ob.Pools[i].Windows[j].ObservedAt.After(job.started) {
				ob.Pools[i].Windows[j].ObservedAt = job.started
			}
		}
	}
	prepared, valid := prepareObservation(ob, now)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active--
	job.state.refreshing = false
	job.state.refreshCancel = nil
	e.wakeup()
	if ctx.Err() != nil || e.epoch != job.epoch || e.accounts[job.account.ID] != job.state || !e.collectibleLocked(job.state) {
		return
	}
	s := job.state
	// Wall clocks can have equal ticks on Windows; sequence ordering protects
	// observations delivered during this read even if timestamps are identical.
	if s.version != job.version {
		s.nextDue = e.nextDeadlineLocked(s, now)
		return
	}
	if err == nil && valid && ob.AuthID == job.account.ID && ob.Provider == job.account.Provider {
		if !e.observeLocked(s, prepared, now) {
			s.nextDue = e.nextDeadlineLocked(s, now)
		}
		return
	}
	// Failed older reads are no stronger evidence than successful older reads.
	if s.observation.ObservedAt.After(job.started) {
		s.nextDue = e.nextDeadlineLocked(s, now)
		return
	}
	if job.started.After(s.invalidAt) {
		s.invalidAt = job.started
	}
	s.failures = min(s.failures+1, 10)
	backoff := e.cfg.RefreshMinInterval * time.Duration(1<<min(s.failures-1, 6))
	backoff = min(backoff, max(e.cfg.RefreshMinInterval, e.cfg.interval(s.account.Provider)))
	s.backoffUntil = now.Add(backoff)
	s.nextDue = s.backoffUntil
	e.eventLocked(s.account.ID, s.account.Provider, "refresh_failed", "unavailable", now)
	e.notifyLocked()
}

// RecordResult changes transient health evidence, never quota fractions or
// persistent auth fields. A 429 is a scoped rate limit, not proof of zero quota.
func (e *Engine) RecordResult(id, provider, model string, status int, success bool, retryAfter time.Duration) {
	e.RecordResultWithLatency(id, provider, model, status, success, retryAfter, 0)
}

// RecordResultWithLatency adds optional bounded latency/stability EWMA evidence.
// A zero latency is omitted, never interpreted as an instant response.
func (e *Engine) RecordResultWithLatency(id, provider, model string, status int, success bool, retryAfter, latency time.Duration) {
	e.recordResult(id, provider, model, nil, status, success, retryAfter, latency)
}

// RecordResultForRevision ignores feedback from an account's replaced principal.
func (e *Engine) RecordResultForRevision(id, provider, model, revision string, status int, success bool, retryAfter time.Duration) {
	e.RecordResultWithLatencyForRevision(id, provider, model, revision, status, success, retryAfter, 0)
}

// RecordResultWithLatencyForRevision checks principal identity before changing
// health, metrics, events, activity, or refresh scheduling. Empty matches exactly.
func (e *Engine) RecordResultWithLatencyForRevision(id, provider, model, revision string, status int, success bool, retryAfter, latency time.Duration) {
	e.recordResult(id, provider, model, &revision, status, success, retryAfter, latency)
}

func (e *Engine) recordResult(id, provider, model string, revision *string, status int, success bool, retryAfter, latency time.Duration) {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.accounts[id]
	if e.cfg.Mode == ModeOff || s == nil || s.account.Provider != provider || !e.cfg.covers(provider) || s.account.Disabled || revision != nil && s.account.Revision != *revision {
		return
	}
	s.lastUsed = now
	scopes := make([]string, 0, 2)
	// Status-only Codex feedback cannot prove account or pool scope. In
	// particular an opaque model-limit namespace must not poison sibling models.
	if provider != ProviderCodex {
		for _, i := range matchingPools(s.observation.Pools, provider, model) {
			scopes = append(scopes, "pool:"+s.observation.Pools[i].ID)
		}
	}
	if len(scopes) == 0 {
		if provider == ProviderAntigravity && canonicalAG(model) != "" {
			scopes = append(scopes, "pool:"+canonicalAG(model))
		} else if validModel(model) {
			scopes = append(scopes, "model:"+model)
		}
	}
	if provider != ProviderCodex && (status == 401 || status == 403) {
		scopes = []string{"account"}
	}
	for _, scope := range scopes {
		metric, exists := s.metrics[scope]
		if !exists && len(s.metrics) >= maxPools+1 {
			continue
		}
		failed := 0.0
		if !success {
			failed = 1
		}
		metric.failure = 0.8*metric.failure + 0.2*failed
		if latency > 0 {
			latency = min(latency, time.Minute)
			if metric.latency == 0 {
				metric.latency = latency
			} else {
				metric.latency = time.Duration(0.8*float64(metric.latency) + 0.2*float64(latency))
			}
		}
		s.metrics[scope] = metric
	}
	if success {
		// Success cannot clear a newer denial from a concurrent attempt; health
		// recovers via expiry or authoritative observations, not optimistic resets.
		return
	}
	kind := ""
	duration := retryAfter
	switch {
	case status == 429:
		kind = "rate_limit"
		if duration <= 0 {
			duration = 30 * time.Second
		}
	case status == 401 || status == 403:
		kind = "auth_failure"
		if duration <= 0 {
			duration = time.Minute
		}
	case status >= 500 && status <= 599 || status == 0:
		kind = "circuit"
		if duration <= 0 {
			duration = 15 * time.Second
		}
	default:
		return
	}
	duration = min(duration, 24*time.Hour)
	changed := false
	for _, scope := range scopes {
		h := s.health[scope]
		if now.Before(h.until) {
			continue
		}
		if len(s.health) >= maxPools+1 {
			if _, exists := s.health[scope]; !exists {
				continue
			}
		}
		if now.Sub(h.at) > time.Minute {
			h.failures = 0
		}
		h.failures++
		h.at, h.kind = now, kind
		if kind != "circuit" || h.failures >= 3 {
			h.until = now.Add(duration)
			changed = true
		}
		s.health[scope] = h
	}
	// Expired unassociated model evidence cannot grow without bound.
	for scope, h := range s.health {
		if !h.until.IsZero() && now.Sub(h.until) > time.Minute {
			delete(s.health, scope)
		}
	}
	if changed {
		e.eventLocked(id, provider, "health", kind, now)
		e.notifyLocked()
	}
	if changed && e.collectibleLocked(s) && !s.requested && !s.refreshing {
		// One deferred read at the earliest permitted deadline, even if the
		// denial arrived during cooldown. Repeated denials do not extend it.
		s.requested = true
		s.nextDue = now
		if floor := s.lastAttempt.Add(e.cfg.RefreshMinInterval); floor.After(s.nextDue) {
			s.nextDue = floor
		}
		if s.backoffUntil.After(s.nextDue) {
			s.nextDue = s.backoffUntil
		}
		e.wakeup()
	}
}

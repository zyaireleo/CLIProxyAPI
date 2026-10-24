package nativequota

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"
)

type healthEvidence struct {
	at       time.Time
	until    time.Time
	kind     string
	failures int
}

type healthMetric struct {
	failure float64
	latency time.Duration
}

type accountState struct {
	account       Account
	observation   Observation
	version       uint64
	invalidAt     time.Time
	lastAttempt   time.Time
	lastUsed      time.Time
	registeredAt  time.Time
	nextDue       time.Time
	backoffUntil  time.Time
	failures      int
	refreshing    bool
	refreshCancel context.CancelFunc
	requested     bool
	inFlight      int
	poolFlight    map[string]int
	health        map[string]healthEvidence
	metrics       map[string]healthMetric
}

// Engine owns no credentials and launches no work until Start. All state is
// protected by mu; fetch callbacks and all waits execute outside that mutex.
type Engine struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	cfg       Config
	fetch     Fetcher
	accounts  map[string]*accountState
	events    []Event
	eventPos  int
	wake      chan struct{}
	changed   chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	runCtx    context.Context
	epoch     uint64
	active    int
	queued    int
}

// New falls closed to off for invalid input because the agreed constructor has
// no error return. Owners should Validate or Configure to receive validation errors.
func New(cfg Config, fetch Fetcher) *Engine {
	if cfg.Validate() != nil {
		cfg = DefaultConfig()
	}
	return &Engine{
		cfg: cfg.normalized(), fetch: fetch, accounts: make(map[string]*accountState),
		wake: make(chan struct{}, 1), changed: make(chan struct{}),
	}
}
func (e *Engine) Configure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg = cfg.normalized()
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	// Management apply and watcher reload may publish the same effective config.
	// Preserve in-flight reads and their deadlines when nothing has changed.
	if e.cfg.Mode == cfg.Mode && slices.Equal(e.cfg.Providers, cfg.Providers) &&
		e.cfg.RefreshConcurrency == cfg.RefreshConcurrency &&
		e.cfg.AntigravityInterval == cfg.AntigravityInterval &&
		e.cfg.CodexInterval == cfg.CodexInterval &&
		e.cfg.RefreshMinInterval == cfg.RefreshMinInterval &&
		e.cfg.MaxWait == cfg.MaxWait && e.cfg.MaxInFlight == cfg.MaxInFlight &&
		e.cfg.UnknownMaxInFlight == cfg.UnknownMaxInFlight && e.cfg.QueueLimit == cfg.QueueLimit {
		e.mu.Unlock()
		return nil
	}
	e.cfg = cfg
	e.epoch++
	for _, s := range e.accounts {
		if s.refreshCancel != nil {
			s.refreshCancel()
		}
		if s.lastAttempt.IsZero() {
			s.nextDue = time.Time{}
		} else {
			s.nextDue = e.nextDeadlineLocked(s, time.Now())
		}
	}
	e.notifyLocked()
	e.mu.Unlock()
	e.wakeup()
	return nil
}
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cfg
	c.Providers = append([]string{}, c.Providers...)
	return c
}
func (e *Engine) wakeup() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) notifyLocked() { close(e.changed); e.changed = make(chan struct{}) }
func (e *Engine) eventLocked(id, provider, kind, reason string, now time.Time) {
	v := Event{At: now.UTC(), AuthID: id, Provider: provider, Kind: kind, Reason: reason}
	if len(e.events) < maxEvents {
		e.events = append(e.events, v)
		return
	}
	e.events[e.eventPos] = v
	e.eventPos = (e.eventPos + 1) % maxEvents
}
func (e *Engine) Events() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, 0, len(e.events))
	out = append(out, e.events[e.eventPos:]...)
	out = append(out, e.events[:e.eventPos]...)
	return out
}

func (e *Engine) SetAccounts(accounts []Account) {
	e.mu.Lock()
	next := make(map[string]*accountState, len(accounts))
	for _, a := range accounts {
		if !validID(a.ID) || !supportedProvider(a.Provider) {
			continue
		}
		s := e.accounts[a.ID]
		if s == nil || s.account.Provider != a.Provider || s.account.Revision != a.Revision {
			s = &accountState{registeredAt: time.Now(), poolFlight: make(map[string]int), health: make(map[string]healthEvidence), metrics: make(map[string]healthMetric)}
		}
		s.account = a
		next[a.ID] = s
	}
	for id, old := range e.accounts {
		if (next[id] != old || old.account.Disabled) && old.refreshCancel != nil {
			old.refreshCancel()
		}
	}
	e.accounts = next
	e.notifyLocked()
	e.mu.Unlock()
	e.wakeup()
}

func prepareObservation(ob Observation, now time.Time) (Observation, bool) {
	if !validID(ob.AuthID) || !supportedProvider(ob.Provider) || ob.ObservedAt.IsZero() || ob.ObservedAt.After(now.Add(time.Second)) || len(ob.Pools) == 0 || len(ob.Pools) > maxPools {
		return Observation{}, false
	}
	ob.Source = safeSource(ob.Source)
	ob.Pools = clonePools(ob.Pools)
	seen := make(map[string]bool)
	evidence := false
	for i := range ob.Pools {
		p := &ob.Pools[i]
		if !validModel(p.ID) || seen[p.ID] || len(p.Models) > maxModels || len(p.Windows) > maxWindows {
			return Observation{}, false
		}
		seen[p.ID] = true
		if ob.Provider == ProviderAntigravity && p.ID != "gemini" && p.ID != "claude_gpt" {
			return Observation{}, false
		}
		if ob.Provider == ProviderCodex && p.ID != "codex" && !stringsHasCodexScope(p.ID) {
			return Observation{}, false
		}
		models := make(map[string]bool)
		for _, m := range p.Models {
			if !validModel(m) || models[m] || (ob.Provider == ProviderAntigravity && canonicalAG(m) != p.ID) {
				return Observation{}, false
			}
			models[m] = true
		}
		if p.ObservedAt.IsZero() {
			p.ObservedAt = ob.ObservedAt
		}
		if p.ObservedAt.After(ob.ObservedAt) {
			return Observation{}, false
		}
		if p.Source == "" {
			p.Source = ob.Source
		} else {
			p.Source = safeSource(p.Source)
		}
		evidence = evidence || p.Blocked
		windows := make(map[string]bool)
		for j := range p.Windows {
			w := &p.Windows[j]
			if !validModel(w.ID) || windows[w.ID] || w.DurationSeconds < 0 || w.DurationSeconds > 366*24*3600 || (w.Remaining != nil && !validFraction(*w.Remaining)) {
				return Observation{}, false
			}
			windows[w.ID] = true
			if w.ObservedAt.IsZero() {
				w.ObservedAt = p.ObservedAt
			}
			if w.ObservedAt.After(ob.ObservedAt) {
				return Observation{}, false
			}
			if w.Source == "" {
				w.Source = ob.Source
			} else {
				w.Source = safeSource(w.Source)
			}
			evidence = evidence || w.Remaining != nil || w.Blocked
		}
	}
	return ob, evidence
}
func stringsHasCodexScope(id string) bool {
	return len(id) > len("codex:") && id[:len("codex:")] == "codex:"
}

// Observe rejects ties as well as older observations; a late read cannot replace
// newer passive evidence. All caller-owned slices and numeric pointers are copied.
func (e *Engine) Observe(ob Observation) bool {
	return e.observe(ob, nil)
}

// ObserveForRevision rejects evidence from a different stable principal even if
// its AuthID and timestamp are current. Empty revision is matched exactly.
func (e *Engine) ObserveForRevision(ob Observation, revision string) bool {
	return e.observe(ob, &revision)
}

func (e *Engine) observe(ob Observation, revision *string) bool {
	now := time.Now()
	prepared, ok := prepareObservation(ob, now)
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Mode == ModeOff {
		return false
	}
	s := e.accounts[ob.AuthID]
	if s == nil || s.account.Provider != ob.Provider || !e.cfg.covers(ob.Provider) || revision != nil && s.account.Revision != *revision {
		return false
	}
	return e.observeLocked(s, prepared, now)
}
func (e *Engine) observeLocked(s *accountState, ob Observation, now time.Time) bool {
	if !s.observation.ObservedAt.IsZero() && !ob.ObservedAt.After(s.observation.ObservedAt) {
		return false
	}
	if !s.invalidAt.IsZero() && !ob.ObservedAt.After(s.invalidAt) {
		return false
	}
	if ob.Partial && len(s.observation.Pools) > 0 {
		pools := clonePools(s.observation.Pools)
		for _, incoming := range ob.Pools {
			idx := -1
			for i := range pools {
				if pools[i].ID == incoming.ID {
					idx = i
					break
				}
			}
			if idx < 0 {
				pools = append(pools, incoming)
				continue
			}
			old := pools[idx]
			// Model-only AG fallback cannot replace authoritative summary windows.
			if ob.Provider == ProviderAntigravity && hasAGSummary(old) && !hasAGSummary(incoming) {
				continue
			}
			// Omitted metadata is not a new statement about the old scope.
			if !incoming.BlockedKnown && !incoming.Blocked {
				incoming.Blocked, incoming.BlockedKnown = old.Blocked, old.BlockedKnown
				if old.Blocked {
					incoming.ObservedAt = old.ObservedAt
				}
			}
			if len(incoming.Models) == 0 {
				incoming.Models = old.Models
			}
			windows := old.Windows
			for _, w := range incoming.Windows {
				found := false
				for i := range windows {
					if windows[i].ID == w.ID {
						windows[i] = w
						found = true
						break
					}
				}
				if !found {
					windows = append(windows, w)
				}
			}
			incoming.Windows = windows
			pools[idx] = incoming
		}
		if len(pools) > maxPools {
			return false
		}
		for _, p := range pools {
			if len(p.Windows) > maxWindows {
				return false
			}
		}
		ob.Pools = pools
	}
	// Missing fields are not counterevidence to an explicit, unexpired zero.
	// Only a new numeric balance, explicit non-applicability, or reset clears it.
	for _, oldPool := range s.observation.Pools {
		for _, oldWindow := range oldPool.Windows {
			if !hardZero(oldWindow, now) {
				continue
			}
			pi := -1
			for i := range ob.Pools {
				if ob.Pools[i].ID == oldPool.ID {
					pi = i
					break
				}
			}
			if pi < 0 {
				p := oldPool
				p.Windows = nil
				ob.Pools = append(ob.Pools, p)
				pi = len(ob.Pools) - 1
			}
			wi := -1
			for i := range ob.Pools[pi].Windows {
				if ob.Pools[pi].Windows[i].ID == oldWindow.ID {
					wi = i
					break
				}
			}
			if wi < 0 {
				ob.Pools[pi].Windows = append(ob.Pools[pi].Windows, oldWindow)
			} else if w := ob.Pools[pi].Windows[wi]; w.Remaining == nil && !w.Disabled {
				ob.Pools[pi].Windows[wi] = oldWindow
			}
		}
	}
	if len(ob.Pools) > maxPools {
		return false
	}
	for _, p := range ob.Pools {
		if len(p.Windows) > maxWindows {
			return false
		}
	}
	s.observation = ob
	s.version++
	// invalidAt is intentionally kept: a partial observation cannot resurrect
	// other windows invalidated by a newer failed collector.
	// Quota reads are not generation successes. They must not clear a recent
	// scoped rate limit/circuit merely because a provider reports positive quota.
	s.failures = 0
	s.backoffUntil = time.Time{}
	s.nextDue = e.nextDeadlineLocked(s, now)
	e.eventLocked(s.account.ID, s.account.Provider, "observation", ob.Source, now)
	e.notifyLocked()
	e.wakeup()
	return true
}
func hardZero(w Window, now time.Time) bool {
	return !w.Disabled && w.Remaining != nil && *w.Remaining == 0 && w.ResetAt.After(now) && w.ResetAt.Sub(w.ObservedAt) <= 32*24*time.Hour
}
func hasAGSummary(p Pool) bool {
	for _, w := range p.Windows {
		if agBucketGroup(w.ID) != "" && (w.ID != "gemini" && w.ID != "claude_gpt") && (stringsHasWindowSuffix(w.ID)) {
			return true
		}
	}
	return false
}
func stringsHasWindowSuffix(id string) bool {
	for _, suffix := range []string{"-weekly", "-5h", "-daily", "-monthly"} {
		if len(id) >= len(suffix) && id[len(id)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func (e *Engine) Assess(id, provider, model string, now time.Time) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.assessLocked(id, provider, model, now, true)
}

// AssessSummary is the selection hot path: identical eligibility without copying
// display-only pool/window details. AtCapacity is advisory, not quota exhaustion.
func (e *Engine) AssessSummary(id, provider, model string, now time.Time) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.assessLocked(id, provider, model, now, false)
}
func (e *Engine) assessLocked(id, provider, model string, now time.Time, copyPools bool) View {
	v := View{AuthID: id, Provider: provider, Mode: e.cfg.Mode, State: "unknown", Reason: "no_observation", Weight: 0.1, Allowed: true}
	s := e.accounts[id]
	if s != nil {
		v.Disabled = s.account.Disabled
		v.InFlight = s.inFlight
	}
	if e.cfg.Mode == ModeOff {
		v.State = "off"
		v.Reason = "mode_off"
		v.Weight = 1
		return v
	}
	if s == nil {
		v.Reason = "account_unknown"
		v.Allowed = e.cfg.Mode != ModeEnforce
		return v
	}
	if s.account.Disabled {
		v.State = "disabled"
		v.Reason = "manual_disabled"
		if s.account.Provider == ProviderAntigravity && s.account.RejectionReason == "TOS_VIOLATION" {
			v.Reason = "upstream_tos_violation"
		}
		v.Allowed = false
		v.Weight = 0
		return v
	}
	if provider != s.account.Provider {
		v.Reason = "provider_mismatch"
		v.Allowed = e.cfg.Mode != ModeEnforce
		return v
	}
	if !e.cfg.covers(provider) {
		v.Reason = "provider_uncovered"
		v.Weight = 1
		return v
	}
	ob := s.observation
	v.RefreshFailed = !s.invalidAt.IsZero() && !ob.ObservedAt.After(s.invalidAt)
	v.Stale = !freshAt(ob.ObservedAt, s.invalidAt, now, e.cfg.interval(provider))
	v.ObservedAt = ob.ObservedAt
	if !ob.ObservedAt.IsZero() {
		v.ExpiresAt = ob.ObservedAt.Add(3 * e.cfg.interval(provider))
	}
	indices := matchingPools(ob.Pools, provider, model)
	if model != "" && len(indices) == 0 && len(ob.Pools) > 0 {
		v.Reason = "model_unknown"
	}
	if copyPools {
		selected := make([]Pool, 0, len(indices))
		for _, i := range indices {
			selected = append(selected, ob.Pools[i])
		}
		v.Pools = clonePools(selected)
	}
	unknown := len(indices) == 0
	// A short window or an unclassified model fallback cannot establish the
	// account's long-period quota. Specific Codex limits supplement its base.
	longWindowKnown := false
	blocked, exhausted, stale := false, false, false
	weight := 1.0
	for _, i := range indices {
		p := ob.Pools[i]
		freshPool := freshAt(p.ObservedAt, s.invalidAt, now, e.cfg.interval(provider))
		if p.Blocked && freshPool && !poolResetPassed(p, now) {
			blocked = true
		}
		if len(p.Windows) == 0 && !p.Blocked {
			unknown = true
		}
		activeWindows := 0
		for _, w := range p.Windows {
			if w.Disabled {
				continue
			}
			activeWindows++
			if (provider == ProviderAntigravity || p.ID == "codex") && (w.ID == "weekly" || w.ID == "monthly" || w.DurationSeconds >= 7*24*3600 && w.DurationSeconds <= 32*24*3600) {
				longWindowKnown = true
			}
			expires := w.ObservedAt.Add(3 * e.cfg.interval(provider))
			zeroUntilReset := hardZero(w, now)
			if !s.invalidAt.IsZero() && !w.ObservedAt.After(s.invalidAt) {
				v.RefreshFailed = true
			}
			if !freshAt(w.ObservedAt, s.invalidAt, now, e.cfg.interval(provider)) {
				stale = true
			}
			if zeroUntilReset {
				expires = w.ResetAt
			}
			if !w.ResetAt.IsZero() && w.ResetAt.Before(expires) {
				expires = w.ResetAt
			}
			if v.ExpiresAt.IsZero() || expires.Before(v.ExpiresAt) {
				v.ExpiresAt = expires
			}
			if !zeroUntilReset && !freshAt(w.ObservedAt, s.invalidAt, now, e.cfg.interval(provider)) || !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
				unknown = true
				stale = true
				continue
			}
			if w.Blocked {
				blocked = true
			}
			if w.Remaining == nil {
				unknown = true
				continue
			}
			if *w.Remaining == 0 {
				exhausted = true
			}
			weight = min(weight, *w.Remaining)
		}
		if activeWindows == 0 && !p.Blocked {
			unknown = true
		}
		if p.Blocked && !freshPool {
			unknown = true
			stale = true
		}
	}
	if !longWindowKnown {
		unknown = true
		if len(indices) > 0 {
			v.Reason = "incomplete_windows"
		}
	}
	for scope, h := range s.health {
		if !now.Before(h.until) {
			continue
		}
		matches := scope == "account" || model != "" && scope == "model:"+model || provider == ProviderAntigravity && canonicalAG(model) != "" && scope == "pool:"+canonicalAG(model)
		for _, i := range indices {
			if scope == "pool:"+ob.Pools[i].ID {
				matches = true
				break
			}
		}
		if matches {
			blocked = true
			v.Reason = h.kind
		}
	}
	switch {
	case blocked:
		v.State = "blocked"
		if v.Reason != "rate_limit" && v.Reason != "circuit" && v.Reason != "auth_failure" {
			v.Reason = "provider_blocked"
		}
		v.Weight = 0
	case exhausted:
		v.State = "exhausted"
		v.Reason = "quota_exhausted"
		v.Weight = 0
	case unknown:
		if stale || !s.invalidAt.IsZero() && !ob.ObservedAt.After(s.invalidAt) {
			v.Reason = "stale"
		}
		if !s.invalidAt.IsZero() && !ob.ObservedAt.After(s.invalidAt) {
			v.Reason = "refresh_failed"
		}
		v.Weight = 0.1
	default:
		v.State = "ready"
		v.Reason = "quota_available"
		v.Weight = 0.25 + 0.75*weight
	}
	limit := e.cfg.MaxInFlight
	if v.State == "unknown" {
		limit = e.cfg.UnknownMaxInFlight
	}
	v.AtCapacity = s.inFlight >= limit
	for _, i := range indices {
		if s.poolFlight[ob.Pools[i].ID] >= limit {
			v.AtCapacity = true
		}
	}
	v.Stale = v.Stale || stale
	stability, latency := 1.0, 1.0
	for scope, metric := range s.metrics {
		matches := scope == "account" || scope == "model:"+model
		for _, i := range indices {
			if scope == "pool:"+ob.Pools[i].ID {
				matches = true
				break
			}
		}
		if matches {
			stability = min(stability, 1/(1+4*metric.failure))
			latency = min(latency, max(0.25, 1/(1+metric.latency.Seconds())))
		}
	}
	v.Weight *= stability * latency
	v.Weight /= float64(1 + s.inFlight)
	v.Allowed = e.cfg.Mode != ModeEnforce || !blocked && !exhausted
	return v
}
func freshAt(at, invalid, now time.Time, interval time.Duration) bool {
	return !at.IsZero() && (invalid.IsZero() || at.After(invalid)) && now.Before(at.Add(3*interval))
}
func poolResetPassed(p Pool, now time.Time) bool {
	if len(p.Windows) == 0 {
		return false
	}
	for _, w := range p.Windows {
		if w.ResetAt.IsZero() || now.Before(w.ResetAt) {
			return false
		}
	}
	return true
}
func (e *Engine) Views(now time.Time) []View {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.accounts))
	for id := range e.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]View, 0, len(ids))
	for _, id := range ids {
		out = append(out, e.assessLocked(id, e.accounts[id].account.Provider, "", now, true))
	}
	return out
}

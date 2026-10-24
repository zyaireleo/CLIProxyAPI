package auth

import (
	"context"
	"math"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
)

// auditNativeQuotaSelection evaluates a deterministic shadow proposal without
// invoking any selector, refreshing affinity, advancing rotation, or copying auths.
func (m *Manager) auditNativeQuotaSelection(ctx context.Context, selected *Auth, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) {
	if selected == nil {
		return
	}
	e := m.nativeQuotaFor(selected.Provider, false)
	if e == nil {
		return
	}
	if e.Config().Mode != nativequota.ModeShadow {
		e.RecordDecision(selected.ID, selected.ID, selected.Provider, "same")
		return
	}
	now := time.Now()
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	reg := registry.GetGlobalRegistry()
	var best *Auth
	var bestView nativequota.View
	m.mu.RLock()
	for _, a := range m.auths {
		if a == nil || a.Disabled || a.Status == StatusDisabled || !strings.EqualFold(a.Provider, selected.Provider) || !eligibility.allows(a) {
			continue
		}
		if pinned != "" && a.ID != pinned {
			continue
		}
		if _, used := tried[a.ID]; used {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(reg, a, model) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now); blocked {
			continue
		}
		v, covered := m.nativeQuotaView(a, model, now)
		if !covered || v.State == "blocked" || v.State == "exhausted" || v.Disabled {
			continue
		}
		if best == nil || authPriority(a) > authPriority(best) || (authPriority(a) == authPriority(best) && nativeQuotaProposalBetter(a, v, best, bestView)) {
			best, bestView = a, v
		}
	}
	proposed := ""
	if best != nil {
		proposed = best.ID
	}
	m.mu.RUnlock()
	reason := "quota"
	if proposed == selected.ID {
		reason = "same"
	} else if proposed == "" {
		reason = "no_candidate"
	}
	e.RecordDecision(selected.ID, proposed, selected.Provider, reason)
}

func nativeQuotaProposalBetter(a *Auth, v nativequota.View, b *Auth, other nativequota.View) bool {
	if (v.State == "ready") != (other.State == "ready") {
		return v.State == "ready"
	}
	if v.AtCapacity != other.AtCapacity {
		return !v.AtCapacity
	}
	return v.Weight > other.Weight || (v.Weight == other.Weight && a.ID < b.ID)
}

type nativeQuotaSelectionContextKey struct{}
type nativeQuotaSelection struct {
	manager *Manager
	model   string
}

func (m *Manager) nativeQuotaView(a *Auth, model string, now time.Time) (nativequota.View, bool) {
	if a == nil || a.AuthKind() == AuthKindAPIKey {
		return nativequota.View{}, false
	}
	e := m.nativeQuotaFor(a.Provider, false)
	if e == nil {
		return nativequota.View{}, false
	}
	return e.AssessSummary(a.ID, a.Provider, m.selectionModelKeyForAuth(a, model), now), true
}

func (m *Manager) nativeQuotaEligible(a *Auth, model string, now time.Time) bool {
	v, covered := m.nativeQuotaView(a, model, now)
	// Shadow computes the same evidence, but cannot filter a live candidate.
	return !covered || v.Mode != nativequota.ModeEnforce || v.Allowed
}

func (m *Manager) nativeQuotaCandidates(auths []*Auth, model string, now time.Time) ([]*Auth, bool) {
	enforced := false
	for _, a := range auths {
		if a != nil && a.AuthKind() != AuthKindAPIKey && m.nativeQuotaFor(a.Provider, true) != nil {
			enforced = true
			break
		}
	}
	if !enforced {
		return auths, false
	}
	// Native eligibility has already run before this priority crop. Affinity may
	// only reuse a credential in the highest eligible manual tier.
	tier := highestPriorityAuths(auths)
	ready := make([]*Auth, 0, len(tier))
	for _, a := range tier {
		v, covered := m.nativeQuotaView(a, model, now)
		if !covered || v.Mode != nativequota.ModeEnforce || v.State == "ready" {
			ready = append(ready, a)
		}
	}
	if len(ready) > 0 {
		tier = ready
	}
	free := make([]*Auth, 0, len(tier))
	for _, a := range tier {
		v, covered := m.nativeQuotaView(a, model, now)
		if !covered || v.Mode != nativequota.ModeEnforce || !v.AtCapacity {
			free = append(free, a)
		}
	}
	if len(free) > 0 {
		return free, true
	}
	return tier, true
}

func (m *Manager) nativeQuotaSelectorContext(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := m.nativeQuota.Load(); e == nil || e.Config().Mode != nativequota.ModeEnforce {
		return ctx
	}
	return context.WithValue(ctx, nativeQuotaSelectionContextKey{}, nativeQuotaSelection{manager: m, model: model})
}

// pickNativeQuotaWeighted runs only after ordinary eligibility and manual tier
// cropping, including the fallback of the existing affinity selector. It never
// rewrites a credential's persisted priority, weight, disabled flag, or cooldown.
func pickNativeQuotaWeighted(ctx context.Context, auths []*Auth, configuredWeight bool) (*Auth, bool) {
	if ctx == nil {
		return nil, false
	}
	selection, ok := ctx.Value(nativeQuotaSelectionContextKey{}).(nativeQuotaSelection)
	if !ok || selection.manager == nil {
		return nil, false
	}
	auths, enforced := selection.manager.nativeQuotaCandidates(auths, selection.model, time.Now())
	if !enforced || len(auths) == 0 {
		return nil, false
	}
	weights := make(map[string]int64, len(auths))
	for _, a := range auths {
		weight := 1.0
		if v, covered := selection.manager.nativeQuotaView(a, selection.model, time.Now()); covered && v.Mode == nativequota.ModeEnforce {
			if !v.Allowed {
				continue
			}
			// Engine Weight already includes the current in-flight load.
			weight = v.Weight
		}
		if configuredWeight {
			weight *= float64(authWeight(a))
		}
		if weight <= 0 {
			continue
		}
		// Quantization is only a scheduling weight, never a quota judgment.
		weights[a.ID] = max(1, int64(math.Min(weight*1000000, 1000000000)))
	}
	m := selection.manager
	m.nativeQuotaSelectionMu.Lock()
	defer m.nativeQuotaSelectionMu.Unlock()
	key := canonicalModelKey(selection.model)
	if m.nativeQuotaSelections == nil || len(m.nativeQuotaSelections) >= 4096 {
		m.nativeQuotaSelections = make(map[string]*smoothWeightedState)
	}
	state := m.nativeQuotaSelections[key]
	if state == nil {
		state = &smoothWeightedState{current: make(map[string]int64)}
		m.nativeQuotaSelections[key] = state
	}
	// Dynamic load changes must not reset accrued credits: otherwise a busy
	// credential can continually restart the round and starve its siblings.
	state.weights = weights
	state.pruneStale(weights)
	var picked *Auth
	var pickedCurrent, total int64
	for _, a := range auths {
		weight := weights[a.ID]
		if weight <= 0 {
			continue
		}
		state.current[a.ID] = saturatingAddInt64(state.current[a.ID], weight)
		total = saturatingAddInt64(total, weight)
		if picked == nil || state.current[a.ID] > pickedCurrent || (state.current[a.ID] == pickedCurrent && a.ID < picked.ID) {
			picked, pickedCurrent = a, state.current[a.ID]
		}
	}
	if picked != nil {
		state.current[picked.ID] = saturatingAddInt64(state.current[picked.ID], -total)
	}
	return picked, true
}

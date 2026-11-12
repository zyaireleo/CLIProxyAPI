package auth

import (
	"context"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (m *Manager) withAntigravity429Guard(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) context.Context {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || !cfg.Antigravity429Guard || m.HomeEnabled() || opts.Headers.Get(cliproxyexecutor.GuardContractHeader) != "v1" {
		return ctx
	}
	// This contract is accepted only on a dedicated, gateway-authenticated CPA
	// listener. Public headers are stripped and recreated by the gateway.
	if !cliproxyexecutor.IsAntigravityGuardModel(req.Model) {
		return ctx
	}
	for _, provider := range providers {
		if strings.EqualFold(provider, "antigravity") {
			return cliproxyexecutor.WithAntigravityGuardTrace(ctx, opts.Headers.Get("X-Sub2API-Trace-ID"))
		}
	}
	return ctx
}

func isGuardNeutralClass(class string) bool {
	switch class {
	case cliproxyexecutor.GuardClassAmbiguous429, cliproxyexecutor.GuardClassPolicy, cliproxyexecutor.GuardClassBudget, "upstream_rate_limited":
		return true
	}
	return false
}

func annotateAntigravityGuardResult(ctx context.Context, result Result) Result {
	guard := cliproxyexecutor.AntigravityGuardFromContext(ctx)
	if guard == nil || !strings.EqualFold(result.Provider, "antigravity") || !cliproxyexecutor.IsAntigravityGuardModel(result.Model) {
		return result
	}
	result.AttemptsUsed = guard.Attempts()
	result.RetryExhausted = result.AttemptsUsed >= 2
	if result.Error != nil && result.FailureClass == "" {
		result.FailureClass = result.Error.Code
	}
	if guard.PolicyRejected() {
		result.FailureClass = cliproxyexecutor.GuardClassPolicy
	}
	if isGuardNeutralClass(result.FailureClass) {
		result.HealthEffect = cliproxyexecutor.GuardHealthNeutral
	}
	return result
}

func (a *Auth) observeGuardHealth(result Result, model string, now time.Time) {
	key := canonicalModelLookupKey(model)
	if result.HealthEffect == cliproxyexecutor.GuardHealthNeutral {
		a.NeutralResults++
		if key != "" && (result.FailureClass == cliproxyexecutor.GuardClassAmbiguous429 || result.FailureClass == "upstream_rate_limited") {
			if a.AdmissionBackoffs == nil {
				a.AdmissionBackoffs = make(map[string]AdmissionBackoff)
			}
			state := a.AdmissionBackoffs[key].afterFailure(now, result.ExecutionSeq, result.RetryAfter)
			a.AdmissionBackoffs[key] = state
		}
		return
	}
	if result.Success {
		a.HealthSuccess++
	} else {
		a.HealthFailed++
	}
	// Reuse the same bucket layout while keeping raw and health denominators separate.
	tmp := Auth{recentRequests: a.recentHealthRequests}
	tmp.recordRecentRequest(now, result.Success)
	a.recentHealthRequests = tmp.recentRequests
	if result.Success && result.CompletedResponse && !result.SkipQuotaObservation {
		if state, ok := a.AdmissionBackoffs[key]; ok && result.ExecutionSeq > state.Sequence {
			delete(a.AdmissionBackoffs, key)
		}
	}
}

func (a *Auth) RecentHealthRequestsSnapshot(now time.Time) []RecentRequestBucket {
	if a == nil {
		return nil
	}
	tmp := Auth{recentRequests: a.recentHealthRequests}
	return tmp.RecentRequestsSnapshot(now)
}

// Fail closed if a gateway enables the contract before the matching standalone
// CPA configuration is active. Unsupported provider mixes cannot reset a budget.
func (m *Manager) antigravityGuardContractError(ctx context.Context, providers []string, opts cliproxyexecutor.Options) error {
	if opts.Headers.Get(cliproxyexecutor.GuardContractHeader) != "v1" {
		return nil
	}
	valid := cliproxyexecutor.AntigravityGuardFromContext(ctx) != nil && !m.HomeEnabled()
	for _, provider := range providers {
		valid = valid && strings.EqualFold(provider, "antigravity")
	}
	if !valid {
		return &Error{Code: "upstream_retry_contract_unavailable", Message: "CPA retry isolation contract is unavailable for this route", HTTPStatus: 503}
	}
	return nil
}

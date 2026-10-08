package auth

import (
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// claimImageRecovery allows one caller to test an expired image-model cooldown.
// The lease lasts through MarkResult, so siblings cannot observe stale availability.
func (m *Manager) claimImageRecovery(auth *Auth, model string) (func(), bool) {
	noop := func() {}
	if auth == nil || auth.Provider != "antigravity" || !cliproxyexecutor.FlashImageModel(model) {
		return noop, true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[auth.ID]
	if current == nil {
		return noop, true
	}
	state := existingModelState(current, canonicalModelKey(model))
	if state == nil || state.LastError == nil {
		return noop, true
	}
	if state.NextRetryAfter.After(time.Now()) {
		return noop, false
	}
	key := auth.ID + "\x00gemini-3.1-flash-image"
	if _, loaded := m.imageRecoveryLeases.LoadOrStore(key, struct{}{}); loaded {
		return noop, false
	}
	return func() { m.imageRecoveryLeases.Delete(key) }, true
}

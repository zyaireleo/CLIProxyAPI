package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
)

const nativeQuotaTOSStatusMessage = "disabled: antigravity TOS_VIOLATION"

// applyNativeQuotaRejection patches the current credential, not the snapshot
// used for the HTTP request. Token rotation is allowed; principal replacement
// and unregister/re-register are not. Nothing is published before a durable save.
func (m *Manager) applyNativeQuotaRejection(ctx context.Context, expected *Auth, authorization nativequota.RejectionAuthorization) error {
	// StopNativeQuota holds this mutex while waiting for collectors. Never wait
	// for it from a collector; leave the failed read pending for a later retry.
	if !m.nativeQuotaMu.TryLock() {
		return errors.New("native-quota: rejection update busy; retry pending")
	}
	var installed *Auth
	err := authorization.Apply(func() error {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		if m.nativeQuotaFor("antigravity", true) == nil {
			return errors.New("native-quota: rejection authorization changed")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.auths[expected.ID]
		if current == nil || !strings.EqualFold(current.Provider, "antigravity") ||
			current.RegistrationEpoch != expected.RegistrationEpoch ||
			nativeQuotaAccount(current).Revision != nativeQuotaAccount(expected).Revision {
			return errors.New("native-quota: rejection account changed")
		}
		if current.Disabled || current.Status == StatusDisabled {
			return nil
		}
		if current.AuthKind() == AuthKindAPIKey || IsConfigAPIKeyAuth(current) || IsPluginVirtualAuth(current) ||
			strings.EqualFold(strings.TrimSpace(current.Attributes["runtime_only"]), "true") ||
			m.store == nil || current.Metadata == nil || shouldSkipPersist(ctx) {
			return errors.New("native-quota: rejection persistence unavailable")
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		updated := current.Clone()
		updated.Disabled = true
		updated.Status = StatusDisabled
		updated.StatusMessage = nativeQuotaTOSStatusMessage
		updated.UpdatedAt = time.Now()
		updated.Generation++
		updated.Metadata["type"] = "antigravity"
		updated.Metadata["disabled"] = true
		updated.Metadata["native_quota_rejection_reason"] = nativequota.ReasonTOSViolation
		updated.Metadata["native_quota_rejected_at"] = updated.UpdatedAt.UTC().Format(time.RFC3339Nano)
		updated.Metadata["status_message"] = nativeQuotaTOSStatusMessage
		if errPersist := m.persistNativeQuotaRejection(ctx, updated); errPersist != nil {
			return errPersist
		}
		installed = updated.Clone()
		m.auths[updated.ID] = installed
		return nil
	})
	m.nativeQuotaMu.Unlock()
	if err != nil || installed == nil {
		return err
	}
	// SetAccounts cancels this disabled account's current collection. In
	// particular it must not run before Save with the collection's context.
	if m.scheduler != nil {
		m.scheduler.upsertAuth(installed.Clone())
	}
	m.queueRefreshReschedule(installed.ID)
	m.syncNativeQuotaAccounts()
	m.hook.OnAuthUpdated(ctx, installed.Clone())
	return nil
}

// persistNativeQuotaRejection uses the ordinary per-auth persistence ordering,
// but cannot treat skip policies or an empty save receipt as durable success.
// Called with m.mu held, as in the Meta mint persist-before-install path.
func (m *Manager) persistNativeQuotaRejection(ctx context.Context, updated *Auth) error {
	if ValidateAuthWeight(updated) != nil {
		return errors.New("native-quota: rejection persistence invalid metadata")
	}
	lockValue, _ := m.persistLocks.LoadOrStore(updated.ID, &authPersistLock{})
	lock := lockValue.(*authPersistLock)
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if updated.RegistrationEpoch < lock.lastEpoch ||
		updated.RegistrationEpoch == lock.lastEpoch && updated.Generation < lock.lastGeneration {
		return errors.New("native-quota: rejection persistence superseded")
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	receipt, err := m.store.Save(ctx, updated)
	if err != nil {
		// Store errors can contain paths or credential data. Return a stable safe
		// failure and leave the current auth unchanged so the collector retries.
		return errors.New("native-quota: rejection persistence failed")
	}
	if strings.TrimSpace(receipt) == "" {
		return errors.New("native-quota: rejection persistence not confirmed")
	}
	lock.lastEpoch, lock.lastGeneration = updated.RegistrationEpoch, updated.Generation
	return nil
}

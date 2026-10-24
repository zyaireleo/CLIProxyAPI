package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
)

type nativeQuotaRequestContextKey struct{}

// IsNativeQuotaRequest identifies the internal, fixed-URL quota collector. It is
// not set from request headers or metadata and must never authorize generation.
func IsNativeQuotaRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(nativeQuotaRequestContextKey{}).(bool)
	return v
}

func (m *Manager) NativeQuota() *nativequota.Engine {
	if m == nil {
		return nil
	}
	if e := m.nativeQuota.Load(); e != nil {
		return e
	}
	m.nativeQuotaMu.Lock()
	defer m.nativeQuotaMu.Unlock()
	return m.nativeQuotaLocked()
}

func (m *Manager) nativeQuotaLocked() *nativequota.Engine {
	if e := m.nativeQuota.Load(); e != nil {
		return e
	}
	e := nativequota.New(nativequota.DefaultConfig(), m.fetchNativeQuota)
	m.nativeQuota.Store(e)
	return e
}

// ValidateNativeQuotaConfig is a side-effect-free preflight for management and
// config reload. A disabled plugin configuration is insufficient proof of unload.
func (m *Manager) ValidateNativeQuotaConfig(cfg internalconfig.NativeQuotaConfig) error {
	r, err := cfg.RuntimeConfig()
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("native-quota: manager is nil")
	}
	full := m.runtimeConfigSnapshot().CloneForRuntime()
	if full == nil {
		full = &internalconfig.Config{}
	}
	full.NativeQuota = cfg
	if err = full.ValidateNativeQuota(); err != nil {
		return err
	}
	if r.Mode != nativequota.ModeEnforce {
		return nil
	}
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	if guard, ok := scheduler.(interface{ NativeQuotaConflicts([]string) []string }); ok {
		if conflicts := guard.NativeQuotaConflicts(append([]string(nil), r.Providers...)); len(conflicts) != 0 {
			return fmt.Errorf("native-quota enforce conflicts with active plugin ownership: %s", strings.Join(conflicts, ", "))
		}
	} else if m.hasPluginScheduler() {
		return fmt.Errorf("native-quota enforce requires verified plugin scheduler handoff")
	}
	return nil
}

func (m *Manager) ApplyNativeQuotaConfig(ctx context.Context, cfg internalconfig.NativeQuotaConfig) error {
	if m == nil {
		return fmt.Errorf("native-quota: manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.nativeQuotaMu.Lock()
	defer m.nativeQuotaMu.Unlock()
	if err := m.ValidateNativeQuotaConfig(cfg); err != nil {
		return err
	}
	r, err := cfg.RuntimeConfig()
	if err != nil {
		return err
	}
	var ownership func()
	if r.Mode == nativequota.ModeEnforce {
		m.mu.RLock()
		scheduler := m.pluginScheduler
		m.mu.RUnlock()
		ownership, err = acquireNativeQuotaOwnership(scheduler, r.Providers)
		if err != nil {
			return err
		}
	}
	e := m.nativeQuotaLocked()
	if err = e.Configure(r); err != nil {
		if ownership != nil {
			ownership()
		}
		return err
	}
	previousOwnership := m.nativeQuotaOwnership
	m.nativeQuotaOwnership = ownership
	if previousOwnership != nil {
		previousOwnership()
	}
	m.syncNativeQuotaAccounts()
	// The manager owns this worker, not a management HTTP request's lifetime.
	// Service shutdown calls StopNativeQuota, including failed startup cleanup.
	if r.Mode != nativequota.ModeOff {
		e.Start(context.Background())
	}
	return nil
}

func (m *Manager) StopNativeQuota(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.nativeQuotaMu.Lock()
	defer m.nativeQuotaMu.Unlock()
	if e := m.nativeQuota.Load(); e != nil {
		cfg := e.Config()
		cfg.Mode = nativequota.ModeOff
		if err := e.Configure(cfg); err != nil {
			return err
		}
		if err := e.Stop(ctx); err != nil {
			return err
		}
	}
	if m.nativeQuotaOwnership != nil {
		m.nativeQuotaOwnership()
		m.nativeQuotaOwnership = nil
	}
	return nil
}

func acquireNativeQuotaOwnership(scheduler PluginScheduler, providers []string) (func(), error) {
	if scheduler == nil {
		return nil, nil
	}
	if owner, ok := scheduler.(interface {
		AcquireNativeQuotaOwnership([]string) (func(), error)
	}); ok {
		return owner.AcquireNativeQuotaOwnership(append([]string{}, providers...))
	}
	// A currently idle host without an atomic reservation could load a legacy
	// scheduler after preflight. Refuse it rather than relying on a racy check.
	return nil, fmt.Errorf("native-quota enforce requires atomic plugin ownership reservation")
}

// setNativeQuotaPluginScheduler serializes scheduler installation with enforce
// ownership. An unsafe replacement leaves the previous owner and selector intact.
func (m *Manager) setNativeQuotaPluginScheduler(scheduler PluginScheduler) error {
	m.nativeQuotaMu.Lock()
	defer m.nativeQuotaMu.Unlock()
	var ownership func()
	if e := m.nativeQuota.Load(); e != nil {
		cfg := e.Config()
		if cfg.Mode == nativequota.ModeEnforce {
			var err error
			ownership, err = acquireNativeQuotaOwnership(scheduler, cfg.Providers)
			if err != nil {
				return err
			}
		}
	}
	m.mu.Lock()
	m.pluginScheduler = scheduler
	m.mu.Unlock()
	previous := m.nativeQuotaOwnership
	m.nativeQuotaOwnership = ownership
	if previous != nil {
		previous()
	}
	return nil
}

func nativeQuotaAccount(a *Auth) nativequota.Account {
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	principal := ""
	for _, key := range []string{"account_id", "sub", "subject", "email"} {
		if value, ok := a.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			principal += "\x00" + key + "=" + strings.TrimSpace(value)
		}
	}
	if project, ok := a.Metadata["project_id"].(string); ok {
		principal += "\x00project=" + strings.TrimSpace(project)
	}
	if principal == "" {
		principal = fmt.Sprintf("%s:%d", a.ID, a.RegistrationEpoch)
	}
	revision := fmt.Sprintf("%x", sha256.Sum256([]byte(provider+"\x00"+principal)))
	return nativequota.Account{ID: a.ID, Provider: provider, Disabled: a.Disabled || a.Status == StatusDisabled, Priority: authPriority(a), Revision: revision}
}

// Membership changes are published only from lifecycle/config events. This
// copies safe account descriptors, never the full credential pool per request.
func (m *Manager) syncNativeQuotaAccounts() {
	if m == nil {
		return
	}
	e := m.nativeQuota.Load()
	if e == nil || e.Config().Mode == nativequota.ModeOff {
		return
	}
	m.nativeQuotaAccountsMu.Lock()
	defer m.nativeQuotaAccountsMu.Unlock()
	m.mu.RLock()
	accounts := make([]nativequota.Account, 0, len(m.auths))
	for _, a := range m.auths {
		if a != nil && (strings.EqualFold(a.Provider, "antigravity") || strings.EqualFold(a.Provider, "codex")) && a.AuthKind() != AuthKindAPIKey {
			accounts = append(accounts, nativeQuotaAccount(a))
		}
	}
	m.mu.RUnlock()
	e.SetAccounts(accounts)
}

func (m *Manager) nativeQuotaFor(provider string, enforce bool) *nativequota.Engine {
	if m == nil {
		return nil
	}
	e := m.nativeQuota.Load()
	if e == nil {
		return nil
	}
	c := e.Config()
	if c.Mode == nativequota.ModeOff || (enforce && c.Mode != nativequota.ModeEnforce) {
		return nil
	}
	for _, p := range c.Providers {
		if strings.EqualFold(provider, p) {
			return e
		}
	}
	return nil
}

func (m *Manager) nativeQuotaEnforcesAny(providers []string) bool {
	for _, p := range providers {
		if m.nativeQuotaFor(p, true) != nil {
			return true
		}
	}
	return false
}

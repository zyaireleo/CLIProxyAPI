package nativequota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxSnapshotBytes = 16 << 20

func revisionHash(revision string) string {
	if revision == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(revision))
	return hex.EncodeToString(sum[:])
}

type snapshot struct {
	Version  int               `json:"version"`
	Accounts []snapshotAccount `json:"accounts"`
}
type snapshotAccount struct {
	RevisionHash string                    `json:"revision_hash,omitempty"`
	Observation  Observation               `json:"observation"`
	InvalidAt    time.Time                 `json:"invalid_at"`
	LastAttempt  time.Time                 `json:"last_attempt"`
	BackoffUntil time.Time                 `json:"backoff_until"`
	Failures     int                       `json:"failures"`
	Health       map[string]snapshotHealth `json:"health,omitempty"`
}
type snapshotHealth struct {
	At       time.Time `json:"at"`
	Until    time.Time `json:"until"`
	Kind     string    `json:"kind"`
	Failures int       `json:"failures"`
}

// ExportSnapshot returns a versioned allowlist of quota/health evidence only.
// It contains no config, credentials, upstream bodies, errors, manual priorities,
// disabled flags, active leases, or queue entries. It performs no file writes.
func (e *Engine) ExportSnapshot() ([]byte, error) {
	e.mu.Lock()
	if e.cfg.Mode == ModeOff {
		e.mu.Unlock()
		return nil, ErrModeOff
	}
	out := snapshot{Version: 1, Accounts: make([]snapshotAccount, 0, len(e.accounts))}
	ids := make([]string, 0, len(e.accounts))
	for id := range e.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now()
	for _, id := range ids {
		s := e.accounts[id]
		if s.observation.ObservedAt.IsZero() && s.lastAttempt.IsZero() && len(s.health) == 0 {
			continue
		}
		item := snapshotAccount{RevisionHash: revisionHash(s.account.Revision), Observation: s.observation, InvalidAt: s.invalidAt, LastAttempt: s.lastAttempt, BackoffUntil: s.backoffUntil, Failures: s.failures}
		if item.Observation.AuthID == "" {
			item.Observation.AuthID, item.Observation.Provider = s.account.ID, s.account.Provider
		}
		item.Observation.Pools = clonePools(item.Observation.Pools)
		item.Observation.Partial = false
		for scope, h := range s.health {
			if !h.until.After(now) {
				continue
			}
			if item.Health == nil {
				item.Health = make(map[string]snapshotHealth)
			}
			item.Health[scope] = snapshotHealth{h.at, h.until, h.kind, h.failures}
		}
		out.Accounts = append(out.Accounts, item)
	}
	e.mu.Unlock()
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSnapshotBytes {
		return nil, errors.New("native quota: snapshot too large")
	}
	return raw, nil
}

// ImportSnapshot validates the entire document before atomically applying any
// entry. Accounts must already be registered; unknown IDs/providers are ignored.
// Old observations and health entries can never overwrite newer runtime evidence.
func (e *Engine) ImportSnapshot(raw []byte) error {
	if len(raw) == 0 || len(raw) > maxSnapshotBytes {
		return ErrInvalidQuota
	}
	var data snapshot
	if err := json.Unmarshal(raw, &data); err != nil || data.Version != 1 || len(data.Accounts) > 10000 {
		return ErrInvalidQuota
	}
	now := time.Now()
	seen := map[string]bool{}
	for i := range data.Accounts {
		item := &data.Accounts[i]
		ob, ok := prepareObservation(item.Observation, now)
		if item.Observation.ObservedAt.IsZero() && len(item.Observation.Pools) == 0 && validID(item.Observation.AuthID) && supportedProvider(item.Observation.Provider) {
			ob = Observation{AuthID: item.Observation.AuthID, Provider: item.Observation.Provider}
			ok = true
		}
		if !ok || seen[ob.AuthID] || item.Failures < 0 || item.Failures > 10 || item.InvalidAt.After(now.Add(time.Second)) || item.LastAttempt.After(now.Add(time.Second)) || item.BackoffUntil.After(now.Add(7*24*time.Hour)) || len(item.Health) > maxPools+1 {
			return ErrInvalidQuota
		}
		seen[ob.AuthID] = true
		item.Observation = ob
		item.Observation.Partial = false
		for scope, h := range item.Health {
			validScope := scope == "account" || strings.HasPrefix(scope, "pool:") && validModel(strings.TrimPrefix(scope, "pool:")) || strings.HasPrefix(scope, "model:") && validModel(strings.TrimPrefix(scope, "model:"))
			if !validScope || h.At.IsZero() || h.At.After(now.Add(time.Second)) || h.Until.Before(h.At) || h.Until.Sub(h.At) > 24*time.Hour || h.Failures < 1 || h.Failures > 10000 || h.Kind != "rate_limit" && h.Kind != "circuit" && h.Kind != "auth_failure" {
				return ErrInvalidQuota
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Mode == ModeOff {
		return ErrModeOff
	}
	for _, item := range data.Accounts {
		s := e.accounts[item.Observation.AuthID]
		if s == nil || s.account.Provider != item.Observation.Provider || item.RevisionHash != revisionHash(s.account.Revision) || !e.cfg.covers(s.account.Provider) {
			continue
		}
		if item.Observation.ObservedAt.After(s.observation.ObservedAt) && (s.invalidAt.IsZero() || item.Observation.ObservedAt.After(s.invalidAt)) {
			s.observation = item.Observation
			s.version++
		}
		if item.InvalidAt.After(s.invalidAt) && !item.InvalidAt.Before(s.observation.ObservedAt) {
			s.invalidAt = item.InvalidAt
		}
		if item.LastAttempt.After(s.lastAttempt) {
			s.lastAttempt = item.LastAttempt
			s.failures = item.Failures
		}
		if item.BackoffUntil.After(s.backoffUntil) {
			s.backoffUntil = item.BackoffUntil
		}
		for scope, h := range item.Health {
			if h.Until.After(now) && h.At.After(s.health[scope].at) {
				s.health[scope] = healthEvidence{h.At, h.Until, h.Kind, h.Failures}
			}
		}
		s.nextDue = e.nextDeadlineLocked(s, now)
	}
	e.notifyLocked()
	e.wakeup()
	return nil
}

// SaveSnapshot is an explicit owner-invoked operation; there is no background
// persistence. A same-directory temporary file is synced and renamed into place.
// The owner chooses a dedicated state path, never an auth/config file path.
func (e *Engine) SaveSnapshot(path string) error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	raw, err := e.ExportSnapshot()
	if err != nil {
		return err
	}
	if info, errStat := os.Lstat(path); errStat == nil && !info.Mode().IsRegular() {
		return errors.New("native quota: snapshot path must be a regular file")
	} else if errStat != nil && !os.IsNotExist(errStat) {
		return errStat
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".native-quota-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(temp)
	}()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	closed = true
	// persistMu serializes Configure and saves, without holding the request-state
	// mutex across any filesystem operation. The parent owns directory security.
	return os.Rename(temp, path)
}
func (e *Engine) LoadSnapshot(path string) error {
	e.mu.Lock()
	off := e.cfg.Mode == ModeOff
	e.mu.Unlock()
	if off {
		return ErrModeOff
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	raw, errRead := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	errClose := f.Close()
	if errRead != nil {
		return errRead
	}
	if errClose != nil {
		return errClose
	}
	return e.ImportSnapshot(raw)
}

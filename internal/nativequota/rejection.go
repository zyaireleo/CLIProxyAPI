package nativequota

import (
	"encoding/json"
	"errors"
	"net/http"
)

const ReasonTOSViolation = "TOS_VIOLATION"

// RejectionError contains only the allowlisted upstream rejection, never a body.
type RejectionError struct{}

func (*RejectionError) Error() string { return "native-quota: antigravity TOS_VIOLATION" }

// IsAntigravityTOSViolation requires the complete, case-sensitive ErrorInfo tuple.
// Human-readable messages, generic permission failures and other domains do not qualify.
func IsAntigravityTOSViolation(status int, raw []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var envelope map[string]json.RawMessage
	var upstream map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || json.Unmarshal(envelope["error"], &upstream) != nil {
		return false
	}
	text := func(raw json.RawMessage) string {
		var value string
		_ = json.Unmarshal(raw, &value)
		return value
	}
	if text(upstream["status"]) != "PERMISSION_DENIED" {
		return false
	}
	var details []map[string]json.RawMessage
	if json.Unmarshal(upstream["details"], &details) != nil {
		return false
	}
	for _, detail := range details {
		if text(detail["@type"]) == "type.googleapis.com/google.rpc.ErrorInfo" &&
			text(detail["reason"]) == ReasonTOSViolation && text(detail["domain"]) == "cloudcode-pa.googleapis.com" {
			return true
		}
	}
	return false
}

// RejectionAuthorization binds an enforce-only action to the collection's config
// epoch. A shadow read cannot acquire authority by completing after an upgrade.
type RejectionAuthorization struct {
	engine *Engine
	epoch  uint64
}

func (e *Engine) AuthorizeAntigravityRejection() RejectionAuthorization {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Mode != ModeEnforce || !e.cfg.covers(ProviderAntigravity) {
		return RejectionAuthorization{}
	}
	return RejectionAuthorization{engine: e, epoch: e.epoch}
}

// Apply serializes persistence with Configure without holding the request-state
// mutex during I/O. TryLock also avoids blocking a collector behind shutdown.
// The callback must not reconfigure the engine or save an engine snapshot.
func (a RejectionAuthorization) Apply(fn func() error) error {
	if a.engine == nil {
		return nil // Shadow is observational only.
	}
	e := a.engine
	if !e.persistMu.TryLock() {
		return errors.New("native-quota: rejection authorization busy; retry pending")
	}
	defer e.persistMu.Unlock()
	e.mu.Lock()
	allowed := e.epoch == a.epoch && e.cfg.Mode == ModeEnforce && e.cfg.covers(ProviderAntigravity)
	e.mu.Unlock()
	if !allowed {
		return errors.New("native-quota: rejection authorization changed")
	}
	return fn()
}

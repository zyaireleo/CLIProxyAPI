package nativequota

import "time"

// RecordDecision audits the owner's actual/hypothetical selection. It never
// selects an account, changes priorities, or triggers collection/generation.
// IDs must belong to the registered provider; reasons are a fixed safe enum.
func (e *Engine) RecordDecision(selectedID, proposedID, provider, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cfg.Mode == ModeOff || !e.cfg.covers(provider) {
		return
	}
	if selectedID != "" {
		s := e.accounts[selectedID]
		if s == nil || s.account.Provider != provider {
			return
		}
	}
	if proposedID != "" {
		s := e.accounts[proposedID]
		if s == nil || s.account.Provider != provider {
			return
		}
	}
	switch reason {
	case "same", "quota", "capacity", "weight", "unknown", "disabled", "no_candidate":
	default:
		reason = "selection"
	}
	e.eventLocked(selectedID, provider, "decision", reason, time.Now())
	index := len(e.events) - 1
	if len(e.events) == maxEvents && e.eventPos != 0 {
		index = e.eventPos - 1
	}
	e.events[index].ProposedID = proposedID
}

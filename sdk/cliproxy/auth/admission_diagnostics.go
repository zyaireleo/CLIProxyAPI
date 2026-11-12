package auth

import (
	"sort"
	"time"
)

// AdmissionPauseSnapshot describes a runtime admission pause separately from
// credential health and upstream quota. It intentionally contains no identity.
type AdmissionPauseSnapshot struct {
	Model            string    `json:"model"`
	Reason           string    `json:"reason"`
	RetryAt          time.Time `json:"retry_at"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	Level            int       `json:"level"`
}

// AdmissionPauseSnapshotForAuth reads a cloned manager auth without changing its
// deadlines. Older states have no persisted failure reason, so report the local
// mechanism rather than guessing an upstream ErrorInfo reason.
func AdmissionPauseSnapshotForAuth(auth *Auth, now time.Time) []AdmissionPauseSnapshot {
	if auth == nil {
		return nil
	}
	var pauses []AdmissionPauseSnapshot
	for model, state := range auth.AdmissionBackoffs {
		if !state.Until.After(now) {
			continue
		}
		remaining := state.Until.Sub(now)
		seconds := int64(remaining / time.Second)
		if remaining%time.Second != 0 {
			seconds++
		}
		pauses = append(pauses, AdmissionPauseSnapshot{
			Model: model, Reason: "local_admission_backoff", RetryAt: state.Until.UTC(),
			RemainingSeconds: seconds, Level: state.Level,
		})
	}
	sort.Slice(pauses, func(i, j int) bool { return pauses[i].Model < pauses[j].Model })
	return pauses
}

package auth

import "time"

// AdmissionBackoff is a runtime-only admission pause, separate from credential
// health, priority and quota. Sequence prevents stale successes from clearing it.
type AdmissionBackoff struct {
	Until    time.Time
	Level    int
	Sequence uint64
}

func (state AdmissionBackoff) afterFailure(now time.Time, sequence uint64, hint *time.Duration) AdmissionBackoff {
	if !state.Until.After(now) {
		delays := [...]time.Duration{2, 4, 8, 16, 30}
		level := state.Level
		if level < 0 {
			level = 0
		}
		if level >= len(delays) {
			level = len(delays) - 1
		}
		delay := delays[level] * time.Second
		if hint != nil && *hint > delay {
			delay = *hint
		}
		state.Until = now.Add(delay)
		if state.Level < len(delays) {
			state.Level = level + 1
		}
	}
	// Failures within an active window preserve its deadline and exponential
	// step, but still advance the stale-success watermark.
	if sequence > state.Sequence {
		state.Sequence = sequence
	}
	return state
}

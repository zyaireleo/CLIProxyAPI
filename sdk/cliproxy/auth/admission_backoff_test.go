package auth

import (
	"testing"
	"time"
)

func TestAdmissionBackoffWindowsAndCap(t *testing.T) {
	now := time.Unix(1000, 0)
	var state AdmissionBackoff
	for i, seconds := range []int{2, 4, 8, 16, 30, 30, 30} {
		state = state.afterFailure(now, uint64(i+1), nil)
		if got := state.Until.Sub(now); got != time.Duration(seconds)*time.Second {
			t.Fatalf("step %d: %v", i, got)
		}
		now = state.Until
	}
}

func TestAdmissionBackoffConcurrentFailuresDoNotExtendWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	state := (AdmissionBackoff{}).afterFailure(now, 10, nil)
	deadline := state.Until
	for i := 11; i < 100; i++ {
		state = state.afterFailure(now.Add(time.Second), uint64(i), nil)
	}
	if !state.Until.Equal(deadline) || state.Level != 1 || state.Sequence != 99 {
		t.Fatalf("concurrent errors amplified pause: %+v", state)
	}
}

func TestAdmissionBackoffHonorsStructuredHint(t *testing.T) {
	now := time.Unix(1000, 0)
	hint := 40 * time.Second
	state := (AdmissionBackoff{}).afterFailure(now, 1, &hint)
	if !state.Until.Equal(now.Add(hint)) {
		t.Fatalf("hint lost: %+v", state)
	}
}

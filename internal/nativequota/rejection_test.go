package nativequota

import (
	"errors"
	"strings"
	"testing"
)

const tosRejectionFixture = `{"error":{"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","domain":"cloudcode-pa.googleapis.com"}]}}`

func TestAntigravityTOSViolationExactTuple(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		raw    string
		want   bool
	}{
		{"exact", 403, tosRejectionFixture, true},
		{"429", 429, tosRejectionFixture, false},
		{"200", 200, tosRejectionFixture, false},
		{"generic", 403, `{"error":{"status":"PERMISSION_DENIED","message":"TOS_VIOLATION"}}`, false},
		{"status", 403, strings.ReplaceAll(tosRejectionFixture, "PERMISSION_DENIED", "UNKNOWN"), false},
		{"type", 403, strings.ReplaceAll(tosRejectionFixture, "google.rpc.ErrorInfo", "google.rpc.OtherInfo"), false},
		{"reason", 403, strings.ReplaceAll(tosRejectionFixture, "TOS_VIOLATION", "tos_violation"), false},
		{"domain", 403, strings.ReplaceAll(tosRejectionFixture, "cloudcode-pa.googleapis.com", "other.googleapis.com"), false},
		{"key casing", 403, strings.ReplaceAll(tosRejectionFixture, "status", "Status"), false},
		{"split tuple", 403, `{"error":{"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"},{"domain":"cloudcode-pa.googleapis.com"}]}}`, false},
		{"truncated", 403, tosRejectionFixture[:len(tosRejectionFixture)-1], false},
		{"trailing", 403, tosRejectionFixture + `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAntigravityTOSViolation(tc.status, []byte(tc.raw)); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRejectionAuthorizationModeEpochAndLock(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeShadow
	e := New(cfg, nil)
	shadow := e.AuthorizeAntigravityRejection()
	cfg.Mode = ModeEnforce
	if err := e.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	calls := 0
	apply := func() error { calls++; return nil }
	if err := shadow.Apply(apply); err != nil || calls != 0 {
		t.Fatal("shadow acquired authority")
	}
	old := e.AuthorizeAntigravityRejection()
	cfg.Mode = ModeShadow
	if err := e.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Mode = ModeEnforce
	if err := e.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := old.Apply(apply); err == nil || calls != 0 {
		t.Fatal("old epoch acquired authority")
	}
	fresh := e.AuthorizeAntigravityRejection()
	e.persistMu.Lock()
	if err := fresh.Apply(apply); err == nil || calls != 0 {
		t.Error("busy config lock was not rejected")
	}
	e.persistMu.Unlock()
	sentinel := errors.New("fixture")
	if err := fresh.Apply(func() error {
		if !e.mu.TryLock() {
			t.Fatal("request-state lock held during persistence")
		}
		e.mu.Unlock()
		if e.persistMu.TryLock() {
			e.persistMu.Unlock()
			t.Fatal("configuration not guarded")
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatal("callback error not propagated")
	}
}

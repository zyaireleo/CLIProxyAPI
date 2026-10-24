package nativequota

import (
	"testing"
	"time"
)

func TestConfirmedRejectionReasonIsWhitelistedInDisabledView(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	engine := New(cfg, nil)
	engine.SetAccounts([]Account{
		{ID: "confirmed", Provider: ProviderAntigravity, Disabled: true, RejectionReason: "TOS_VIOLATION"},
		{ID: "manual", Provider: ProviderAntigravity, Disabled: true},
		{ID: "untrusted", Provider: ProviderAntigravity, Disabled: true, RejectionReason: "secret body must not escape"},
		{ID: "codex", Provider: ProviderCodex, Disabled: true, RejectionReason: "TOS_VIOLATION"},
	})
	for _, test := range []struct{ id, provider, reason string }{
		{"confirmed", ProviderAntigravity, "upstream_tos_violation"},
		{"manual", ProviderAntigravity, "manual_disabled"},
		{"untrusted", ProviderAntigravity, "manual_disabled"},
		{"codex", ProviderCodex, "manual_disabled"},
	} {
		view := engine.AssessSummary(test.id, test.provider, "", time.Now())
		if view.Reason != test.reason || view.Allowed || view.State != "disabled" {
			t.Fatalf("%s: %+v", test.id, view)
		}
	}
}

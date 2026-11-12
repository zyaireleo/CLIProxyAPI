package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestAdmissionPauseSnapshotIsSortedReadOnlyAndPrivate(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	a := &Auth{ID: "private-account", Metadata: map[string]any{"access_token": "private-token"}, AdmissionBackoffs: map[string]AdmissionBackoff{
		"gemini-z": {Until: now.Add(5 * time.Second), Level: 3},
		"gemini-a": {Until: now.Add(time.Nanosecond), Level: 1},
		"expired":  {Until: now},
	}}
	views := AdmissionPauseSnapshotForAuth(a, now)
	if len(views) != 2 || views[0].Model != "gemini-a" || views[1].Model != "gemini-z" {
		t.Fatalf("unexpected ordering or expired pause: %+v", views)
	}
	if views[0].RemainingSeconds != 1 || views[1].RemainingSeconds != 5 || views[1].Reason != "local_admission_backoff" {
		t.Fatalf("unexpected recovery metadata: %+v", views)
	}
	if strings.Contains(fmt.Sprint(views), "private-") {
		t.Fatal("admission snapshot disclosed auth identity or token")
	}
	views[1].RetryAt = now
	if !a.AdmissionBackoffs["gemini-z"].Until.Equal(now.Add(5*time.Second)) || len(a.AdmissionBackoffs) != 3 {
		t.Fatal("snapshot changed admission state")
	}
	if AdmissionPauseSnapshotForAuth(nil, now) != nil {
		t.Fatal("nil auth should have no admission snapshot")
	}
}

func TestAdmissionSevenPausesAndOneTOSPreserveFiveSecondRetry(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	const model = "gemini-2.5-flash"
	var auths []*Auth
	for i := range 7 {
		pause := AdmissionBackoff{Level: 2}.afterFailure(now.Add(-3*time.Second), uint64(i+1), nil)
		auths = append(auths, &Auth{ID: fmt.Sprintf("paused-%d", i), Provider: "antigravity", Status: StatusActive,
			AdmissionBackoffs: map[string]AdmissionBackoff{model: pause}})
	}
	tos := &Error{HTTPStatus: http.StatusForbidden, Message: "This service has been disabled in this account for violation of Terms of Service."}
	auths = append(auths, &Auth{ID: "tos", Provider: "antigravity", Status: StatusError, Unavailable: true,
		ModelStates: map[string]*ModelState{model: {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), LastError: tos, UpdatedAt: now}}})
	m := NewManager(nil, nil, nil)
	hook := setupTestLoggerHook(t)
	ctx := logging.WithRequestID(context.Background(), "admission-seven-plus-one")
	available, err := m.availableAuthsForRouteModelForRequest(ctx, auths, "antigravity", model, now, false)
	var unavailable *authUnavailableError
	if len(available) != 0 || !errors.As(err, &unavailable) || unavailable.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("expected temporary auth_unavailable, got %v %v", available, err)
	}
	if unavailable.Headers().Get("Retry-After") != "5" || !strings.Contains(err.Error(), "Terms of Service") {
		t.Fatalf("expected five-second local hint plus cached error: %v", err)
	}
	var found bool
	for _, entry := range hook.AllEntries() {
		if entry.Data["request_id"] != "admission-seven-plus-one" || entry.Data["event"] != "auth_selection_unavailable" {
			continue
		}
		found = true
		if entry.Data["admission_count"] != 7 || entry.Data["health_or_other_count"] != 1 || entry.Data["earliest_recovery_source"] != "admission_backoff" {
			t.Fatalf("incorrect exclusion diagnosis: %+v", entry.Data)
		}
		if entry.Data["retry_after_seconds"] != int64(5) || entry.Data["earliest_recovery_at"] != now.Add(5*time.Second).Format(time.RFC3339Nano) {
			t.Fatalf("incorrect admission deadline: %+v", entry.Data)
		}
		if entry.Data["cached_error_context"] != "last_observed_upstream_error_not_all_candidates" {
			t.Fatal("cached error was not distinguished from candidate-wide cause")
		}
	}
	if !found {
		t.Fatal("missing correlated selection diagnosis")
	}
	for _, a := range auths[:7] {
		if a.Unavailable || a.Quota.Exceeded || a.Status != StatusActive {
			t.Fatal("admission pause mutated health or quota")
		}
	}
	healthy := &Auth{ID: "available", Provider: "antigravity", Status: StatusActive}
	available, err = m.availableAuthsForRouteModelForRequest(ctx, append(auths, healthy), "antigravity", model, now, false)
	if err != nil || len(available) != 1 || available[0].ID != healthy.ID {
		t.Fatalf("healthy unpaused credential was rejected: %v %v", available, err)
	}
	available, err = m.availableAuthsForRouteModelForRequest(ctx, auths, "antigravity", model, now.Add(6*time.Second), false)
	if err != nil || len(available) != 7 {
		t.Fatalf("natural admission expiry did not release seven credentials: %v %v", available, err)
	}
	if !auths[7].ModelStates[model].NextRetryAfter.Equal(now.Add(time.Hour)) {
		t.Fatal("natural admission expiry modified TOS protection")
	}
}

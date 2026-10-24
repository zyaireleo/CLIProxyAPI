package nativequota

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotRoundTripSanitizedAtomicFileAndRevision(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "stable-principal-private", Priority: 55}})
	ob := observation("a", ProviderCodex, time.Now().Add(-time.Second), 0.75)
	ob.Source = "Bearer private-source"
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	e.RecordResult("a", ProviderCodex, "gpt-5", 429, false, time.Minute)
	raw, err := e.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"stable-principal-private", "Bearer", "private-source", "priority", "in_flight", "access_token", "refresh_token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot contains %s", secret)
		}
	}
	other := testEngine(t, ProviderCodex, nil)
	other.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "stable-principal-private", Disabled: true}})
	if err = other.ImportSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	if v := other.Assess("a", ProviderCodex, "gpt-5", time.Now()); !v.Disabled || v.Allowed {
		t.Fatal("import changed manual disable")
	}
	other.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "stable-principal-private"}})
	v := other.Assess("a", ProviderCodex, "gpt-5", time.Now())
	if v.Reason != "rate_limit" || *v.Pools[0].Windows[0].Remaining != 0.75 {
		t.Fatalf("lost evidence: %+v", v)
	}
	other.SetAccounts([]Account{{ID: "a", Provider: ProviderCodex, Revision: "different"}})
	if err = other.ImportSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	if v = other.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()); v.State != "unknown" {
		t.Fatal("identity revision leaked old snapshot")
	}
	path := filepath.Join(t.TempDir(), "quota.json")
	if err = e.SaveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if err = e.SaveSnapshot(path); err != nil {
		t.Fatalf("atomic replacement: %v", err)
	}
	if err = other.LoadSnapshot(path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("left temp file: %v %v", entries, err)
	}
}
func TestSnapshotInvalidAtomicAndOldCannotOverwrite(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	at := time.Now().Add(-time.Minute)
	e.Observe(observation("a", ProviderCodex, at, 0))
	raw, _ := e.ExportSnapshot()
	e.Observe(observation("a", ProviderCodex, time.Now(), 0.9))
	if err := e.ImportSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	if v := e.Assess("a", ProviderCodex, "gpt-5", time.Now()); *v.Pools[0].Windows[0].Remaining != 0.9 {
		t.Fatal("old snapshot overwrote")
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	snap.Accounts[0].Observation.ObservedAt = time.Now()
	snap.Accounts[0].Observation.Pools[0].Windows[0].Remaining = fraction(0.2)
	bad := snap.Accounts[0]
	bad.Observation.AuthID = "bad"
	bad.Failures = -1
	snap.Accounts = append(snap.Accounts, bad)
	malformed, _ := json.Marshal(snap)
	if err := e.ImportSnapshot(malformed); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("malformed: %v", err)
	}
	if v := e.Assess("a", ProviderCodex, "gpt-5", time.Now()); *v.Pools[0].Windows[0].Remaining != 0.9 {
		t.Fatal("partial invalid import applied")
	}
	for _, data := range []string{``, `null`, `{"version":2}`, `{"version":1,"accounts":[null]}`} {
		if err := e.ImportSnapshot([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
func TestSnapshotOffNeverWrites(t *testing.T) {
	e := New(DefaultConfig(), nil)
	path := filepath.Join(t.TempDir(), "not-created.json")
	if _, err := e.ExportSnapshot(); !errors.Is(err, ErrModeOff) {
		t.Fatal(err)
	}
	if err := e.SaveSnapshot(path); !errors.Is(err, ErrModeOff) {
		t.Fatal(err)
	}
	if err := e.LoadSnapshot(path); !errors.Is(err, ErrModeOff) {
		t.Fatal(err)
	}
	if err := e.ImportSnapshot([]byte(`{"version":1,"accounts":[]}`)); !errors.Is(err, ErrModeOff) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("off wrote state")
	}
}
func TestBoundedExponentAndWindowAllowedFalse(t *testing.T) {
	for _, n := range []string{"1e999999999", "1e-999999999", "1e401", "1e-401"} {
		if _, err := number(json.RawMessage(n)); !errors.Is(err, ErrInvalidQuota) {
			t.Fatalf("unbounded exponent: %s %v", n, err)
		}
	}
	ob, err := ParseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"allowed":false,"limit_window_seconds":604800}}}`), "a", time.Now())
	if err != nil || !ob.Pools[0].Windows[0].Blocked || ob.Pools[0].Windows[0].Remaining != nil {
		t.Fatalf("window denial: %+v %v", ob, err)
	}
}
func TestMissingDataPreservesUnexpiredZeroAndPartialDeadline(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	at := time.Now().Add(-time.Minute)
	ob := observation("a", ProviderCodex, at, 0)
	ob.Source = SourceCodexUsage
	ob.Pools[0].Windows[0].ResetAt = at.Add(time.Hour)
	ob.Pools[0].Windows = append(ob.Pools[0].Windows, Window{ID: "short", Remaining: fraction(1)})
	if !e.Observe(ob) {
		t.Fatal("initial")
	}
	newer := observation("a", ProviderCodex, time.Now(), 0.8)
	newer.Source = SourceCodexHeaders
	newer.Pools[0].Windows[0].ID = "short"
	if !e.Observe(newer) {
		t.Fatal("new observation")
	}
	v := e.Assess("a", ProviderCodex, "gpt-5", time.Now())
	if v.Allowed || len(v.Pools[0].Windows) != 2 {
		t.Fatal("omitted weekly erased explicit zero")
	}
	e.mu.Lock()
	due := e.accounts["a"].nextDue
	e.mu.Unlock()
	if due.After(at.Add(e.Config().CodexInterval)) {
		t.Fatal("passive data postponed weekly read")
	}
	raw, err := e.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	other := testEngine(t, ProviderCodex, nil)
	if err = other.ImportSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	v = other.Assess("a", ProviderCodex, "gpt-5", time.Now())
	for _, w := range v.Pools[0].Windows {
		if w.ID == "weekly" && w.Source != SourceCodexUsage {
			t.Fatal("snapshot changed provenance")
		}
	}
}

func TestSnapshotPreservesHealthBeforeFirstObservation(t *testing.T) {
	e := testEngine(t, ProviderCodex, nil)
	e.RecordResult("a", ProviderCodex, "gpt-5", 429, false, time.Minute)
	raw, err := e.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	other := testEngine(t, ProviderCodex, nil)
	if err = other.ImportSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	if other.AssessSummary("a", ProviderCodex, "gpt-5", time.Now()).Allowed {
		t.Fatal("lost early health")
	}
}

func TestAGFallbackCannotEraseAuthoritativeSummary(t *testing.T) {
	e := testEngine(t, ProviderAntigravity, nil)
	at := time.Now().Add(-time.Second)
	ob, err := ParseAntigravity([]byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}]}]}`), "a", at)
	if err != nil || !e.Observe(ob) {
		t.Fatal(err)
	}
	fallback, err := ParseAntigravity([]byte(`{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1}}}}`), "a", time.Now())
	if err != nil || !e.Observe(fallback) {
		t.Fatal(err)
	}
	if v := e.Assess("a", ProviderAntigravity, "gemini-pro", time.Now()); v.Allowed || v.Pools[0].Windows[0].ID != "gemini-weekly" {
		t.Fatalf("fallback erased summary: %+v", v)
	}
}

package nativequota

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAntigravitySummaryGroupsAndDisabledWindow(t *testing.T) {
	at := time.Date(2026, 10, 23, 16, 0, 18, 0, time.UTC)
	raw := []byte(`{"groups":[{"displayName":"Gemini Models","description":"not retained","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0,"resetTime":"2026-10-30T16:00:18Z"},{"bucketId":"gemini-5h","window":"5h","remainingFraction":1,"disabled":true,"resetTime":"2026-10-23T21:00:18Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.99210614},{"bucketId":"3p-5h","window":"5h","remainingFraction":0.000000001}]}],"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1}}}}`)
	ob, err := ParseAntigravity(raw, "a", at)
	if err != nil {
		t.Fatal(err)
	}
	if ob.Partial || len(ob.Pools) != 2 || ob.Pools[0].ID != "gemini" || ob.Pools[1].ID != "claude_gpt" {
		t.Fatalf("bad groups: %+v", ob)
	}
	g := ob.Pools[0]
	if g.Blocked || len(g.Windows) != 2 || *g.Windows[0].Remaining != 0 || !g.Windows[1].Disabled || g.Windows[1].DurationSeconds != 18000 || g.Windows[0].DurationSeconds != 604800 {
		t.Fatalf("bad windows: %+v", g)
	}
	if *ob.Pools[1].Windows[0].Remaining != 0.99210614 || *ob.Pools[1].Windows[1].Remaining != 1e-9 {
		t.Fatal("lost precision")
	}
	cfg := DefaultConfig()
	cfg.Mode = ModeEnforce
	e := New(cfg, nil)
	e.SetAccounts([]Account{{ID: "a", Provider: ProviderAntigravity}})
	if !e.Observe(ob) {
		t.Fatal("rejected summary")
	}
	if v := e.Assess("a", ProviderAntigravity, "gemini-pro", at.Add(time.Second)); v.State != "exhausted" || v.Allowed {
		t.Fatalf("weekly depletion lost: %+v", v)
	}
	if v := e.Assess("a", ProviderAntigravity, "claude-sonnet", at.Add(time.Second)); !v.Allowed || v.State != "ready" {
		t.Fatalf("cross group poison: %+v", v)
	}
	encoded, _ := json.Marshal(ob)
	if strings.Contains(string(encoded), "description") || strings.Contains(string(encoded), "not retained") {
		t.Fatal("retained uncontrolled metadata")
	}
}

func TestAntigravityPresenceValidityAndFallback(t *testing.T) {
	at := time.Now().Add(-time.Second)
	cases := []struct {
		name, raw string
		wantErr   bool
	}{
		{"empty", `{}`, true}, {"models empty", `{"models":{}}`, true},
		{"groups empty must not fallback", `{"groups":[],"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1}}}}`, true},
		{"unknown only", `{"models":{"gemini-pro":{"quotaInfo":{}}}}`, true},
		{"missing with other known", `{"models":{"gemini-pro":{"quotaInfo":{}},"claude-sonnet":{"quotaInfo":{"remainingFraction":0}}}}`, false},
		{"malformed mixed", `{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1}},"claude-sonnet":{"quotaInfo":{"remainingFraction":"bad"}}}}`, true},
		{"out of range", `{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1.1}}}}`, true},
		{"null", `{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":null}}}}`, true},
		{"bucket unknown", `{"buckets":[{"bucketId":"gemini-weekly","window":"weekly"},{"bucketId":"3p-weekly","remainingFraction":1}]}`, false},
		{"bucket zero", `{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}]}`, false},
		{"malformed second bucket", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","remainingFraction":1},{"bucketId":"gemini-5h","remainingFraction":-1}]}]}`, true},
		{"invalid reset", `{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":1,"resetTime":"yesterday"}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ob, err := ParseAntigravity([]byte(tc.raw), "a", at)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ob=%+v err=%v", ob, err)
			}
		})
	}
	ob, err := ParseAntigravity([]byte(`{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":1e-400,"resetTime":"2027-01-01T00:00:00Z"}},"gemini-flash":{},"claude-sonnet":{"quotaInfo":{"remainingFraction":0}}}}`), "a", at)
	if err != nil {
		t.Fatal(err)
	}
	if !ob.Partial || ob.Pools[0].Windows[0].Remaining != nil || *ob.Pools[0].Windows[1].Remaining <= 0 || ob.Pools[0].Windows[1].DurationSeconds != 0 {
		t.Fatalf("presence/underflow/duration: %+v", ob)
	}
}

func TestCodexUsageWindowsModelScopesAndPrecision(t *testing.T) {
	at := time.Now().Add(-time.Second)
	raw := []byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":99.9999999999999999999,"limit_window_seconds":604800},"secondary_window":{"used_percent":40,"limit_window_seconds":2592000}},"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000}}}],"credits":{"has_credits":true,"balance":"secret"},"code_review_rate_limit":{"allowed":false}}`)
	ob, err := ParseCodexUsage(raw, "a", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(ob.Pools) != 2 || ob.Pools[0].Windows[0].ID != "weekly" || ob.Pools[0].Windows[1].ID != "monthly" || *ob.Pools[0].Windows[0].Remaining <= 0 || *ob.Pools[0].Windows[1].Remaining != 0.6 {
		t.Fatalf("bad usage: %+v", ob)
	}
	if ob.Pools[1].ID != "codex:gpt-5.3-codex-spark" || len(ob.Pools[1].Models) != 1 || ob.Pools[1].Models[0] != "gpt-5.3-codex-spark" || !ob.Pools[1].Blocked {
		t.Fatalf("lost model scope: %+v", ob.Pools[1])
	}
	e := testEngine(t, ProviderCodex, nil)
	if !e.Observe(ob) {
		t.Fatal("observe")
	}
	if v := e.Assess("a", ProviderCodex, "gpt-5.3-codex", at); !v.Allowed {
		t.Fatalf("specific blocked generic: %+v", v)
	}
	if v := e.Assess("a", ProviderCodex, "gpt-5.3-codex-spark", at); v.Allowed {
		t.Fatal("ignored specific block")
	}
	if v := e.Assess("a", ProviderCodex, "unrelated", at); v.State != "unknown" {
		t.Fatalf("fabricated model membership: %+v", v)
	}
}

func TestCodexInvalidAndUnknown(t *testing.T) {
	at := time.Now()
	for _, raw := range []string{`{}`, `{"rate_limit":{}}`, `{"rate_limit":{"allowed":true}}`, `{"credits":{"unlimited":true}}`, `{"rate_limit":{"primary_window":{"used_percent":null}}}`, `{"rate_limit":{"primary_window":{"used_percent":101}}}`, `{"rate_limit":{"primary_window":{"used_percent":"20"}}}`, `{"rate_limit":{"primary_window":{"used_percent":20},"secondary_window":{"used_percent":-1}}}`, `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":1.5}}}`} {
		if _, err := ParseCodexUsage([]byte(raw), "a", at); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	ob, err := ParseCodexUsage([]byte(`{"additional_rate_limits":{"opaque":{"models":["gpt-special"],"rate_limit":{"primary":{"usedPercent":25,"windowMinutes":10080}}}}}`), "a", at)
	if err != nil || ob.Pools[0].Models[0] != "gpt-special" || *ob.Pools[0].Windows[0].Remaining != 0.75 {
		t.Fatalf("explicit model: %+v %v", ob, err)
	}
}

func TestCodexPassiveHeaders(t *testing.T) {
	at := time.Now()
	h := http.Header{
		"x-codex-primary-used-percent":                                    {"99.999999999999999999"},
		"x-codex-primary-window-minutes":                                  {"10080"},
		"x-codex-primary-reset-after-seconds":                             {"120"},
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Secondary-Used-Percent":   {"100"},
		"X-Codex-Additional-Gpt-5.3-Codex-Spark-Secondary-Window-Minutes": {"300"},
		"Authorization": {"Bearer never retain"}, "Set-Cookie": {"never retain"},
	}
	ob, err := ParseCodexHeaders(h, "a", at)
	if err != nil {
		t.Fatal(err)
	}
	if !ob.Partial || len(ob.Pools) != 2 {
		t.Fatalf("bad headers: %+v", ob)
	}
	var base, specific *Pool
	for i := range ob.Pools {
		if ob.Pools[i].ID == "codex" {
			base = &ob.Pools[i]
		} else {
			specific = &ob.Pools[i]
		}
	}
	if base == nil || specific == nil || *base.Windows[0].Remaining <= 0 || base.Windows[0].ID != "weekly" || !base.Windows[0].ResetAt.Equal(at.Add(120*time.Second)) || specific.Models[0] != "gpt-5.3-codex-spark" {
		t.Fatalf("bad windows: %+v", ob)
	}
	b, _ := json.Marshal(ob)
	if strings.Contains(string(b), "retain") {
		t.Fatal("header secret leaked")
	}
	h["x-codex-primary-used-percent"] = []string{"NaN"}
	if _, err := ParseCodexHeaders(h, "a", at); !errors.Is(err, ErrInvalidQuota) {
		t.Fatalf("accepted nan: %v", err)
	}
	if _, err := ParseCodexHeaders(http.Header{"Retry-After": {"30"}}, "a", at); !errors.Is(err, ErrNoQuota) {
		t.Fatalf("retry-after fabricated balance: %v", err)
	}
}

func TestFractionsNoTinyRoundToZero(t *testing.T) {
	for _, raw := range []string{"0.00000000001", "1e-400"} {
		p, err := parseFraction(json.RawMessage(raw), false)
		if err != nil || p == nil || *p <= 0 {
			t.Fatalf("lost %s", raw)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
		if validFraction(v) {
			t.Fatal("accepted invalid float")
		}
	}
	p, err := parseFraction(json.RawMessage("100"), true)
	if err != nil || *p != 0 {
		t.Fatal("exact exhaustion not preserved")
	}
}

func FuzzProviderParsers(f *testing.F) {
	f.Add([]byte(`{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":0.01}}}}`))
	f.Add([]byte(`{"rate_limit":{"primary_window":{"used_percent":20}}}`))
	f.Add([]byte(`{"groups":[]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8192 {
			t.Skip()
		}
		at := time.Unix(1780000000, 0)
		for _, parse := range []func([]byte, string, time.Time) (Observation, error){ParseAntigravity, ParseCodexUsage} {
			ob, err := parse(raw, "a", at)
			if err != nil {
				continue
			}
			if _, ok := prepareObservation(ob, at); !ok {
				t.Fatalf("parser produced invalid observation: %+v", ob)
			}
		}
	})
}

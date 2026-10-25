package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func safetyBuildRequest(t *testing.T, cfg *config.Config, payload []byte) map[string]any {
	t.Helper()
	executor := &AntigravityExecutor{cfg: cfg}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	req, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.8-flash", payload, false, "", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	body := requestBody(t, req)
	wire, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request envelope missing: %v", body)
	}
	return wire
}

func safetySettingsByCategory(t *testing.T, wire map[string]any) map[string]string {
	t.Helper()
	raw, ok := wire["safetySettings"].([]any)
	if !ok {
		t.Fatalf("request.safetySettings missing: %v", wire)
	}
	out := map[string]string{}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("safetySettings entry not an object: %v", item)
		}
		out[entry["category"].(string)] = entry["threshold"].(string)
	}
	return out
}

func TestAntigravityBuildRequest_SafetyModeStripsByDefault(t *testing.T) {
	for _, mode := range []string{"", "strip", "STRIP", "unknown"} {
		payload, err := json.Marshal(map[string]any{
			"request": map[string]any{
				"contents":       []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
				"safetySettings": []any{map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		wire := safetyBuildRequest(t, &config.Config{Antigravity: config.AntigravityConfig{SafetyMode: mode}}, payload)
		if _, exists := wire["safetySettings"]; exists {
			t.Fatalf("mode %q: request.safetySettings must stay stripped, got %v", mode, wire["safetySettings"])
		}
	}
}

func TestAntigravityBuildRequest_SafetyModeFourOffInjectsDefaults(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := safetyBuildRequest(t, &config.Config{Antigravity: config.AntigravityConfig{SafetyMode: "four_off"}}, payload)
	settings := safetySettingsByCategory(t, wire)
	want := map[string]string{
		"HARM_CATEGORY_HARASSMENT":        "OFF",
		"HARM_CATEGORY_HATE_SPEECH":       "OFF",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT": "OFF",
		"HARM_CATEGORY_DANGEROUS_CONTENT": "OFF",
	}
	for category, threshold := range want {
		if got, ok := settings[category]; !ok || got != threshold {
			t.Fatalf("category %s = %v (present=%v), want %s", category, got, ok, threshold)
		}
	}
	if _, exists := settings["HARM_CATEGORY_CIVIC_INTEGRITY"]; exists {
		t.Fatalf("CIVIC_INTEGRITY must never be sent to the antigravity endpoint: %v", settings)
	}
	if len(settings) != len(want) {
		t.Fatalf("unexpected extra categories: %v", settings)
	}
}

func TestAntigravityBuildRequest_SafetyModeFourOffPreservesCallerSettingsWithoutCivic(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
			"safetySettings": []any{
				map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
				map[string]any{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
				map[string]any{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"},
				map[string]any{"category": "HARM_CATEGORY_NONEXISTENT", "threshold": "OFF"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := safetyBuildRequest(t, &config.Config{Antigravity: config.AntigravityConfig{SafetyMode: "four_off"}}, payload)
	settings := safetySettingsByCategory(t, wire)
	if got := settings["HARM_CATEGORY_HARASSMENT"]; got != "BLOCK_NONE" {
		t.Fatalf("caller threshold not preserved: HARASSMENT = %q, want BLOCK_NONE", got)
	}
	if got := settings["HARM_CATEGORY_HATE_SPEECH"]; got != "BLOCK_NONE" {
		t.Fatalf("caller threshold not preserved: HATE_SPEECH = %q, want BLOCK_NONE", got)
	}
	if _, exists := settings["HARM_CATEGORY_CIVIC_INTEGRITY"]; exists {
		t.Fatalf("CIVIC_INTEGRITY must be dropped: %v", settings)
	}
	if _, exists := settings["HARM_CATEGORY_NONEXISTENT"]; exists {
		t.Fatalf("unknown category must be dropped: %v", settings)
	}
	// Remaining accepted categories default to OFF.
	if got := settings["HARM_CATEGORY_SEXUALLY_EXPLICIT"]; got != "OFF" {
		t.Fatalf("absent caller category should default to OFF, got %q", got)
	}
}

func TestAntigravityBuildRequest_SafetyModeFourOffFallsBackWhenOnlyCivicSupplied(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{
			"contents":       []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
			"safetySettings": []any{map[string]any{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := safetyBuildRequest(t, &config.Config{Antigravity: config.AntigravityConfig{SafetyMode: "four_off"}}, payload)
	settings := safetySettingsByCategory(t, wire)
	for _, category := range []string{"HARM_CATEGORY_HARASSMENT", "HARM_CATEGORY_HATE_SPEECH", "HARM_CATEGORY_SEXUALLY_EXPLICIT", "HARM_CATEGORY_DANGEROUS_CONTENT"} {
		if got := settings[category]; got != "OFF" {
			t.Fatalf("category %s = %q, want OFF fallback", category, got)
		}
	}
}

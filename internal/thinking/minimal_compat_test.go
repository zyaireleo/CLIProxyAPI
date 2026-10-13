package thinking_test

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestGemini38MinimalCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, model, metadata, from, level, want string
		userDefined                              bool
	}{
		{name: "native", model: "gemini-3.8-flash-high", from: "gemini", level: "minimal", want: "low"},
		{name: "antigravity", model: "gemini-3.8-flash-high", from: "antigravity", level: "minimal", want: "low"},
		{name: "openai source", model: "gemini-3.8-flash", from: "openai", level: "minimal", want: "low"},
		{name: "alias", model: "custom-flash", metadata: "gemini-3.8-flash-high", from: "gemini", level: "minimal", want: "low"},
		{name: "configured alias", model: "custom-flash", metadata: "gemini-3.8-flash-high", from: "gemini", level: "minimal", want: "low", userDefined: true},
		{name: "suffix wins", model: "gemini-3.8-flash-high(high)", from: "gemini", level: "minimal", want: "high"},
		{name: "minimal suffix", model: "gemini-3.8-flash-high(minimal)", from: "gemini", level: "high", want: "low"},
		{name: "low preserved", model: "gemini-3.8-flash-high", from: "gemini", level: "low", want: "low"},
		{name: "medium preserved", model: "gemini-3.8-flash-high", from: "gemini", level: "medium", want: "medium"},
		{name: "high preserved", model: "gemini-3.8-flash-high", from: "gemini", level: "high", want: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := gemini38ModelInfo(thinking.ParseSuffix(tc.model).ModelName, "antigravity", []string{"low", "medium", "high"})
			info.MetadataModelID, info.UserDefined = tc.metadata, tc.userDefined
			body := []byte(fmt.Sprintf(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":16,"thinkingConfig":{"thinkingLevel":%q}}}}`, tc.level))
			var source []byte
			if tc.from == "openai" {
				source = []byte(`{"reasoning_effort":"minimal"}`)
			}
			out, err := thinking.ApplyThinkingWithModelInfo(body, source, tc.model, tc.from, "antigravity", "antigravity", info)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != tc.want {
				t.Fatalf("want %s, got %s: %s", tc.want, got, out)
			}
			if gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
				t.Fatalf("level unexpectedly converted to budget: %s", out)
			}
		})
	}
}

func TestGemini38MinimalCompatibilityDoesNotBroadenValidation(t *testing.T) {
	for _, tc := range []struct {
		name, model, provider string
		levels                []string
		wantError             bool
	}{
		{"other model", "gemini-3.7-flash-high", "antigravity", []string{"low", "medium", "high"}, true},
		{"direct Gemini", "gemini-3.8-flash", "gemini", []string{"low", "medium", "high"}, true},
		{"low unavailable", "gemini-3.8-flash-high", "antigravity", []string{"medium", "high"}, true},
		{"minimal supported", "gemini-3.8-flash-high", "antigravity", []string{"minimal", "low", "high"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal"}}}`)
			path := "generationConfig.thinkingConfig.thinkingLevel"
			if tc.provider == "antigravity" {
				body = append(append([]byte(`{"request":`), body...), '}')
				path = "request." + path
			}
			out, err := thinking.ApplyThinkingWithModelInfo(body, nil, tc.model, tc.provider, tc.provider, tc.provider, gemini38ModelInfo(tc.model, tc.provider, tc.levels))
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && gjson.GetBytes(out, path).String() != "minimal" {
				t.Fatalf("supported minimal changed: %s", out)
			}
		})
	}
}

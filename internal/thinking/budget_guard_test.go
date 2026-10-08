package thinking_test

import (
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/antigravity"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/gemini"
	"github.com/tidwall/gjson"
)

// S-12 probe guard: small output budget (e.g. A6 health probes with max_tokens=16)
// must suppress thinking so the model can emit a final answer within the budget.
// Guard applies only when the request carries no explicit thinking configuration,
// only for gemini/antigravity targets, and only when the model supports the
// "minimal" level.

func budgetGuardModelInfo(levels []string) *registry.ModelInfo {
	return &registry.ModelInfo{
		ID: "gemini-3.6-flash-high",
		Thinking: &registry.ThinkingSupport{
			Min:    1,
			Max:    65535,
			Levels: levels,
		},
	}
}

func TestBudgetGuardFiresAntigravity(t *testing.T) {
	body := []byte(`{"model":"gemini-3.6-flash","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String()
	if got != "minimal" {
		t.Fatalf("want thinkingLevel=minimal, got=%q body=%s", got, out)
	}
}

func TestBudgetGuardFiresGemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "gemini", "gemini",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").String()
	if got != "minimal" {
		t.Fatalf("want thinkingLevel=minimal, got=%q body=%s", got, out)
	}
}

func TestBudgetGuardIgnoresNormalBudget(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":2000}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("guard must not touch normal-budget requests: %s", out)
	}
}

func TestBudgetGuardRespectsExplicitConfig(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16,"thinkingConfig":{"thinkingLevel":"high"}}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String()
	if got != "high" {
		t.Fatalf("explicit config must win, want high got=%q body=%s", got, out)
	}
}

func TestBudgetGuardRespectsSuffix(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash(low)", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String()
	if got != "low" {
		t.Fatalf("suffix must win, want low got=%q body=%s", got, out)
	}
}

func TestBudgetGuardSkipsMissingBudget(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("guard must not fire without an output budget: %s", out)
	}
}

func TestBudgetGuardSkipsOtherTargets(t *testing.T) {
	body := []byte(`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "claude-x", "claude", "claude", "claude",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("guard must not apply to claude target: %s", out)
	}
}

func TestBudgetGuardSkipsNonThinkingModel(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	info := &registry.ModelInfo{ID: "gemini-plain", Thinking: nil}
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-plain", "gemini", "antigravity", "antigravity", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("guard must not fire for non-thinking models: %s", out)
	}
}

func TestBudgetGuardSkipsModelWithoutMinimalLevel(t *testing.T) {
	// e.g. gemini-pro-agent: levels [low,medium,high]; the pipeline cannot express
	// a true zero-thinking budget for level models, so the guard must no-op.
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-pro-agent", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("guard must not inject a level the model does not support: %s", out)
	}
}

func TestBudgetGuardReadsSourceBodyBudget(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	source := []byte(`{"model":"gemini-3.6-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, source, "gemini-3.6-flash", "openai", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String()
	if got != "minimal" {
		t.Fatalf("guard must read source openai max_tokens, want minimal got=%q body=%s", got, out)
	}
}

func TestBudgetGuardBoundary1024(t *testing.T) {
	fire := func(budget int) bool {
		body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":` +
			strconv.Itoa(budget) + `}}}`)
		out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity",
			budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
		if err != nil {
			t.Fatalf("ApplyThinking error: %v", err)
		}
		return gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String() == "minimal"
	}
	if !fire(1024) {
		t.Fatal("budget=1024 must trigger the guard (threshold is inclusive)")
	}
	if fire(1025) {
		t.Fatal("budget=1025 must not trigger the guard")
	}
}

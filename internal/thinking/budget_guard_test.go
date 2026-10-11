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
// Guard applies only when the request carries no explicit thinking configuration
// and only for gemini/antigravity targets. Gemini 3.8 uses thinkingLevel=low;
// other Min==1 models retain thinkingBudget=1.

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

func gemini38ModelInfo(id, modelType string, levels []string) *registry.ModelInfo {
	return &registry.ModelInfo{
		ID:   id,
		Type: modelType,
		Thinking: &registry.ThinkingSupport{
			Min:            1,
			Max:            65535,
			DynamicAllowed: true,
			Levels:         levels,
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
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("want thinkingBudget=1, got=%v body=%s", got, out)
	}
}

func TestBudgetGuardFiresGemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "gemini", "gemini",
		budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("want thinkingBudget=1, got=%v body=%s", got, out)
	}
}

func TestBudgetGuardUsesLowForGemini38Antigravity(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.8-flash-high", "gemini", "antigravity", "antigravity",
		gemini38ModelInfo("gemini-3.8-flash-high", "antigravity", []string{"low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	level := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String()
	if level != "low" {
		t.Fatalf("want thinkingLevel=low, got=%q body=%s", level, out)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("3.8 level guard must not emit thinkingBudget: %s", out)
	}
}

func TestBudgetGuardUsesLowForGemini38Gemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.8-flash", "gemini", "gemini", "gemini",
		gemini38ModelInfo("gemini-3.8-flash", "gemini", []string{"low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	level := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").String()
	if level != "low" {
		t.Fatalf("want thinkingLevel=low, got=%q body=%s", level, out)
	}
	if gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("3.8 level guard must not emit thinkingBudget: %s", out)
	}
}

func TestBudgetGuardGemini38ReadsOpenAISourceBudget(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	source := []byte(`{"model":"gemini-3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, source, "gemini-3.8-flash", "openai", "gemini", "gemini",
		gemini38ModelInfo("gemini-3.8-flash", "gemini", []string{"low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	level := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").String()
	if level != "low" {
		t.Fatalf("want source-budget guard to emit thinkingLevel=low, got=%q body=%s", level, out)
	}
}

func TestBudgetGuardGemini38UserDefinedPathsPreserveLow(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		toFormat   string
		levelPath  string
		budgetPath string
	}{
		{
			name:       "antigravity",
			body:       `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
			toFormat:   "antigravity",
			levelPath:  "request.generationConfig.thinkingConfig.thinkingLevel",
			budgetPath: "request.generationConfig.thinkingConfig.thinkingBudget",
		},
		{
			name:       "gemini",
			body:       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			toFormat:   "gemini",
			levelPath:  "generationConfig.thinkingConfig.thinkingLevel",
			budgetPath: "generationConfig.thinkingConfig.thinkingBudget",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := gemini38ModelInfo("user-gemini-alias", tt.toFormat, []string{"low", "medium", "high"})
			info.MetadataModelID = "gemini-3.8-flash-high"
			info.UserDefined = true
			source := []byte(`{"model":"user-gemini-alias","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			out, err := thinking.ApplyThinkingWithModelInfo([]byte(tt.body), source, "user-gemini-alias", "openai", tt.toFormat, tt.toFormat, info)
			if err != nil {
				t.Fatalf("ApplyThinking error: %v", err)
			}
			level := gjson.GetBytes(out, tt.levelPath).String()
			if level != "low" {
				t.Fatalf("want user-defined guard to emit thinkingLevel=low, got=%q body=%s", level, out)
			}
			if gjson.GetBytes(out, tt.budgetPath).Exists() {
				t.Fatalf("user-defined 3.8 guard must not emit thinkingBudget: %s", out)
			}
		})
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
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.8-flash-high", "gemini", "antigravity", "antigravity",
		gemini38ModelInfo("gemini-3.8-flash-high", "antigravity", []string{"low", "medium", "high"}))
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
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.8-flash-high(low)", "gemini", "antigravity", "antigravity",
		gemini38ModelInfo("gemini-3.8-flash-high", "antigravity", []string{"low", "medium", "high"}))
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

func TestBudgetGuardBudget1FallbackForNoMinimal(t *testing.T) {
	// e.g. gemini-pro-agent: levels [low,medium,high], min=1; keep the existing
	// budget=1 guard for non-3.8 models even when minimal is absent.
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-pro-agent", "gemini", "antigravity", "antigravity",
		budgetGuardModelInfo([]string{"low", "medium", "high"}))
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("want thinkingBudget=1 fallback, got=%v body=%s", got, out)
	}
}

func TestBudgetGuardKeepsBudget1ForGemini31FlashLite(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	info := &registry.ModelInfo{
		ID: "gemini-3.1-flash-lite",
		Thinking: &registry.ThinkingSupport{
			Min:            1,
			Max:            65535,
			DynamicAllowed: true,
			Levels:         []string{"minimal", "low", "medium", "high"},
		},
	}
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, info.ID, "gemini", "antigravity", "antigravity", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("want thinkingBudget=1 for Gemini 3.1 Flash Lite, got=%v body=%s", got, out)
	}
}

func TestBudgetGuardGemini38WithoutLowDoesNotFallback(t *testing.T) {
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	info := gemini38ModelInfo("gemini-3.8-flash-high", "antigravity", []string{"minimal", "medium", "high"})
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, info.ID, "gemini", "antigravity", "antigravity", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("3.8 without low must not fall back to budget or another level: %s", out)
	}
}

func TestBudgetGuardGemini38SkipsInvalidBudgets(t *testing.T) {
	for _, budget := range []int{0, 1025} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			body := []byte(`{"generationConfig":{"maxOutputTokens":` + strconv.Itoa(budget) + `}}`)
			info := gemini38ModelInfo("gemini-3.8-flash", "gemini", []string{"low", "medium", "high"})
			out, err := thinking.ApplyThinkingWithModelInfo(body, nil, info.ID, "gemini", "gemini", "gemini", info)
			if err != nil {
				t.Fatalf("ApplyThinking error: %v", err)
			}
			if gjson.GetBytes(out, "generationConfig.thinkingConfig").Exists() {
				t.Fatalf("budget=%d must not trigger the 3.8 guard: %s", budget, out)
			}
		})
	}
}

func TestBudgetGuardPreservesExplicitBudgetForGemini38(t *testing.T) {
	body := []byte(`{"generationConfig":{"maxOutputTokens":16,"thinkingConfig":{"thinkingBudget":128}}}`)
	info := gemini38ModelInfo("gemini-3.8-flash", "gemini", []string{"low", "medium", "high"})
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, info.ID, "gemini", "gemini", "gemini", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 128 {
		t.Fatalf("explicit thinkingBudget must win, got=%v body=%s", got, out)
	}
	if gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").Exists() {
		t.Fatalf("explicit budget must not be replaced with thinkingLevel: %s", out)
	}
}

func TestBudgetGuardSkipsHighMinModel(t *testing.T) {
	// min>1 (e.g. 2.5-pro min=128): budget=min would still exceed a 16-token
	// output budget, and smaller budgets are invalid — guard must no-op.
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	info := &registry.ModelInfo{
		ID: "gemini-2.5-pro",
		Thinking: &registry.ThinkingSupport{
			Min:    128,
			Max:    32768,
			Levels: []string{"low", "medium", "high"},
		},
	}
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-2.5-pro", "gemini", "antigravity", "antigravity", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	if gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("guard must not fire when no valid suppression exists: %s", out)
	}
}

func TestBudgetGuardFiresUserDefinedPath(t *testing.T) {
	// Production: config alias fork:true registers these models as UserDefined,
	// routing them through applyUserDefinedModel — the guard must fire there too.
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}}`)
	info := budgetGuardModelInfo([]string{"minimal", "low", "medium", "high"})
	info.UserDefined = true
	out, err := thinking.ApplyThinkingWithModelInfo(body, nil, "gemini-3.6-flash", "gemini", "antigravity", "antigravity", info)
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("user-defined path must honor the guard, want budget=1 got=%v body=%s", got, out)
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
	got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if got != 1 {
		t.Fatalf("guard must read source openai max_tokens, want budget=1 got=%v body=%s", got, out)
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
		return gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int() == 1
	}
	if !fire(1024) {
		t.Fatal("budget=1024 must trigger the guard (threshold is inclusive)")
	}
	if fire(1025) {
		t.Fatal("budget=1025 must not trigger the guard")
	}
}

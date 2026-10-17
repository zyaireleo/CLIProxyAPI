package models

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCurrentCodexCatalogUsesCompleteTemplatesAndOmitsRetirements(t *testing.T) {
	var available []map[string]any
	for _, model := range registry.GetCodexProModels() {
		available = append(available, map[string]any{"id": model.ID})
	}
	response := BuildResponseForClient(available, func(string) []string { return []string{"codex"} }, false, "0.162.1")
	models, ok := response["models"].([]map[string]any)
	if !ok || len(models) != len(available) {
		t.Fatalf("client catalog has %d models, want %d", len(models), len(available))
	}
	byID := make(map[string]map[string]any)
	for _, model := range models {
		byID[stringModelValue(model, "slug")] = model
	}
	for _, id := range []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna"} {
		model := byID[id]
		if model == nil || stringModelValue(model, "base_instructions") == "" {
			t.Fatalf("current model %q has no complete client template", id)
		}
		if got := stringModelValue(model, "comp_hash"); got != "3000" {
			t.Errorf("model %q used generic fallback comp_hash %q", id, got)
		}
	}
	for _, id := range []string{"gpt-5.5", "gpt-5.3-codex-spark"} {
		if byID[id] != nil {
			t.Errorf("retired OAuth model %q leaked through client templates", id)
		}
	}
}

package registry

import "testing"

func TestCodexOAuthCatalogCurrentModelsAndRetirements(t *testing.T) {
	plans := map[string][]*ModelInfo{
		"free": GetCodexFreeModels(),
		"plus": GetCodexPlusModels(),
		"team": GetCodexTeamModels(),
		"pro":  GetCodexProModels(),
	}
	for plan, models := range plans {
		t.Run(plan, func(t *testing.T) {
			seen := make(map[string]bool)
			for _, model := range models {
				if model == nil || model.ID == "" || seen[model.ID] {
					t.Fatalf("invalid or duplicate model: %#v", model)
				}
				seen[model.ID] = true
			}
			for _, retired := range []string{"gpt-5.5", "gpt-5.3-codex-spark"} {
				if seen[retired] {
					t.Errorf("retired OAuth model %q is still advertised", retired)
				}
			}
			if !seen["gpt-6-luna"] {
				t.Error("missing current Luna model")
			}
			for _, paid := range []string{"gpt-6.1-sol", "gpt-6-sol"} {
				if seen[paid] != (plan != "free") {
					t.Errorf("paid model %q eligibility does not match plan %q", paid, plan)
				}
			}
			for _, retained := range []string{"gpt-5.6-terra", "gpt-5.6-luna", "codex-auto-review", "gpt-image-2.5"} {
				if !seen[retained] {
					t.Errorf("existing supported model %q was lost", retained)
				}
			}
		})
	}
}

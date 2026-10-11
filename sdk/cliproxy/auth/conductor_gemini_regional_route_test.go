package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
)

func TestGeminiRegionalFaultDoesNotDisableCredentialForOtherExit(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	credential := &Auth{ID: t.Name(), Provider: "antigravity"}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	fault := &geminiresponse.Error{Code: "upstream_region_unavailable", Message: "Regional route rejected", Status: 503, RouteFault: true}
	if isRequestInvalidError(fault) {
		t.Fatal("regional fault incorrectly prevents alternate credential recovery")
	}
	manager.MarkResult(context.Background(), Result{AuthID: credential.ID, Provider: credential.Provider, Model: "gemini-3.8-flash", Error: resultErrorFromError(fault)})
	updated, ok := manager.GetByID(credential.ID)
	if !ok || updated == nil {
		t.Fatal("credential disappeared")
	}
	if state := existingModelState(updated, "gemini-3.8-flash"); state != nil && state.Unavailable {
		t.Fatal("a regional exit rejection disabled the model on other exits")
	}
	if !updated.NextRetryAfter.IsZero() || updated.Unavailable {
		t.Fatal("regional route rejection disabled the whole credential")
	}
}

package helps

import (
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"testing"
)

func TestGeminiGateContract(t *testing.T) {
	gate := NewGeminiResponseGate(false, false)
	metadata := []byte(`{"usageMetadata":{"promptTokenCount":3}}`)
	if chunks, err := gate.Observe(metadata); err != nil || len(chunks) != 0 {
		t.Fatalf("metadata escaped gate: %q %v", chunks, err)
	}
	if chunks, err := gate.Finish(); err == nil || len(chunks) != 0 {
		t.Fatalf("empty stream accepted: %q %v", chunks, err)
	}
	gate = NewGeminiResponseGate(false, false)
	gate.Observe(metadata)
	chunks, err := gate.Observe([]byte(`{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`))
	if err != nil || len(chunks) != 2 || string(chunks[0]) != string(metadata) {
		t.Fatalf("order=%q error=%v", chunks, err)
	}
	if _, err = gate.Observe([]byte(`{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}]}`)); err == nil {
		t.Fatal("late policy did not terminate stream")
	}
	var outcome *geminiresponse.Error
	if !errors.As(err, &outcome) || outcome.Code != "content_policy_block" {
		t.Fatal(err)
	}
	native := NewGeminiResponseGate(true, false)
	if chunks, err = native.Observe([]byte(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`)); err != nil || len(chunks) != 1 {
		t.Fatalf("native policy changed: %q %v", chunks, err)
	}
}

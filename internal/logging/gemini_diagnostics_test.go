package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestGeminiDiagnosticFormatterRetainsAttemptFieldsAndOmitsPayload(t *testing.T) {
	for _, message := range []string{"gemini_generation_attempt", "gemini_generation_outcome"} {
		entry := log.NewEntry(log.New()).WithFields(log.Fields{
			"request_id": "request-123", "sub2api_trace_id": "client-123", "native_model": "gemini-3.8-flash-high",
			"attempt": 4, "attempts": 4, "source": "upstream", "status": 200, "outcome": "success",
			"first_content_ms": 321, "termination_reason": "STOP", "auth_index": "opaque-index", "exit_fingerprint": "opaque-exit",
			"prompt": "PRIVATE_PROMPT", "api_key": "PRIVATE_KEY", "image": "PRIVATE_IMAGE", "tool_arguments": "PRIVATE_TOOL_ARGUMENTS",
		})
		entry.Message = message
		formatted, err := (&LogFormatter{}).Format(entry)
		if err != nil {
			t.Fatal(err)
		}
		text := string(formatted)
		for _, expected := range []string{"[request-123]", "sub2api_trace_id=client-123", "attempt=4", "attempts=4", `native_model="gemini-3.8-flash-high"`, "status=200", "first_content_ms=321", `termination_reason="STOP"`} {
			if !strings.Contains(text, expected) {
				t.Fatalf("formatted diagnostic omitted %s", expected)
			}
		}
		if strings.Contains(text, "PRIVATE_") {
			t.Fatal("formatter exposed request content or credentials")
		}
	}
}

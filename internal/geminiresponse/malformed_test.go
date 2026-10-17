package geminiresponse

import (
	"errors"
	"testing"
)

func TestMalformedFunctionCallIsExplicitAndNotReplayable(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, parts := range []string{`[]`, `[{"thought":true,"text":"private reasoning"}]`, `[{"text":"partial answer"}]`} {
			var summary Summary
			summary.Observe([]byte(`{"candidates":[{"content":{"parts":` + parts + `},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`))
			var outcome *Error
			if !errors.As(summary.Failure(false, native), &outcome) || outcome.Code != "upstream_malformed_function_call" || outcome.Status != 502 || !outcome.Stop {
				t.Fatalf("native=%v outcome=%+v", native, outcome)
			}
			if summary.Finish != "MALFORMED_FUNCTION_CALL" {
				t.Fatal("raw finish reason lost")
			}
		}
	}
}

func TestSummarySeparatesTextThinkingAndToolWithoutRetainingContent(t *testing.T) {
	var summary Summary
	summary.Observe([]byte(`{"candidates":[{"content":{"parts":[{"thought":true,"text":"secret"}]}},{"content":{"parts":[{"functionCall":{"name":"tool","args":{"secret":"hidden"}}}]}}]}`))
	if summary.Text || !summary.Thought || !summary.Tool || !summary.Answer || summary.Candidates != 2 {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.Failure(false, false) != nil {
		t.Fatal("valid tool call incorrectly rejected")
	}
}

func TestNativeErrorRetainsMachineReason(t *testing.T) {
	var summary Summary
	summary.Observe([]byte(`{"error":{"code":502,"status":"INTERNAL","reason":"upstream_empty_response","message":"no usable content"}}`))
	if summary.ErrorCode != "upstream_empty_response" {
		t.Fatalf("lost semantic code: %s", summary.ErrorCode)
	}
}

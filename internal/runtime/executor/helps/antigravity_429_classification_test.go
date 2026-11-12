package helps

import (
	"testing"
)

func TestAntigravityFailureClassRequiresStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"bare", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, "upstream_429_ambiguous"},
		{"words are not evidence", 429, `{"error":{"message":"quota exhausted safety"}}`, "upstream_429_ambiguous"},
		{"hint alone", 429, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"20s"}]}}`, "upstream_429_ambiguous"},
		{"rate", 429, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`, "upstream_rate_limited"},
		{"quota", 429, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`, "upstream_quota_exhausted"},
		{"quota violations", 429, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"subject":"project"}]}]}}`, "upstream_quota_exhausted"},
		{"policy HTTP200", 200, `{"response":{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}}`, "upstream_request_rejected"},
		{"output policy", 200, `{"candidates":[{"finishReason":"SAFETY"}]}`, "upstream_request_rejected"},
		{"real ToS403", 403, `{"error":{"code":403,"message":"account disabled for terms of service violation"}}`, ""},
		{"normal", 200, `{"candidates":[{"finishReason":"STOP"}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AntigravityFailureClass(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

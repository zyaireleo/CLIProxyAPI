package geminiresponse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGenerationOutcomeContract(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		native, image    bool
	}{
		{"empty parts", `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`, "upstream_empty_response", false, false},
		{"metadata", `{"usageMetadata":{"promptTokenCount":12}}`, "upstream_empty_response", false, false},
		{"tool", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, "", false, false},
		{"text", `{"response":{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}}`, "", false, false},
		{"policy", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, "content_policy_block", false, false},
		{"native policy", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, "", true, false},
		{"thinking budget", `{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking"}]},"finishReason":"MAX_TOKENS"}]}`, "upstream_output_limit", false, false},
		{"native budget", `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, "", true, false},
		{"image text only", `{"candidates":[{"content":{"parts":[{"text":"cannot draw"}]}}]}`, "upstream_empty_response", false, true},
		{"image", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLbtAAAAABJRU5ErkJggg=="}}]}}]}`, "", false, true},
		{"invalid image", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"bad!"}}]}}]}`, "upstream_empty_response", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var summary Summary
			summary.Observe([]byte(tc.body))
			err := summary.Failure(tc.image, tc.native)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var outcome *Error
			if !errors.As(err, &outcome) || outcome.Code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
			if tc.code == "content_policy_block" && (!outcome.IsRequestScoped() || outcome.StatusCode() != 400) {
				t.Fatal("policy must stop retry without harming credentials")
			}
		})
	}
}

func TestSharedBudgetBoundsActualHTTPCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(429) }))
	defer server.Close()
	ctx := WithBudget(context.Background(), 4)
	for i := 0; i < 8; i++ {
		request, _ := http.NewRequestWithContext(ctx, "POST", server.URL, nil)
		response, err := Do(context.WithValue(ctx, struct{}{}, i), server.Client(), request)
		if i < 4 {
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		} else if err == nil {
			t.Fatal("fifth generation was sent")
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("actual calls=%d", calls.Load())
	}
	cancelled, cancel := context.WithCancel(WithBudget(context.Background(), 4))
	cancel()
	if _, err := TakeAttempt(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if GetBudget(WithBudget(context.Background(), 0)) != nil {
		t.Fatal("legacy mode must not introduce a limit")
	}
}

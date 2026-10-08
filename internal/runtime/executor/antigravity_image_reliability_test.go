package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestFlashImageNativeHTTPClassificationAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		textOnly         bool
	}{
		{"empty", `{"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":283,"totalTokenCount":283}}}`, "upstream_empty_image", 200, false},
		{"invalid-image", `{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}}`, "upstream_empty_image", 200, false},
		{"policy", `{"response":{"promptFeedback":{"blockReason":"SAFETY"}}}`, "content_policy_violation", 200, false},
		{"truncated", `{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}]}}`, "upstream_output_incomplete", 200, false},
		{"text", `{"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}}`, "", 200, true},
		{"quota", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"model":"gemini-3.1-flash-image"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12613s"}]}}`, "upstream_quota_exhausted", 429, false},
		{"input", `{"error":{"message":"private reference input"}}`, "invalid_request_error", 400, false},
		{"authentication", `{"error":{"message":"private credential identity"}}`, "upstream_authentication_error", 401, false},
		{"access", `{"error":{"message":"private credential identity"}}`, "upstream_access_denied", 403, false},
		{"capacity", `{"error":{"message":"private upstream context"}}`, "upstream_capacity_unavailable", 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					fmt.Fprintln(w, "data: "+tc.body)
				} else {
					io.WriteString(w, tc.body)
				}
			}))
			defer srv.Close()
			e := NewAntigravityExecutor(&config.Config{RequestRetry: 3})
			a := &coreauth.Auth{ID: "image-http-" + tc.name, Attributes: map[string]string{"base_url": srv.URL}, Metadata: map[string]any{"access_token": "test", "project_id": "test", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
			modalities := `["IMAGE","TEXT"]`
			if tc.textOnly {
				modalities = `["TEXT"]`
			}
			payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"draw a circle"}]}],"generationConfig":{"responseModalities":` + modalities + `}}`)
			ctx := core.WithImageGenerationBudget(context.Background())
			for i := 0; i < 5; i++ {
				_, err := e.Execute(ctx, a, core.Request{Model: "gemini-3.1-flash-image", Payload: payload}, core.Options{SourceFormat: translator.FormatGemini, OriginalRequest: payload})
				if tc.textOnly {
					if err != nil {
						t.Fatal(err)
					}
					break
				}
				want := tc.code
				if i == 4 {
					want = "upstream_image_budget_exhausted"
				}
				if err == nil || gjson.Get(err.Error(), "error.code").String() != want {
					t.Fatalf("attempt=%d err=%v want=%s", i, err, want)
				}
				if gjson.Get(err.Error(), "error.message").String() == "private credential identity" || gjson.Get(err.Error(), "error.message").String() == "private reference input" {
					t.Fatal("private upstream context was exposed")
				}
				if tc.name == "quota" && i < 4 {
					var failure *core.ImageFailure
					if !errors.As(err, &failure) || failure.Cooldown == nil || *failure.Cooldown != 12613*time.Second {
						t.Fatal("quota reset lost")
					}
				}
			}
			wantCalls := int32(4)
			if tc.textOnly {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("wire calls=%d want=%d", calls.Load(), wantCalls)
			}
		})
	}
}

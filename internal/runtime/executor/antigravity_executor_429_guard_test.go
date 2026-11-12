package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityGuardCapsActualGenerationSends(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "gemini-3-pro-high"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", model, stream), func(t *testing.T) {
				sends := 0
				ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					sends++
					return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"45"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`))}, nil
				}))
				exec := NewAntigravityExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{ID: "guard-" + model, Provider: "antigravity", Metadata: map[string]any{"access_token": "test-token", "project_id": "test-project", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}}
				req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream}
				for i := 0; i < 3; i++ {
					var err error
					if stream {
						_, err = exec.ExecuteStream(ctx, auth, req, opts)
					} else {
						_, err = exec.Execute(ctx, auth, req, opts)
					}
					var guard *cliproxyexecutor.AntigravityGuardError
					if !errors.As(err, &guard) {
						t.Fatalf("attempt %d lost guard: %v", i, err)
					}
					if i < 2 && (guard.RetryAfter() == nil || *guard.RetryAfter() != 45*time.Second) {
						t.Fatal("HTTP Retry-After hint lost")
					}
					if i < 2 && guard.Class != cliproxyexecutor.GuardClassAmbiguous429 {
						t.Fatalf("bare 429 classified as %q", guard.Class)
					}
					if i == 2 && guard.Class != cliproxyexecutor.GuardClassBudget {
						t.Fatalf("third send not stopped: %v", err)
					}
				}
				if sends != 2 {
					t.Fatalf("actual HTTP sends=%d want 2", sends)
				}
			})
		}
	}
}

func TestAntigravityGuardClassifiesSSEError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			sends := 0
			ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				sends++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"message\":\"upstream unavailable\"}}\n\n"))}, nil
			}))
			exec := NewAntigravityExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "sse-guard", Provider: "antigravity", Metadata: map[string]any{"access_token": "test-token", "project_id": "test-project", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339)}}
			req := cliproxyexecutor.Request{Model: "gemini-3-pro-high", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream}
			var err error
			if stream {
				var result *cliproxyexecutor.StreamResult
				result, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil && result != nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = exec.Execute(ctx, auth, req, opts)
			}
			var guard *cliproxyexecutor.AntigravityGuardError
			if !errors.As(err, &guard) || guard.Class != cliproxyexecutor.GuardClassAmbiguous429 || guard.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("SSE error lost trusted classification: %v", err)
			}
			if sends != 1 || cliproxyexecutor.AntigravityGuardFromContext(ctx).Attempts() != 1 {
				t.Fatalf("guard budget counted one transport more than once: sends=%d attempts=%d", sends, cliproxyexecutor.AntigravityGuardFromContext(ctx).Attempts())
			}
		})
	}
}

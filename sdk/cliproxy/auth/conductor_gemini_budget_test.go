package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type geminiBudgetExecutor struct {
	antigravityCreditsFallbackExecutor
	url string
}

func (e *geminiBudgetExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url, nil)
	resp, err := geminiresponse.Do(ctx, http.DefaultClient, req)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var summary geminiresponse.Summary
	summary.Observe(body)
	return cliproxyexecutor.Response{Payload: body}, summary.Failure(false, false)
}

func (e *geminiBudgetExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.Execute(ctx, auth, req, opts)
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: resp.Payload, Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func TestGeminiManagerTotalGenerationBudget(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, policy := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/policy=%t", streaming, policy), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					if policy {
						_, _ = io.WriteString(w, "{\"promptFeedback\":{\"blockReason\":\"PROHIBITED_CONTENT\"}}")
					} else {
						_, _ = io.WriteString(w, "{\"candidates\":[{\"content\":{\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":4}}")
					}
				}))
				defer server.Close()
				m := NewManager(nil, nil, nil)
				m.SetConfig(&internalconfig.Config{AntigravityGeminiMaxAttempts: 4})
				m.SetRetryConfig(3, 0, 0)
				m.RegisterExecutor(&geminiBudgetExecutor{url: server.URL})
				const model = "gemini-budget-contract"
				for i := 0; i < 8; i++ {
					id := fmt.Sprintf("gemini-budget-%d", i)
					registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "antigravity"}); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if streaming {
					var result *cliproxyexecutor.StreamResult
					result, err = m.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
					if err == nil && result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = m.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				}
				want := int32(4)
				if policy {
					want = 1
				}
				if err == nil || calls.Load() != want {
					t.Fatalf("calls=%d want=%d error=%v", calls.Load(), want, err)
				}
				if policy {
					for _, a := range m.List() {
						if a.Unavailable || !a.NextRetryAfter.IsZero() {
							t.Fatalf("policy affected credential health: %+v", a.ModelStates)
						}
					}
				}
			})
		}
	}
}

func TestGeminiQuotaCooldownContract(t *testing.T) {
	now := time.Now()
	q := QuotaState{}
	for _, want := range []time.Duration{30, 60, 120, 240, 300, 300} {
		next, level := geminiQuotaCooldownAfterFailure(q, now)
		if next.Sub(now) != want*time.Second {
			t.Fatalf("cooldown=%v want=%vs", next.Sub(now), want)
		}
		q = QuotaState{NextRecoverAt: next, BackoffLevel: level}
		duplicate, duplicateLevel := geminiQuotaCooldownAfterFailure(q, now)
		if !duplicate.Equal(next) || duplicateLevel != level {
			t.Fatal("concurrent quota failure advanced the cooldown")
		}
		now = next.Add(time.Second)
	}
	q.NextRecoverAt = now.Add(24 * time.Hour)
	if next, _ := geminiQuotaCooldownAfterFailure(q, now); !next.Equal(q.NextRecoverAt) {
		t.Fatal("longer known quota recovery deadline shortened")
	}
}

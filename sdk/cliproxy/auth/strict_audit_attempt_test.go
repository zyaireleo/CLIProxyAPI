package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type strictAuditBudgetExecutor struct {
	antigravityCreditsFallbackExecutor
	url string
}

func (e *strictAuditBudgetExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, nil)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resp, err := geminiresponse.Do(ctx, http.DefaultClient, req)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	return cliproxyexecutor.Response{}, &geminiresponse.Error{Code: "fixture_failure", Message: "fixture", Status: resp.StatusCode}
}

func (e *strictAuditBudgetExecutor) ExecuteStream(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, err := e.Execute(ctx, a, r, o)
	return nil, err
}

func TestStrictAuditBudgetPreventsCredentialAndRoundRetries(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, status := range []int{403, 429, 503} {
			t.Run(fmt.Sprintf("stream=%t/status=%d", streaming, status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(status)
				}))
				defer server.Close()
				m := NewManager(nil, nil, nil)
				m.SetConfig(&internalconfig.Config{AntigravityGeminiMaxAttempts: 4})
				m.SetRetryConfig(3, 0, 0)
				m.RegisterExecutor(&strictAuditBudgetExecutor{url: server.URL})
				const model = "gemini-strict-audit-budget"
				for i := 0; i < 4; i++ {
					id := fmt.Sprintf("strict-audit-budget-%d", i)
					registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "antigravity"}); err != nil {
						t.Fatal(err)
					}
				}
				ctx := geminiresponse.WithBudget(context.Background(), 1)
				var err error
				if streaming {
					_, err = m.ExecuteStream(ctx, []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				} else {
					_, err = m.Execute(ctx, []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				}
				if err == nil {
					t.Fatal("expected upstream failure")
				}
				if calls.Load() != 1 {
					t.Fatalf("calls=%d, want exactly one", calls.Load())
				}
			})
		}
	}
}

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type antigravityBootstrapExecutor struct{ bootstrapStreamExecutor }

func (*antigravityBootstrapExecutor) Identifier() string { return "antigravity" }

func TestAntigravityGuardSurvivesHandlerBootstrapRetries(t *testing.T) {
	for _, class := range []string{coreexecutor.GuardClassAmbiguous429, coreexecutor.GuardClassPolicy} {
		t.Run(class, func(t *testing.T) {
			var sends atomic.Int32
			executor := &antigravityBootstrapExecutor{}
			executor.stream = func(ctx context.Context, _ int) (*coreexecutor.StreamResult, error) {
				if err := coreexecutor.ConsumeAntigravityAttempt(ctx, "gemini-guard-bootstrap"); err != nil {
					return nil, err
				}
				sends.Add(1)
				chunks := make(chan coreexecutor.StreamChunk, 2)
				// The manager has accepted a stream, but a handler interceptor drops
				// its first chunk. A later failure reaches handler bootstrap retry.
				chunks <- coreexecutor.StreamChunk{Payload: []byte("drop")}
				chunks <- coreexecutor.StreamChunk{Err: coreexecutor.WrapAntigravityGuardFailure(ctx, &coreauth.Error{HTTPStatus: 429, Message: "upstream refused"}, class)}
				close(chunks)
				return &coreexecutor.StreamResult{Chunks: chunks}, nil
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{Antigravity429Guard: true})
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(executor)
			for i := 0; i < 12; i++ {
				id := fmt.Sprintf("guard-bootstrap-%s-%d", class, i)
				_, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "antigravity", Status: coreauth.StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: "gemini-guard-bootstrap"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 10}}, manager)
			handler.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(_ context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
				return pluginapi.StreamChunkInterceptResponse{DropChunk: string(req.Body) == "drop"}
			}})
			data, _, errs := handler.executeStreamWithAuthManagerFormats(context.Background(), "openai", "openai", "gemini-guard-bootstrap", []byte(`{"model":"gemini-guard-bootstrap"}`), "", false, modelExecutionOptions{Headers: http.Header{coreexecutor.GuardContractHeader: {"v1"}}})
			if data != nil {
				for chunk := range data {
					t.Fatalf("unexpected completion: %q", chunk)
				}
			}
			failures := 0
			for err := range errs {
				if err != nil {
					failures++
				}
			}
			want := int32(2)
			if class == coreexecutor.GuardClassPolicy {
				want = 1
			}
			if sends.Load() != want || failures != 1 {
				t.Fatalf("sends=%d want %d; failures=%d", sends.Load(), want, failures)
			}
		})
	}
}

package helps

import (
	"context"
	"net/http"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// WithAntigravityAttemptBudget counts at the transport boundary so HTTP
// redirects also share the generation budget. Credential refresh, quota probes
// and token counting never use this wrapper.
func WithAntigravityAttemptBudget(ctx context.Context, client *http.Client, model string) *http.Client {
	if client == nil || cliproxyexecutor.AntigravityGuardFromContext(ctx) == nil || !cliproxyexecutor.IsAntigravityGuardModel(model) {
		return client
	}
	copyClient := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copyClient.Transport = &antigravityBudgetTransport{base: transport, ctx: ctx, model: model}
	return &copyClient
}

type antigravityBudgetTransport struct {
	base  http.RoundTripper
	ctx   context.Context
	model string
}

func (t *antigravityBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if err := cliproxyexecutor.ConsumeAntigravityAttempt(t.ctx, t.model); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *antigravityBudgetTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
)

func (m *Manager) geminiAttemptContext(ctx context.Context, providers []string, model string) context.Context {
	// Resolve aliases later; only Gemini generation calls consume this context.
	if !hasAntigravityProvider(providers) {
		return ctx
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return ctx
	}
	return geminiresponse.WithBudget(ctx, cfg.AntigravityGeminiMaxAttempts)
}

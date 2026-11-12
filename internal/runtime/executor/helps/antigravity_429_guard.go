package helps

import (
	"context"
	"errors"
	"net/http"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func GuardAntigravityFailure(ctx context.Context, model string, cause error, status int, body []byte, headers ...http.Header) error {
	if cliproxyexecutor.AntigravityGuardFromContext(ctx) == nil || !cliproxyexecutor.IsAntigravityGuardModel(model) {
		return cause
	}
	wrapped := cliproxyexecutor.WrapAntigravityGuardFailure(ctx, cause, AntigravityFailureClass(status, body))
	var guard *cliproxyexecutor.AntigravityGuardError
	if status == http.StatusTooManyRequests && errors.As(wrapped, &guard) {
		now := time.Now()
		for _, header := range headers {
			if until, ok := parseRetryAfterHeader(header.Get("Retry-After"), now); ok && until.After(now) {
				delay := until.Sub(now)
				if guard.RetryHint == nil || delay > *guard.RetryHint {
					guard.RetryHint = &delay
				}
			}
		}
	}
	return wrapped
}

func ObserveAntigravityPolicy(ctx context.Context, model string, body []byte) {
	guard := cliproxyexecutor.AntigravityGuardFromContext(ctx)
	if guard != nil && cliproxyexecutor.IsAntigravityGuardModel(model) && AntigravityFailureClass(http.StatusOK, body) == cliproxyexecutor.GuardClassPolicy {
		guard.ObservePolicyRejection()
	}
}

package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAntigravityGuardSharedBudgetConcurrent(t *testing.T) {
	ctx := WithAntigravityGuard(context.Background())
	ctx = WithAntigravityGuard(ctx)
	var sent atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ConsumeAntigravityAttempt(ctx, "gemini-test") == nil {
				sent.Add(1)
			}
		}()
	}
	wg.Wait()
	if sent.Load() != 2 {
		t.Fatalf("actual sends=%d", sent.Load())
	}
	if ConsumeAntigravityAttempt(ctx, "claude-test") != nil {
		t.Fatal("other models affected")
	}
	err := ConsumeAntigravityAttempt(ctx, "gemini-test")
	var typed *AntigravityGuardError
	if !errors.As(err, &typed) || !typed.IsRequestStop() {
		t.Fatal("budget exhaustion must stop outer retry")
	}
}

func TestAntigravityGuardCancellationDoesNotConsume(t *testing.T) {
	ctx, cancel := context.WithCancel(WithAntigravityGuard(context.Background()))
	cancel()
	if !errors.Is(ConsumeAntigravityAttempt(ctx, "gemini-test"), context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if AntigravityGuardFromContext(ctx).Attempts() != 0 {
		t.Fatal("cancelled call consumed budget")
	}
}

func TestAntigravityGuardPrefixedModelsAndTraceCannotResetBudget(t *testing.T) {
	ctx := WithAntigravityGuardTrace(context.Background(), "trusted-trace-1")
	if err := ConsumeAntigravityAttempt(ctx, "models/gemini-test"); err != nil {
		t.Fatal(err)
	}
	ctx = WithAntigravityGuardTrace(ctx, "replacement-trace")
	guard := AntigravityGuardFromContext(ctx)
	if guard.Attempts() != 1 || guard.GatewayTraceID() != "trusted-trace-1" {
		t.Fatal("nested execution reset request identity or budget")
	}
	if AntigravityGuardFromContext(WithAntigravityGuardTrace(context.Background(), "bad\ntrace")).GatewayTraceID() != "" {
		t.Fatal("invalid trace retained")
	}
}

type guard429Error struct{}

func (guard429Error) Error() string   { return `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}` }
func (guard429Error) StatusCode() int { return 429 }

func TestAntigravityGuardPreservesUpstreamError(t *testing.T) {
	ctx := WithAntigravityGuard(context.Background())
	_ = ConsumeAntigravityAttempt(ctx, "gemini-test")
	_ = ConsumeAntigravityAttempt(ctx, "gemini-test")
	err := WrapAntigravityGuardFailure(ctx, guard429Error{}, GuardClassAmbiguous429).(*AntigravityGuardError)
	if err.Error() != (guard429Error{}).Error() || err.StatusCode() != 429 || !err.IsRequestStop() {
		t.Fatal("upstream error or terminal budget lost")
	}
	if err.SafeResponseHeaders().Get("X-CPA-Health-Effect") != "neutral" {
		t.Fatal("missing health classification")
	}
}

// A prior HTTP 200 policy observation must not neutralize a subsequent
// credential authentication failure or its trusted gateway metadata.
type guardAuthenticationError int

func (e guardAuthenticationError) Error() string   { return "authentication failed" }
func (e guardAuthenticationError) StatusCode() int { return int(e) }

func TestAntigravityGuardPreservesAuthenticationAfterPolicyObservation(t *testing.T) {
	for _, status := range []int{401, 403} {
		ctx := WithAntigravityGuard(context.Background())
		AntigravityGuardFromContext(ctx).ObservePolicyRejection()
		err := WrapAntigravityGuardFailure(ctx, guardAuthenticationError(status), GuardClassPolicy)
		var guarded *AntigravityGuardError
		if !errors.As(err, &guarded) || guarded.Class != "" || guarded.StatusCode() != status || guarded.SafeResponseHeaders().Get("X-CPA-Health-Effect") != "" {
			t.Fatalf("authentication failure was neutralized: %#v", err)
		}
	}
}

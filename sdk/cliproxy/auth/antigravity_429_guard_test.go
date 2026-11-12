package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAntigravityNeutralResultPreservesQuotaAndRawFailures(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
	deadline := time.Now().Add(time.Hour)
	_, err := m.Register(WithSkipPersist(ctx), &Auth{ID: "guard", Provider: "antigravity", Status: StatusActive, ModelStates: map[string]*ModelState{"gemini-test": {Unavailable: true, NextRetryAfter: deadline, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: deadline}}}})
	if err != nil {
		t.Fatal(err)
	}
	m.MarkResult(ctx, Result{AuthID: "guard", Provider: "antigravity", Model: "gemini-test", ExecutionSeq: 10, Error: &Error{HTTPStatus: 429, Code: cliproxyexecutor.GuardClassAmbiguous429}})
	a, _ := m.GetByID("guard")
	if a.Failed != 1 || a.HealthFailed != 0 || a.NeutralResults != 1 {
		t.Fatalf("raw/health counts: %d/%d/%d", a.Failed, a.HealthFailed, a.NeutralResults)
	}
	state := a.ModelStates["gemini-test"]
	if !state.Quota.Exceeded || !state.NextRetryAfter.Equal(deadline) {
		t.Fatal("genuine quota overwritten")
	}
	if len(a.AdmissionBackoffs) != 1 {
		t.Fatal("missing separate admission pause")
	}
	clone := a.Clone()
	delete(clone.AdmissionBackoffs, "gemini-test")
	if len(a.AdmissionBackoffs) != 1 {
		t.Fatal("mutable admission state shared by snapshots")
	}
}

type guardBudgetExecutor struct {
	ProviderExecutor
	sends int
}

func (*guardBudgetExecutor) Identifier() string { return "antigravity" }
func (e *guardBudgetExecutor) Execute(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := cliproxyexecutor.ConsumeAntigravityAttempt(ctx, req.Model); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	e.sends++
	return cliproxyexecutor.Response{}, cliproxyexecutor.WrapAntigravityGuardFailure(ctx, &Error{HTTPStatus: 429, Message: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`}, cliproxyexecutor.GuardClassAmbiguous429)
}
func (e *guardBudgetExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, err := e.Execute(ctx, auth, req, opts)
	return nil, err
}

func TestAntigravityGuardManagerDoesNotStartThirdCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&internalconfig.Config{Antigravity429Guard: true})
			m.SetRetryConfig(20, 0, 0)
			exec := &guardBudgetExecutor{}
			m.RegisterExecutor(exec)
			reg := registry.GetGlobalRegistry()
			for i := 0; i < 4; i++ {
				id := fmt.Sprintf("budget-%v-%d", stream, i)
				reg.RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: "gemini-test"}})
				t.Cleanup(func() { reg.UnregisterClient(id) })
				if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}); err != nil {
					t.Fatal(err)
				}
			}
			opts := cliproxyexecutor.Options{Headers: http.Header{cliproxyexecutor.GuardContractHeader: {"v1"}}, Stream: stream}
			req := cliproxyexecutor.Request{Model: "gemini-test"}
			var err error
			if stream {
				_, err = m.ExecuteStream(context.Background(), []string{"antigravity"}, req, opts)
			} else {
				_, err = m.Execute(context.Background(), []string{"antigravity"}, req, opts)
			}
			var guarded *cliproxyexecutor.AntigravityGuardError
			if !errors.As(err, &guarded) || !guarded.IsRequestStop() || guarded.StatusCode() != 429 {
				t.Fatalf("terminal original error lost: %v", err)
			}
			if exec.sends != 2 {
				t.Fatalf("sends=%d want 2 despite larger retry configuration", exec.sends)
			}
		})
	}
}

func TestAntigravityPolicyAndOldSuccessDoNotResetAdmission(t *testing.T) {
	now := time.Unix(1000, 0)
	a := &Auth{}
	a.observeGuardHealth(Result{HealthEffect: "neutral", FailureClass: cliproxyexecutor.GuardClassAmbiguous429, ExecutionSeq: 10}, "gemini-test", now)
	a.observeGuardHealth(Result{Success: true, CompletedResponse: true, ExecutionSeq: 9}, "gemini-test", now)
	if len(a.AdmissionBackoffs) != 1 {
		t.Fatal("old in-flight success reset backoff")
	}
	a.observeGuardHealth(Result{Success: true, CompletedResponse: true, ExecutionSeq: 11, HealthEffect: "neutral", FailureClass: cliproxyexecutor.GuardClassPolicy}, "gemini-test", now)
	if len(a.AdmissionBackoffs) != 1 {
		t.Fatal("HTTP200 policy block reset backoff")
	}
	a.observeGuardHealth(Result{Success: true, CompletedResponse: true, ExecutionSeq: 12}, "gemini-test", now)
	if len(a.AdmissionBackoffs) != 0 {
		t.Fatal("new completed success did not reset backoff")
	}
}

func TestAntigravityGuardRequiresServerAndTrustedGatewayOptIn(t *testing.T) {
	m := NewManager(nil, nil, nil)
	opts := cliproxyexecutor.Options{Headers: http.Header{cliproxyexecutor.GuardContractHeader: {"v1"}}}
	request := cliproxyexecutor.Request{Model: "gemini-test"}
	ctx := context.Background()
	if cliproxyexecutor.AntigravityGuardFromContext(m.withAntigravity429Guard(ctx, []string{"antigravity"}, request, opts)) != nil {
		t.Fatal("client header enabled disabled server")
	}
	m.SetConfig(&internalconfig.Config{Antigravity429Guard: true})
	if cliproxyexecutor.AntigravityGuardFromContext(m.withAntigravity429Guard(ctx, []string{"antigravity"}, request, opts)) == nil {
		t.Fatal("opt in not honored")
	}
	if cliproxyexecutor.AntigravityGuardFromContext(m.withAntigravity429Guard(ctx, []string{"antigravity"}, request, cliproxyexecutor.Options{})) != nil {
		t.Fatal("missing trusted opt in enabled guard")
	}
}

func TestAntigravityWrappedClassificationSurvivesResultConversion(t *testing.T) {
	for _, class := range []string{cliproxyexecutor.GuardClassAmbiguous429, cliproxyexecutor.GuardClassPolicy, cliproxyexecutor.GuardClassBudget, "upstream_rate_limited"} {
		t.Run(class, func(t *testing.T) {
			ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
			cause := &Error{HTTPStatus: http.StatusTooManyRequests, Code: "request_scoped", Message: "provider refusal"}
			wrapped := cliproxyexecutor.WrapAntigravityGuardFailure(ctx, cause, class)
			result := resultErrorFromError(fmt.Errorf("execution: %w", wrapped))
			if result.Code != class || result.HTTPStatus != http.StatusTooManyRequests || !shouldSkipCredentialCooldown(result) {
				t.Fatalf("trusted classification lost: %+v", result)
			}
			annotated := annotateAntigravityGuardResult(ctx, Result{Provider: "antigravity", Model: "gemini-test", Error: result})
			if annotated.HealthEffect != cliproxyexecutor.GuardHealthNeutral || annotated.FailureClass != class {
				t.Fatalf("neutral health annotation lost: %+v", annotated)
			}
		})
	}
}

func TestAntigravityAdmissionPausePreservesCredentialProtections(t *testing.T) {
	now := time.Now()
	pause := now.Add(time.Minute)
	a := &Auth{AdmissionBackoffs: map[string]AdmissionBackoff{"gemini-test": {Until: pause}}}
	if blocked, reason, until := isAuthBlockedForModel(a, "gemini-test", now); !blocked || reason != blockReasonOther || !until.Equal(pause) {
		t.Fatal("admission pause not applied")
	}
	a.Disabled = true
	if blocked, reason, until := isAuthBlockedForModel(a, "gemini-test", now); !blocked || reason != blockReasonDisabled || !until.IsZero() {
		t.Fatal("admission pause weakened disabled protection")
	}
	a.Disabled = false
	quota := now.Add(time.Hour)
	a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: quota}
	if blocked, reason, until := isAuthBlockedForModel(a, "gemini-test", now); !blocked || reason != blockReasonCooldown || !until.Equal(quota) {
		t.Fatal("admission pause weakened genuine quota protection")
	}
}

func TestAntigravityNeutralClassificationCannotSuppressAuthenticationFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
			cliproxyexecutor.AntigravityGuardFromContext(ctx).ObservePolicyRejection()
			wrapped := cliproxyexecutor.WrapAntigravityGuardFailure(ctx, &Error{HTTPStatus: status, Message: "authentication failed"}, cliproxyexecutor.GuardClassAmbiguous429)
			converted := resultErrorFromError(wrapped)
			if converted.HTTPStatus != status || isGuardNeutralClass(converted.Code) || shouldSkipCredentialCooldown(converted) {
				t.Fatalf("authentication protection lost: %+v", converted)
			}
			result := annotateAntigravityGuardResult(ctx, Result{Provider: "antigravity", Model: "gemini-test", Error: converted, HealthEffect: cliproxyexecutor.GuardHealthNeutral})
			if result.HealthEffect == cliproxyexecutor.GuardHealthNeutral || result.FailureClass == cliproxyexecutor.GuardClassPolicy {
				t.Fatalf("earlier policy rejection suppressed current auth failure: %+v", result)
			}
			applyRequestScopedActionToResult(RequestScopedActionContinue, true, &result)
			if shouldSkipCredentialCooldown(result.Error) {
				t.Fatal("configured request rule suppressed auth protection")
			}
		})
	}
}

func TestAntigravityEarlierPolicyDoesNotOverrideQuotaClassification(t *testing.T) {
	ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
	cliproxyexecutor.AntigravityGuardFromContext(ctx).ObservePolicyRejection()
	result := annotateAntigravityGuardResult(ctx, Result{Provider: "antigravity", Model: "gemini-test", Error: &Error{HTTPStatus: 429, Code: "upstream_quota_exhausted"}})
	if result.FailureClass != "upstream_quota_exhausted" || result.HealthEffect == cliproxyexecutor.GuardHealthNeutral {
		t.Fatalf("earlier policy rejection overwrote genuine quota: %+v", result)
	}
}

func TestAntigravityCredentialQuotaOutranksContradictoryNeutralWrapper(t *testing.T) {
	ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
	wrapped := cliproxyexecutor.WrapAntigravityGuardFailure(ctx, issue6503QuotaError{}, cliproxyexecutor.GuardClassAmbiguous429)
	converted := resultErrorFromError(wrapped)
	if converted.Code != credentialQuotaErrorCode || shouldSkipCredentialCooldown(converted) {
		t.Fatalf("neutral wrapper suppressed credential quota: %+v", converted)
	}
	result := annotateAntigravityGuardResult(ctx, Result{Provider: "antigravity", Model: "gemini-test", CredentialScope: true, Error: converted})
	if result.HealthEffect == cliproxyexecutor.GuardHealthNeutral {
		t.Fatal("credential quota became neutral")
	}
}

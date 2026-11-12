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

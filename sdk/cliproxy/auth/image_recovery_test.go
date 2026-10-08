package auth

import (
	"context"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageAliasesShareQuotaWithoutBlockingOtherModels(t *testing.T) {
	m := newNoImageRotationManager(t, nil, "aliases")
	native, alias := "gemini-3.1-flash-image", "gemini-3.1-flash-image-preview"
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"antigravity": {{Name: native, Alias: alias, Fork: true}}})
	registry.GetGlobalRegistry().RegisterClient("aliases", "antigravity", []*registry.ModelInfo{{ID: native}, {ID: alias}, {ID: "gemini-pro-agent"}})
	long := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: "aliases", Provider: "antigravity", Model: native, RouteModel: alias, Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &long})
	a, _ := m.GetByID("aliases")
	for _, model := range []string{native, alias} {
		if key := m.selectionModelKeyForAuth(a, model); key != native {
			t.Fatalf("%s selects state %s instead of %s", model, key, native)
		}
		if _, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: model}, core.Options{}); err == nil {
			t.Fatalf("%s bypassed quota", model)
		}
	}
	if _, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: "gemini-pro-agent"}, core.Options{}); err != nil {
		t.Fatalf("other model blocked: %v", err)
	}
}

type imageRecoveryBlockingExecutor struct {
	noImageRotationExecutor
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (e *imageRecoveryBlockingExecutor) Execute(ctx context.Context, _ *Auth, _ core.Request, _ core.Options) (core.Response, error) {
	if err := core.ConsumeImageGeneration(ctx); err != nil {
		return core.Response{}, err
	}
	e.calls.Add(1)
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		return core.Response{Payload: []byte(`{"image":"ok"}`)}, nil
	case <-ctx.Done():
		return core.Response{}, ctx.Err()
	}
}

func TestImageExpiredCooldownAdmitsOnlyOneConcurrentRecovery(t *testing.T) {
	m := newNoImageRotationManager(t, nil, "single-probe")
	e := &imageRecoveryBlockingExecutor{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.RegisterExecutor(e)
	short := time.Millisecond
	model := "gemini-3.1-flash-image-preview"
	m.MarkResult(context.Background(), Result{AuthID: "single-probe", Provider: "antigravity", Model: model, Error: &Error{HTTPStatus: 502, Message: "empty"}, RetryAfter: &short})
	time.Sleep(5 * time.Millisecond)
	first := make(chan error, 1)
	go func() {
		_, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: model}, core.Options{})
		first <- err
	}()
	select {
	case <-e.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first recovery did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: model}, core.Options{})
			if err == nil {
				t.Error("sibling recovery was admitted")
			}
		}()
	}
	wg.Wait()
	if e.calls.Load() != 1 {
		t.Fatalf("actual recovery calls=%d", e.calls.Load())
	}
	close(e.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: model}, core.Options{}); err != nil {
		t.Fatalf("successful probe did not restore scheduling: %v", err)
	}
}

func TestImageRecoveryLeaseAndLateSuccess(t *testing.T) {
	m := newNoImageRotationManager(t, nil, "recover")
	model := "gemini-3.1-flash-image-preview"
	long := 3 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: "recover", Provider: "antigravity", Model: model, Error: &Error{HTTPStatus: 429, Message: "quota"}, RetryAfter: &long})
	before, _ := m.GetByID("recover")
	deadline := before.ModelStates[canonicalModelKey(model)].NextRetryAfter
	short := time.Minute
	m.MarkResult(context.Background(), Result{AuthID: "recover", Provider: "antigravity", Model: model, Error: &Error{HTTPStatus: 502, Message: "empty"}, RetryAfter: &short})
	m.MarkResult(context.Background(), Result{AuthID: "recover", Provider: "antigravity", Model: model, Success: true})
	a, _ := m.GetByID("recover")
	if a.ModelStates[canonicalModelKey(model)].NextRetryAfter.Before(deadline) {
		t.Fatal("long deadline was shortened")
	}
	if _, ok := m.claimImageRecovery(a, model); ok {
		t.Fatal("active cooldown admitted")
	}
	m.mu.Lock()
	m.auths[a.ID].ModelStates[canonicalModelKey(model)].NextRetryAfter = time.Now().Add(-time.Second)
	m.mu.Unlock()
	release, ok := m.claimImageRecovery(a, model)
	if !ok {
		t.Fatal("expired cooldown did not admit probe")
	}
	if _, ok := m.claimImageRecovery(a, model); ok {
		t.Fatal("concurrent recovery admitted")
	}
	if releaseOther, ok := m.claimImageRecovery(a, "gemini-pro-agent"); !ok {
		t.Fatal("other model blocked")
	} else {
		releaseOther()
	}
	release()
	if releaseAgain, ok := m.claimImageRecovery(a, model); !ok {
		t.Fatal("lease leaked")
	} else {
		releaseAgain()
	}
}

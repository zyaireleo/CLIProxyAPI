package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	initialFailureGeminiModel = "initial-failure-gemini-model"
)

type initialFailureGeminiStreamExecutor struct{}

func (*initialFailureGeminiStreamExecutor) Identifier() string {
	return "initial-failure-gemini-stream-executor"
}

func (*initialFailureGeminiStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*initialFailureGeminiStreamExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Err: errors.New("upstream failed before first payload")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*initialFailureGeminiStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*initialFailureGeminiStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*initialFailureGeminiStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestGeminiStreamGenerateContentDoesNotLoseErrorBeforeFirstPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			executor := &initialFailureGeminiStreamExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			authID := fmt.Sprintf("initial-failure-gemini-auth-%d", idx)
			auth := &coreauth.Auth{ID: authID, Provider: executor.Identifier(), Status: coreauth.StatusActive}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Errorf("register auth %d: %v", idx, errRegister)
				return
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: initialFailureGeminiModel}})
			defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)

			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			h := NewGeminiAPIHandler(base)
			router := gin.New()
			router.POST("/v1beta/models/*action", h.GeminiHandler)

			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/initial-failure-gemini-model:streamGenerateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code == http.StatusOK {
				t.Errorf("request %d lost the buffered initial error and returned HTTP 200: %q", idx, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "upstream failed before first payload") {
				t.Errorf("request %d lost the initial upstream error: status=%d body=%q", idx, recorder.Code, recorder.Body.String())
			}
		}(i)
	}
	wg.Wait()
}

type admissionGeminiExecutor struct {
	initialFailureGeminiStreamExecutor
	provider string
	calls    int
}

func (e *admissionGeminiExecutor) Identifier() string { return e.provider }
func (e *admissionGeminiExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	return coreexecutor.Response{Payload: []byte(`{"candidates":[]}`)}, nil
}
func (e *admissionGeminiExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls++
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type admissionGeminiHook struct {
	coreauth.NoopHook
	results int
}

func (h *admissionGeminiHook) OnResult(context.Context, coreauth.Result) { h.results++ }

func TestGeminiNativeQuotaAdmissionReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, provider := range []string{"antigravity", "codex"} {
		for _, action := range []string{"generateContent", "streamGenerateContent"} {
			t.Run(provider+"/"+action, func(t *testing.T) {
				model := "gemini-3-flash"
				if provider == "codex" {
					model = "gpt-5.4"
				}
				hook := &admissionGeminiHook{}
				manager := coreauth.NewManager(nil, nil, hook)
				executor := &admissionGeminiExecutor{provider: provider}
				manager.RegisterExecutor(executor)
				manager.SetRetryConfig(5, time.Second, 10)
				cfg := nativequota.DefaultConfig()
				cfg.Mode = nativequota.ModeEnforce
				cfg.MaxInFlight = 1
				cfg.MaxWait = 20 * time.Millisecond
				// Configure without starting a collector: this fixture never uses network.
				if err := manager.NativeQuota().Configure(cfg); err != nil {
					t.Fatal(err)
				}
				before := make(map[string]*coreauth.Auth)
				for _, suffix := range []string{"a", "b"} {
					id := "native-admission-" + provider + "-" + suffix
					auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}}
					if _, err := manager.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					manager.RefreshSchedulerEntry(id)
					release, err := manager.NativeQuota().Acquire(context.Background(), id, provider, model)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(release)
					before[id], _ = manager.GetByID(id)
				}
				base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
				router := gin.New()
				router.POST("/v1beta/models/*action", NewGeminiAPIHandler(base).GeminiHandler)
				request := func() *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":"+action, strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
					req.Header.Set("Content-Type", "application/json")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, req)
					return recorder
				}
				start := time.Now()
				recorder := request()
				elapsed := time.Since(start)
				if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
					t.Errorf("local denial status=%d Retry-After=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
				}
				if !strings.Contains(recorder.Body.String(), "wait_budget") || elapsed < cfg.MaxWait || elapsed > time.Second {
					t.Errorf("expected local wait-budget denial: elapsed=%v body=%s", elapsed, recorder.Body.String())
				}
				if executor.calls != 0 || hook.results != 0 {
					t.Errorf("denied request reached executor/MarkResult: calls=%d results=%d", executor.calls, hook.results)
				}
				for id, original := range before {
					current, _ := manager.GetByID(id)
					if !reflect.DeepEqual(original, current) {
						t.Errorf("local denial mutated credential %s", id)
					}
				}
				// Turning enforcement off restores execution even with the leases held.
				cfg.Mode = nativequota.ModeOff
				if err := manager.NativeQuota().Configure(cfg); err != nil {
					t.Fatal(err)
				}
				recorder = request()
				if recorder.Code != http.StatusOK || executor.calls != 1 || recorder.Header().Get("Retry-After") != "" {
					t.Errorf("off mode changed: status=%d calls=%d Retry-After=%q", recorder.Code, executor.calls, recorder.Header().Get("Retry-After"))
				}
			})
		}
	}
}

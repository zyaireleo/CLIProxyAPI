package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func catalogTestService(t *testing.T, url string) (*Service, *coreauth.Auth) {
	t.Helper()
	cfg := &config.Config{AuthDir: t.TempDir(), AntigravityModels: config.AntigravityModelsConfig{DiscoveryEnabled: true}}
	m := coreauth.NewManager(nil, nil, nil)
	a, err := m.Register(context.Background(), &coreauth.Auth{ID: t.Name(), Provider: "antigravity", Attributes: map[string]string{"base_url": url}, Metadata: map[string]any{"access_token": "test-token", "refresh_token": "test-refresh", "project_id": "project-test", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}})
	require.NoError(t, err)
	s := &Service{cfg: cfg, coreManager: m}
	t.Cleanup(func() { s.WaitAntigravityProbes(); registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return s, a
}

func forceCatalogRefresh(s *Service, a *coreauth.Auth) {
	s.agCatalogMu.Lock()
	s.agCatalogs[a.ID].next = time.Time{}
	s.agCatalogMu.Unlock()
	s.queueAntigravityCatalog(context.Background(), a)
	s.WaitAntigravityProbes()
}

func TestAntigravityCatalogDiscoveryAndRealRouting(t *testing.T) {
	var current atomic.Value
	current.Store(`{"models":{}}`)
	var calls atomic.Int32
	const model = "gemini-NewCase-2099"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == antigravityModelsPath {
			body, _ := io.ReadAll(r.Body)
			if gjson.GetBytes(body, "project").String() != "project-test" {
				t.Error("missing account project")
			}
			fmt.Fprint(w, current.Load().(string))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "model").String(); got != model {
			t.Errorf("upstream model=%s", got)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("wrong account selected")
		}
		calls.Add(1)
		response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", response)
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, response)
		}
	}))
	defer upstream.Close()
	s, a := catalogTestService(t, upstream.URL)
	s.registerModelsForAuth(context.Background(), a)
	s.WaitAntigravityProbes()
	require.Empty(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID))
	current.Store(`{"models":{"` + model + `":{"displayName":"New model","inputTokenLimit":12345}}}`)
	forceCatalogRefresh(s, a)
	models := registry.GetGlobalRegistry().GetModelsForClient(a.ID)
	require.Len(t, models, 1)
	require.Equal(t, model, models[0].ID)
	require.True(t, models[0].UserDefined)
	require.Equal(t, 12345, models[0].InputTokenLimit)
	// Exercise the public list handler, the scheduler and the real provider executor.
	router := gin.New()
	handler := &openai.OpenAIAPIHandler{}
	router.GET("/v1/models", handler.OpenAIModels)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), model)
	s.coreManager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(s.cfg))
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	request := exec.Request{Model: model, Payload: payload}
	opts := exec.Options{SourceFormat: translator.FormatGemini, ResponseFormat: translator.FormatGemini, OriginalRequest: payload}
	_, err := s.coreManager.Execute(context.Background(), []string{"antigravity"}, request, opts)
	require.NoError(t, err)
	opts.Stream = true
	result, err := s.coreManager.ExecuteStream(context.Background(), []string{"antigravity"}, request, opts)
	require.NoError(t, err)
	for chunk := range result.Chunks {
		require.NoError(t, chunk.Err)
	}
	require.EqualValues(t, 2, calls.Load())
	_, err = s.coreManager.Execute(context.Background(), []string{"antigravity"}, exec.Request{Model: "not-registered", Payload: payload}, opts)
	require.Error(t, err)
}

func TestAntigravityCatalogFailureInMemoryAndManualModels(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var body atomic.Value
	body.Store(`{"models":{"gemini-catalog-new":{}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, body.Load().(string))
	}))
	defer upstream.Close()
	s, a := catalogTestService(t, upstream.URL)
	s.cfg.AntigravityModels.AdditionalModels = []config.AntigravityAdditionalModel{{ID: "manual-model", AuthIDs: []string{a.ID}}, {ID: "other-account-only", AuthIDs: []string{"other"}}}
	s.registerModelsForAuth(context.Background(), a)
	s.WaitAntigravityProbes()
	require.Len(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID), 2)
	for _, code := range []int{401, 403, 429, 500, 503} {
		status.Store(int32(code))
		forceCatalogRefresh(s, a)
		require.Len(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID), 2)
	}
	status.Store(200)
	for _, bad := range []string{`{}`, `{"models":null}`, `broken`, `{"models":[]}`} {
		body.Store(bad)
		forceCatalogRefresh(s, a)
		require.Len(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID), 2)
	}
	// Refresh failures retain the in-memory catalog without writing auth files.
	entries, err := os.ReadDir(s.cfg.AuthDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	// A new service starts from static/manual models until discovery succeeds.
	restarted := &Service{cfg: s.cfg, coreManager: s.coreManager}
	restarted.cfgMu.RLock()
	restarted.agCatalogMu.Lock()
	saved := restarted.catalogStateLocked(a, s.cfg)
	restored := catalogModels(a, s.cfg, saved)
	restarted.agCatalogMu.Unlock()
	restarted.cfgMu.RUnlock()
	require.True(t, saved.Success.IsZero())
	var restoredIDs []string
	for _, model := range restored {
		restoredIDs = append(restoredIDs, model.ID)
	}
	require.Contains(t, restoredIDs, "manual-model")
	require.NotContains(t, restoredIDs, "gemini-catalog-new")
	body.Store(`{"models":{}}`)
	forceCatalogRefresh(s, a)
	models := registry.GetGlobalRegistry().GetModelsForClient(a.ID)
	require.Len(t, models, 1)
	require.Equal(t, "manual-model", models[0].ID)
	s.cfgMu.Lock()
	s.cfg = s.cfg.CloneForRuntime()
	s.cfg.AntigravityModels.AdditionalModels = nil
	s.cfgMu.Unlock()
	s.registerModelsForAuth(context.Background(), a)
	require.Empty(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID))
	s.cfgMu.Lock()
	s.cfg = s.cfg.CloneForRuntime()
	s.cfg.AntigravityModels.DiscoveryEnabled = false
	s.cfgMu.Unlock()
	s.registerModelsForAuth(context.Background(), a)
	s.WaitAntigravityProbes()
	require.NotEmpty(t, registry.GetGlobalRegistry().GetModelsForClient(a.ID))
}

func TestAntigravityCatalogAccountIsolation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := "account-one-model"
		if r.Header.Get("Authorization") == "Bearer two" {
			model = "account-two-model"
		}
		fmt.Fprintf(w, `{"models":{"%s":{}}}`, model)
	}))
	defer upstream.Close()
	s, a := catalogTestService(t, upstream.URL)
	b := a.Clone()
	b.ID += "-two"
	b.Metadata["access_token"] = "two"
	b, err := s.coreManager.Register(context.Background(), b)
	require.NoError(t, err)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(b.ID) })
	s.registerModelsForAuth(context.Background(), a)
	s.registerModelsForAuth(context.Background(), b)
	s.WaitAntigravityProbes()
	require.True(t, registry.GetGlobalRegistry().ClientSupportsModel(a.ID, "account-one-model"))
	require.False(t, registry.GetGlobalRegistry().ClientSupportsModel(a.ID, "account-two-model"))
	require.True(t, registry.GetGlobalRegistry().ClientSupportsModel(b.ID, "account-two-model"))
	require.False(t, registry.GetGlobalRegistry().ClientSupportsModel(b.ID, "account-one-model"))
}

func TestAntigravityCatalogStaleProbeAndSingleflight(t *testing.T) {
	for _, change := range []string{"delete", "disable", "config", "route"} {
		t.Run(change, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-release
				fmt.Fprint(w, `{"models":{"stale-new-model":{}}}`)
			}))
			defer upstream.Close()
			s, a := catalogTestService(t, upstream.URL)
			s.registerModelsForAuth(context.Background(), a)
			<-started
			for i := 0; i < 10; i++ {
				s.queueAntigravityCatalog(context.Background(), a)
			}
			switch change {
			case "delete":
				s.coreManager.Remove(context.Background(), a.ID)
				s.forgetAntigravityCatalog(a.ID)
				registry.GetGlobalRegistry().UnregisterClient(a.ID)
			case "disable":
				a.Disabled = true
				_, err := s.coreManager.Update(context.Background(), a)
				require.NoError(t, err)
			case "config":
				s.cfgMu.Lock()
				s.cfg = s.cfg.CloneForRuntime()
				s.cfg.AntigravityModels.DiscoveryEnabled = false
				s.cfgMu.Unlock()
			case "route":
				a.ProxyURL = "http://127.0.0.1:1"
				_, err := s.coreManager.Update(context.Background(), a)
				require.NoError(t, err)
			}
			close(release)
			s.WaitAntigravityProbes()
			require.EqualValues(t, 1, calls.Load())
			require.False(t, registry.GetGlobalRegistry().ClientSupportsModel(a.ID, "stale-new-model"))
		})
	}
}

func TestAntigravityCatalogManualAliasesExclusionsAndHotReload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":{}}`) }))
	defer upstream.Close()
	s, a := catalogTestService(t, upstream.URL)
	s.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{"antigravity": {{Name: "manual-upstream", Alias: "friendly"}}}
	s.cfg.AntigravityModels.AdditionalModels = []config.AntigravityAdditionalModel{{ID: "manual-upstream"}, {ID: "excluded"}}
	s.cfg.OAuthExcludedModels = map[string][]string{"antigravity": {"excluded"}}
	s.registerModelsForAuth(context.Background(), a)
	s.WaitAntigravityProbes()
	models := registry.GetGlobalRegistry().GetModelsForClient(a.ID)
	require.Len(t, models, 1)
	require.Equal(t, "friendly", models[0].ID)
	s.cfgMu.Lock()
	s.cfg = s.cfg.CloneForRuntime()
	s.cfg.AntigravityModels.AdditionalModels = []config.AntigravityAdditionalModel{{ID: "replacement"}}
	s.cfgMu.Unlock()
	s.registerModelsForAuth(context.Background(), a)
	s.WaitAntigravityProbes()
	models = registry.GetGlobalRegistry().GetModelsForClient(a.ID)
	require.Len(t, models, 1)
	require.Equal(t, "replacement", models[0].ID)
}

func TestAntigravityCatalogCancellationConnectionFailureAndRouteState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	s, a := catalogTestService(t, upstream.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := fetchAntigravityCatalog(ctx, a, s.cfg)
	require.Error(t, err)
	upstream.Close()
	_, err = fetchAntigravityCatalog(context.Background(), a, s.cfg)
	require.Error(t, err)
	s.agCatalogMu.Lock()
	state := s.catalogStateLocked(a, s.cfg)
	state.Models = []*ModelInfo{{ID: "old-account-model"}}
	state.Success = time.Now()
	s.agCatalogMu.Unlock()
	a.Metadata["project_id"] = "changed-project"
	s.agCatalogMu.Lock()
	loaded := s.catalogStateLocked(a, s.cfg)
	s.agCatalogMu.Unlock()
	require.True(t, loaded.Success.IsZero())
}

func TestAntigravityCatalogIgnoresLegacyDiskSnapshots(t *testing.T) {
	s, a := catalogTestService(t, "http://127.0.0.1:1")
	directory := filepath.Join(s.cfg.AuthDir, ".antigravity-models")
	require.NoError(t, os.Mkdir(directory, 0o700))
	snapshot := antigravityCatalogSnapshot{
		Route: catalogRoute(a, s.cfg), Models: []*ModelInfo{{ID: "legacy-disk-model"}}, Success: time.Now(),
	}
	data, err := json.Marshal(snapshot)
	require.NoError(t, err)
	path := filepath.Join(directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(a.ID))))
	require.NoError(t, os.WriteFile(path, data, 0o600))
	s.agCatalogMu.Lock()
	state := s.catalogStateLocked(a, s.cfg)
	s.agCatalogMu.Unlock()
	require.True(t, state.Success.IsZero())
	require.Empty(t, state.Models)
	s.forgetAntigravityCatalog(a.ID)
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, unchanged)
}

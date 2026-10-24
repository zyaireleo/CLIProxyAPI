package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

const nativeQuotaTestKey = "isolated-test-management-key"

// No executor, token store, live credentials, server listener, or collector is
// started by this fixture. Engine snapshots and HTTP requests are hermetic.
func newNativeQuotaFixture(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	cfg := &config.Config{Port: 8317, NativeQuota: config.DefaultNativeQuotaConfig()}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("# Keep this comment\nport: 8317\n"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{cfg: cfg, authManager: manager, configFilePath: path, envSecret: nativeQuotaTestKey, failedAttempts: make(map[string]*attemptInfo)}
	router := gin.New()
	router.GET("/native-quota.html", h.GetNativeQuotaUI)
	group := router.Group("/v0/management/native-quota", h.Middleware())
	group.GET("/status", h.GetNativeQuotaStatus)
	group.POST("/refresh", h.PostNativeQuotaRefresh)
	group.PUT("/config", h.PutNativeQuotaConfig)
	group.POST("/migration/preview", h.PostNativeQuotaMigrationPreview)
	group.POST("/plugins/stop", h.PostNativeQuotaPluginStop)
	group.GET("/ui", h.GetNativeQuotaUI)
	t.Cleanup(func() { _ = manager.StopNativeQuota(context.Background()) })
	return h, router
}

func nativeQuotaRequest(router http.Handler, method, path, body, header string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	if header == "Authorization" {
		request.Header.Set(header, "Bearer "+nativeQuotaTestKey)
	} else if header != "" {
		request.Header.Set(header, nativeQuotaTestKey)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func nativeQuotaJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nativeQuotaSeedAccounts(t *testing.T, h *Handler) *nativequota.Engine {
	t.Helper()
	for _, account := range []*coreauth.Auth{
		{ID: "ag-a", Provider: "antigravity", Attributes: map[string]string{"priority": "96"}, Metadata: map[string]any{"access_token": "not-a-live-token-do-not-emit"}},
		{ID: "ag-b", Provider: "antigravity", Disabled: true, Attributes: map[string]string{"priority": "100"}},
		{ID: "codex-a", Provider: "codex"},
		{ID: "api-key", Provider: "codex", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey}},
	} {
		if _, errRegister := h.authManager.Register(context.Background(), account); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	engine := h.authManager.NativeQuota()
	cfg := nativequota.DefaultConfig()
	cfg.Mode = nativequota.ModeShadow
	if errConfigure := engine.Configure(cfg); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	engine.SetAccounts([]nativequota.Account{
		{ID: "ag-a", Provider: "antigravity", Priority: 96},
		{ID: "ag-b", Provider: "antigravity", Disabled: true, Priority: 100},
		{ID: "codex-a", Provider: "codex"},
	})
	h.cfg.NativeQuota.Mode = "shadow"
	return engine
}

func TestNativeQuotaRequiresManagementAuthOnEveryRoute(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "status", ""},
		{http.MethodGet, "ui", ""},
		{http.MethodPost, "refresh", `{"auth_id":"ag-a"}`},
		{http.MethodPut, "config", `{}`},
		{http.MethodPost, "migration/preview", `{}`},
		{http.MethodPost, "plugins/stop", `{"id":"antigravity-priority"}`},
	} {
		t.Run(route.path, func(t *testing.T) {
			_, router := newNativeQuotaFixture(t)
			path := "/v0/management/native-quota/" + route.path
			for _, query := range []string{"", "?key=" + nativeQuotaTestKey} {
				response := nativeQuotaRequest(router, route.method, path+query, route.body, "")
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("unauthenticated status = %d, body=%s", response.Code, response.Body.String())
				}
			}
			for _, header := range []string{"Authorization", "X-Management-Key"} {
				response := nativeQuotaRequest(router, route.method, path, route.body, header)
				if response.Code == http.StatusUnauthorized || response.Code == http.StatusForbidden {
					t.Fatalf("valid %s rejected: %s", header, response.Body.String())
				}
			}
		})
	}
}

func TestNativeQuotaStatusPaginationPrecisionAndSecrets(t *testing.T) {
	h, router := newNativeQuotaFixture(t)
	engine := nativeQuotaSeedAccounts(t, h)
	remaining := 0.0000123456789
	now := time.Now().Add(-15 * time.Second).UTC()
	reset := now.Add(time.Hour)
	if !engine.Observe(nativequota.Observation{
		AuthID: "ag-a", Provider: "antigravity", ObservedAt: now, Source: "antigravity",
		Pools: []nativequota.Pool{{ID: "gemini", Models: []string{"gemini-pro"}, Windows: []nativequota.Window{{ID: "quota", Remaining: &remaining, ResetAt: reset}}}},
	}) {
		t.Fatal("fixture observation rejected")
	}
	response := nativeQuotaRequest(router, http.MethodGet, "/v0/management/native-quota/status?provider=antigravity&page_size=1", "", "Authorization")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Accounts []nativeQuotaAccountView `json:"accounts"`
		Total    int                      `json:"total"`
		HasMore  bool                     `json:"has_more"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if body.Total != 2 || !body.HasMore || len(body.Accounts) != 1 || body.Accounts[0].AuthID != "ag-a" {
		t.Fatalf("unexpected pagination: %+v", body)
	}
	account := body.Accounts[0]
	if account.AgeSeconds == nil || *account.AgeSeconds < 14 || account.Reason == "" || account.Mode != nativequota.ModeShadow {
		t.Fatalf("missing assessment fields: %+v", account)
	}
	window := account.Pools[0].Windows[0]
	if window.Remaining == nil || *window.Remaining != remaining || !window.ResetAt.Equal(reset) {
		t.Fatalf("precision/reset changed: %+v", window)
	}
	for _, forbidden := range []string{"not-a-live-token", "access_token", "refresh_token", nativeQuotaTestKey, "api-key"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("status leaked %q", forbidden)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("status must not be cached")
	}
	response = nativeQuotaRequest(router, http.MethodGet, "/v0/management/native-quota/status?auth_id=codex-a", "", "Authorization")
	if !strings.Contains(response.Body.String(), `"age_seconds":null`) {
		t.Fatalf("unknown age fabricated: %s", response.Body.String())
	}
	response = nativeQuotaRequest(router, http.MethodGet, "/v0/management/native-quota/status?provider=antigravity&page=2&page_size=1", "", "Authorization")
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil || len(body.Accounts) != 1 || !body.Accounts[0].Disabled {
		t.Fatalf("manual disable lost: %s", response.Body.String())
	}
	for _, query := range []string{"page=0", "page=", "page_size=", "page=-1", "page=1000001", "page_size=201", "page_size=no", "page=1&page=2", "provider=other", "url=https://invalid.example", "auth_id=" + url.QueryEscape(strings.Repeat("x", 513))} {
		response = nativeQuotaRequest(router, http.MethodGet, "/v0/management/native-quota/status?"+query, "", "Authorization")
		if response.Code != http.StatusBadRequest {
			t.Errorf("query %q status=%d", query, response.Code)
		}
	}
}

func TestNativeQuotaRefreshExactIDAndStrictJSON(t *testing.T) {
	h, router := newNativeQuotaFixture(t)
	nativeQuotaSeedAccounts(t, h)
	for _, test := range []struct {
		body string
		want int
	}{
		{`{"auth_id":"ag-a"}`, http.StatusAccepted},
		{`{"auth_id":"ag-a"}`, http.StatusAccepted},
		{`{"auth_id":"missing"}`, http.StatusNotFound},
		{`{"auth_id":"https://127.0.0.1/private"}`, http.StatusNotFound},
		{`{"auth_id":"../ag-a"}`, http.StatusNotFound},
		{`{"auth_id":" ag-a"}`, http.StatusBadRequest},
		{`{"auth_id":"ag-b"}`, http.StatusConflict},
		{`{"auth_id":"api-key"}`, http.StatusBadRequest},
		{`{"auth_id":"ag-a","url":"https://invalid.example"}`, http.StatusBadRequest},
		{`{"auth_id":"ag-a","auth_id":"codex-a"}`, http.StatusBadRequest},
		{`{"auth_id":"ag-a"} {}`, http.StatusBadRequest},
		{`null`, http.StatusBadRequest},
		{strings.Repeat("x", nativeQuotaBodyLimit+1), http.StatusBadRequest},
	} {
		response := nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/refresh", test.body, "X-Management-Key")
		if response.Code != test.want {
			t.Errorf("body %.90s: status=%d want=%d: %s", test.body, response.Code, test.want, response.Body.String())
		}
	}
	cfg := nativequota.DefaultConfig()
	if errConfigure := h.authManager.NativeQuota().Configure(cfg); errConfigure != nil {
		t.Fatal(errConfigure)
	}
	response := nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/refresh", `{"auth_id":"ag-a"}`, "Authorization")
	if response.Code != http.StatusConflict {
		t.Fatalf("off refresh status=%d", response.Code)
	}
}

func TestNativeQuotaConfigCommitAndReload(t *testing.T) {
	h, router := newNativeQuotaFixture(t)
	next := config.DefaultNativeQuotaConfig()
	next.RefreshConcurrency = 3
	var reloaded *config.Config
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloaded = cfg })
	response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", nativeQuotaJSON(t, next), "Authorization")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !reflect.DeepEqual(h.cfg.NativeQuota, next) || reloaded == nil || !reflect.DeepEqual(reloaded.NativeQuota, next) || h.authManager.NativeQuota().Config().RefreshConcurrency != 3 {
		t.Fatal("config was not published through reload and engine")
	}
	data, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var saved config.Config
	if errDecode := yaml.Unmarshal(data, &saved); errDecode != nil || !reflect.DeepEqual(saved.NativeQuota, next) {
		t.Fatalf("config did not round-trip: %s, %v", data, errDecode)
	}
	if !bytes.Contains(data, []byte("# Keep this comment")) {
		t.Fatal("existing comment lost")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(h.configFilePath), ".native-quota-*.tmp"))
	if len(files) != 0 {
		t.Fatalf("temporary full-config files leaked: %v", files)
	}
}

func TestNativeQuotaConfigEmptyProvidersRoundTrip(t *testing.T) {
	for _, document := range []string{
		"port: 8317\n",
		"port: 8317\nnative-quota:\n  providers: null\n",
	} {
		h, router := newNativeQuotaFixture(t)
		if errWrite := os.WriteFile(h.configFilePath, []byte(document), 0600); errWrite != nil {
			t.Fatal(errWrite)
		}
		next := config.DefaultNativeQuotaConfig()
		next.Providers = []string{}
		response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", nativeQuotaJSON(t, next), "Authorization")
		if response.Code != http.StatusOK {
			t.Fatalf("empty coverage did not persist: %d %s", response.Code, response.Body.String())
		}
		data, _ := os.ReadFile(h.configFilePath)
		var saved config.Config
		if errDecode := yaml.Unmarshal(data, &saved); errDecode != nil || saved.NativeQuota.Providers == nil || len(saved.NativeQuota.Providers) != 0 || saved.Port != 8317 {
			t.Fatalf("explicit empty coverage or unrelated port changed: %s, %v", data, errDecode)
		}
	}
}

func TestNativeQuotaConfigFailuresPreserveState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.NativeQuotaConfig)
	}{
		{"invalid mode", func(c *config.NativeQuotaConfig) { c.Mode = "anything" }},
		{"empty mode", func(c *config.NativeQuotaConfig) { c.Mode = "" }},
		{"zero concurrency", func(c *config.NativeQuotaConfig) { c.RefreshConcurrency = 0 }},
		{"empty interval", func(c *config.NativeQuotaConfig) { c.CodexInterval = "" }},
		{"invalid duration", func(c *config.NativeQuotaConfig) { c.MaxWait = "5s" }},
		{"negative limit", func(c *config.NativeQuotaConfig) { c.QueueLimit = -1 }},
		{"invalid provider", func(c *config.NativeQuotaConfig) { c.Providers = []string{"https://invalid.example"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, router := newNativeQuotaFixture(t)
			before, _ := os.ReadFile(h.configFilePath)
			previous := h.cfg
			next := config.DefaultNativeQuotaConfig()
			test.mutate(&next)
			response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", nativeQuotaJSON(t, next), "Authorization")
			after, _ := os.ReadFile(h.configFilePath)
			if response.Code != http.StatusUnprocessableEntity || h.cfg != previous || !bytes.Equal(before, after) {
				t.Fatalf("validation changed state: %d %s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("save failure", func(t *testing.T) {
		h, router := newNativeQuotaFixture(t)
		previous := h.cfg
		h.configFilePath = filepath.Join(t.TempDir(), "missing", "config.yaml")
		next := config.DefaultNativeQuotaConfig()
		next.RefreshConcurrency = 4
		response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", nativeQuotaJSON(t, next), "Authorization")
		if response.Code != http.StatusInternalServerError || h.cfg != previous || h.authManager.NativeQuota().Config().RefreshConcurrency != 2 {
			t.Fatalf("save failure changed state: %d %s", response.Code, response.Body.String())
		}
	})
	t.Run("partial config", func(t *testing.T) {
		_, router := newNativeQuotaFixture(t)
		for _, body := range []string{`{"mode":"off"}`, `{"mode":"off","url":"https://invalid.example"}`} {
			response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", body, "Authorization")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("partial config accepted: %d %s", response.Code, response.Body.String())
			}
		}
	})
}

func TestNativeQuotaConfigRefusesLegacyAndHomeConflicts(t *testing.T) {
	for _, id := range []string{"antigravity-priority", "codex-quota-scheduler", "Home"} {
		t.Run(id, func(t *testing.T) {
			h, router := newNativeQuotaFixture(t)
			enabled := true
			if id == "Home" {
				h.cfg.Home.Enabled = true
			} else {
				h.cfg.Plugins.Enabled = true
				h.cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{id: {Enabled: &enabled}}
			}
			before, _ := os.ReadFile(h.configFilePath)
			next := config.DefaultNativeQuotaConfig()
			next.Mode = "enforce"
			response := nativeQuotaRequest(router, http.MethodPut, "/v0/management/native-quota/config", nativeQuotaJSON(t, next), "Authorization")
			after, _ := os.ReadFile(h.configFilePath)
			if response.Code != http.StatusConflict || h.cfg.NativeQuota.Mode != "off" || !bytes.Equal(before, after) || !enabled {
				t.Fatalf("conflict changed state: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNativeQuotaConfigTransactionRollback(t *testing.T) {
	h, _ := newNativeQuotaFixture(t)
	original, _ := os.ReadFile(h.configFilePath)
	next := h.cfg.CloneForRuntime()
	next.NativeQuota.RefreshConcurrency = 7
	called := false
	errSave := nativeQuotaSaveTransaction(h.configFilePath, next, func() error {
		called = true
		return errors.New("synthetic apply failure with secret that must not escape")
	})
	after, _ := os.ReadFile(h.configFilePath)
	if !called || !errors.Is(errSave, errNativeQuotaApply) || !bytes.Equal(original, after) {
		t.Fatalf("rollback failed: called=%v err=%v bytes=%q", called, errSave, after)
	}
	if errWrite := os.WriteFile(h.configFilePath, []byte("[invalid yaml"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	called = false
	errSave = nativeQuotaSaveTransaction(h.configFilePath, next, func() error { called = true; return nil })
	after, _ = os.ReadFile(h.configFilePath)
	if called || !errors.Is(errSave, errNativeQuotaSave) || string(after) != "[invalid yaml" {
		t.Fatalf("invalid existing config was touched: called=%v err=%v bytes=%q", called, errSave, after)
	}
}

func TestNativeQuotaMigrationPreviewIsReadOnly(t *testing.T) {
	h, router := newNativeQuotaFixture(t)
	nativeQuotaSeedAccounts(t, h)
	body := `{"schema_version":1,"plugin_id":"antigravity-priority","plugin_version":"1.2.20","user_config":{"refresh_interval":"10m"},"credential_mappings":[{"credential_id":"old-a","auth_id":"ag-a"},{"credential_id":"old-b","auth_id":"ag-b"}]}`
	before, _ := os.ReadFile(h.configFilePath)
	accountsBefore := h.authManager.List()
	sort.Slice(accountsBefore, func(i, j int) bool { return accountsBefore[i].ID < accountsBefore[j].ID })
	configBefore := h.cfg.CloneForRuntime()
	response := nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/migration/preview", body, "Authorization")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		DryRun   bool                          `json:"dry_run"`
		Allowed  bool                          `json:"execution_allowed"`
		Accounts []nativeQuotaMigrationAccount `json:"accounts"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !result.DryRun || result.Allowed || len(result.Accounts) != 2 || !result.Accounts[1].CurrentDisabled || *result.Accounts[0].CurrentPriority != 96 || *result.Accounts[1].CurrentPriority != 100 {
		t.Fatalf("preview changed provenance: %+v", result)
	}
	after, _ := os.ReadFile(h.configFilePath)
	accountsAfter := h.authManager.List()
	sort.Slice(accountsAfter, func(i, j int) bool { return accountsAfter[i].ID < accountsAfter[j].ID })
	if !bytes.Equal(before, after) || !reflect.DeepEqual(h.cfg, configBefore) || !reflect.DeepEqual(accountsAfter, accountsBefore) {
		t.Fatal("preview changed configuration or auth state")
	}
	for _, invalid := range []string{
		strings.Replace(body, "1.2.20", "9.0.0", 1),
		strings.Replace(body, "antigravity-priority", "unreviewed-plugin", 1),
		strings.Replace(body, `"ag-a"`, `"https://invalid.example"`, 1),
		strings.Replace(body, `"ag-a"`, `"codex-a"`, 1),
		strings.Replace(body, `"ag-b"`, `"ag-a"`, 1),
		strings.Replace(body, `"refresh_interval":"10m"`, `"refresh_interval":"10m","access_token":"reject"`, 1),
		strings.Replace(body, `"auth_id":"ag-a"`, `"auth_id":"ag-a","auth_id":"ag-b"`, 1),
	} {
		response = nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/migration/preview", invalid, "Authorization")
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("unsafe preview accepted: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestNativeQuotaPluginStopRequiresExplicitDisableAndConfirmedCompletion(t *testing.T) {
	for _, flag := range []*bool{nil, new(true), new(false)} {
		h, router := newNativeQuotaFixture(t)
		h.pluginHost = pluginhost.New()
		h.cfg.Plugins.Dir = t.TempDir()
		h.cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
			"antigravity-priority": {Enabled: flag},
		}
		h.pluginHost.ApplyConfig(context.Background(), h.cfg.CloneForRuntime())
		before, _ := os.ReadFile(h.configFilePath)
		response := nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/plugins/stop", `{"id":"antigravity-priority"}`, "Authorization")
		want := http.StatusConflict
		if flag != nil && !*flag {
			want = http.StatusOK
		}
		after, _ := os.ReadFile(h.configFilePath)
		if response.Code != want || !bytes.Equal(before, after) {
			t.Fatalf("disable gate: %d want=%d body=%s", response.Code, want, response.Body.String())
		}
	}
	h, router := newNativeQuotaFixture(t)
	h.pluginHost = pluginhost.New()
	h.cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{"antigravity-priority": {Enabled: new(false)}}
	h.pluginHost.ApplyConfig(context.Background(), h.cfg.CloneForRuntime())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v0/management/native-quota/plugins/stop", strings.NewReader(`{"id":"antigravity-priority"}`)).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+nativeQuotaTestKey)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"stopped":false`) {
		t.Fatalf("canceled stop claimed success: %d %s", response.Code, response.Body.String())
	}
}

func TestNativeQuotaPluginStopRejectsUnknownID(t *testing.T) {
	_, router := newNativeQuotaFixture(t)
	for _, id := range []string{"unknown", "https://invalid.example", "../antigravity-priority", " antigravity-priority"} {
		response := nativeQuotaRequest(router, http.MethodPost, "/v0/management/native-quota/plugins/stop", nativeQuotaJSON(t, map[string]string{"id": id}), "Authorization")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unknown ID accepted: %d %s", response.Code, response.Body.String())
		}
	}
}

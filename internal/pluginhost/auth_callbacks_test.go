package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type memoryAuthStorage struct {
	payload []byte
}

func (s *memoryAuthStorage) RawJSON() []byte {
	if s == nil {
		return nil
	}
	return append([]byte(nil), s.payload...)
}
func (s *memoryAuthStorage) SaveTokenToFile(authFilePath string) error {
	if s == nil || len(s.payload) == 0 {
		return fmt.Errorf("memory auth storage payload is empty")
	}
	return os.WriteFile(authFilePath, s.payload, 0o600)
}

func TestHostAuthListCallbackUsesAuthManager(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "demo-a.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"demo","email":"a@example.com","api_key":"k1"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	auth := &coreauth.Auth{
		ID:       "demo-a.json",
		Provider: "demo",
		FileName: "demo-a.json",
		Label:    "a@example.com",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":   path,
			"source": path,
		},
		Metadata: map[string]any{
			"type":    "demo",
			"email":   "a@example.com",
			"api_key": "k1",
		},
		Storage: &memoryAuthStorage{payload: []byte(`{"type":"demo","email":"a@example.com","api_key":"k1"}`)},
	}
	auth.EnsureIndex()

	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))
	if _, errRegister := host.currentAuthManager().Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthList, nil)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[rpcHostAuthListResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("files = %#v, want one entry", resp.Files)
	}
	entry := resp.Files[0]
	if entry.AuthIndex != auth.Index || entry.Name != "demo-a.json" || entry.Email != "a@example.com" {
		t.Fatalf("entry = %#v, want auth index and file metadata", entry)
	}
}

func TestHostAuthGetCallbackReturnsPhysicalJSONByAuthIndex(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "demo-b.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"demo","email":"b@example.com","api_key":"k2"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	auth := &coreauth.Auth{
		ID:       "demo-b.json",
		Provider: "demo",
		FileName: "demo-b.json",
		Label:    "b@example.com",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":   path,
			"source": path,
		},
		Metadata: map[string]any{
			"type":    "demo",
			"email":   "b@example.com",
			"api_key": "k2",
		},
		Storage: &memoryAuthStorage{payload: []byte(`{"type":"demo","email":"b@example.com","api_key":"changed"}`)},
	}
	auth.EnsureIndex()

	host := New()
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))
	if _, errRegister := host.currentAuthManager().Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	req, errMarshal := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthGet, req)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[rpcHostAuthGetResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.AuthIndex != auth.Index || resp.Name != "demo-b.json" {
		t.Fatalf("response = %#v, want auth index and name", resp)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(resp.JSON, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal auth json: %v", errUnmarshal)
	}
	if decoded["email"] != "b@example.com" || decoded["api_key"] != "k2" {
		t.Fatalf("decoded json = %#v, want credential payload", decoded)
	}
}

func TestHostAuthListCallbackFallsBackToDisk(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "claude-a.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"claude","email":"c@example.com"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}

	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthList, nil)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[rpcHostAuthListResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("files = %#v, want one disk entry", resp.Files)
	}
	entry := resp.Files[0]
	if entry.Name != "claude-a.json" || entry.Type != "claude" || entry.Email != "c@example.com" {
		t.Fatalf("entry = %#v, want disk metadata", entry)
	}
	if entry.ModTime.IsZero() {
		t.Fatalf("entry modtime is zero: %#v", entry)
	}
	_ = time.Now()
}

func TestHostAuthGetRuntimeCallbackReturnsRuntimeInfo(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "demo-runtime.json",
		Provider: "demo",
		FileName: "demo-runtime.json",
		Label:    "runtime@example.com",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type":    "demo",
			"email":   "runtime@example.com",
			"api_key": "runtime-key",
		},
		Storage: &memoryAuthStorage{payload: []byte(`{"type":"demo","email":"runtime@example.com","api_key":"runtime-key"}`)},
	}
	auth.EnsureIndex()

	host := New()
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))
	if _, errRegister := host.currentAuthManager().Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	req, errMarshal := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthGetRuntime, req)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostAuthGetRuntimeResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Auth.AuthIndex != auth.Index || resp.Auth.RuntimeOnly != true || resp.Auth.Email != "runtime@example.com" {
		t.Fatalf("response = %#v, want runtime auth entry", resp.Auth)
	}
}

func TestHostAuthGetRuntimeCallbackReturnsBaseURL(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "demo-base-url.json",
		Provider: "demo",
		FileName: "demo-base-url.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
			"base_url":     "https://api.custom.example.com/v1",
		},
	}
	auth.EnsureIndex()

	authMeta := &coreauth.Auth{
		ID:       "demo-meta-base-url.json",
		Provider: "demo",
		FileName: "demo-meta-base-url.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"base_url": "https://meta.custom.example.com/v1",
		},
	}
	authMeta.EnsureIndex()

	host := New()
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))
	if _, errRegister := host.currentAuthManager().Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	if _, errRegister := host.currentAuthManager().Register(context.Background(), authMeta); errRegister != nil {
		t.Fatalf("register authMeta: %v", errRegister)
	}

	req, errMarshal := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: auth.Index})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthGetRuntime, req)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostAuthGetRuntimeResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Auth.BaseURL != "https://api.custom.example.com/v1" {
		t.Fatalf("resp.Auth.BaseURL = %q, want %q", resp.Auth.BaseURL, "https://api.custom.example.com/v1")
	}

	reqMeta, errMarshalMeta := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authMeta.Index})
	if errMarshalMeta != nil {
		t.Fatalf("marshal request meta: %v", errMarshalMeta)
	}
	rawRespMeta, errCallMeta := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthGetRuntime, reqMeta)
	if errCallMeta != nil {
		t.Fatalf("callFromPlugin() meta error = %v", errCallMeta)
	}
	respMeta, errDecodeMeta := decodeRPCEnvelope[pluginapi.HostAuthGetRuntimeResponse](rawRespMeta)
	if errDecodeMeta != nil {
		t.Fatalf("decode response meta: %v", errDecodeMeta)
	}
	if respMeta.Auth.BaseURL != "https://meta.custom.example.com/v1" {
		t.Fatalf("respMeta.Auth.BaseURL = %q, want %q", respMeta.Auth.BaseURL, "https://meta.custom.example.com/v1")
	}

	authBoth := &coreauth.Auth{
		ID:       "demo-both-base-url.json",
		Provider: "demo",
		FileName: "demo-both-base-url.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
			"base_url":     "https://attr.custom.example.com/v1",
		},
		Metadata: map[string]any{
			"base_url": "https://meta.custom.example.com/v1",
		},
	}
	authBoth.EnsureIndex()
	if _, errRegister := host.currentAuthManager().Register(context.Background(), authBoth); errRegister != nil {
		t.Fatalf("register authBoth: %v", errRegister)
	}

	reqBoth, errMarshalBoth := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authBoth.Index})
	if errMarshalBoth != nil {
		t.Fatalf("marshal request both: %v", errMarshalBoth)
	}
	rawRespBoth, errCallBoth := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthGetRuntime, reqBoth)
	if errCallBoth != nil {
		t.Fatalf("callFromPlugin() both error = %v", errCallBoth)
	}
	respBoth, errDecodeBoth := decodeRPCEnvelope[pluginapi.HostAuthGetRuntimeResponse](rawRespBoth)
	if errDecodeBoth != nil {
		t.Fatalf("decode response both: %v", errDecodeBoth)
	}
	if respBoth.Auth.BaseURL != "https://attr.custom.example.com/v1" {
		t.Fatalf("respBoth.Auth.BaseURL = %q, want %q", respBoth.Auth.BaseURL, "https://attr.custom.example.com/v1")
	}
}

func TestListAuthFilesFromDiskReadsBaseURL(t *testing.T) {
	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "test-auth.json")
	fileData := []byte(`{"type":"openai","base_url":"https://disk-proxy.example.com/v1","email":"user@example.com"}`)
	if errWrite := os.WriteFile(filePath, fileData, 0o600); errWrite != nil {
		t.Fatalf("write file: %v", errWrite)
	}

	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}
	entries, errList := host.listAuthFilesFromDisk()
	if errList != nil {
		t.Fatalf("listAuthFilesFromDisk error: %v", errList)
	}
	if len(entries) != 1 {
		t.Fatalf("entries count = %d, want 1", len(entries))
	}
	if entries[0].BaseURL != "https://disk-proxy.example.com/v1" {
		t.Fatalf("entry BaseURL = %q, want https://disk-proxy.example.com/v1", entries[0].BaseURL)
	}
}

func TestHostAuthSaveCallbackRejectsInvalidWeightBeforePersistence(t *testing.T) {
	for _, rawWeight := range []string{`1.5`, `1000001`, `9223372036854775808`, `"invalid"`} {
		t.Run(rawWeight, func(t *testing.T) {
			authDir := t.TempDir()
			host := New()
			host.runtimeConfig = &config.Config{AuthDir: authDir}
			host.SetAuthManager(coreauth.NewManager(nil, nil, nil))

			req, errMarshal := json.Marshal(pluginapi.HostAuthSaveRequest{
				Name: "invalid.json",
				JSON: json.RawMessage(`{"type":"demo","weight":` + rawWeight + `}`),
			})
			if errMarshal != nil {
				t.Fatalf("marshal request: %v", errMarshal)
			}
			if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthSave, req); errCall == nil {
				t.Fatal("host.auth.save accepted an invalid weight")
			}
			if _, errStat := os.Stat(filepath.Join(authDir, "invalid.json")); !os.IsNotExist(errStat) {
				t.Fatalf("invalid auth file was persisted: %v", errStat)
			}
			if auths := host.currentAuthManager().List(); len(auths) != 0 {
				t.Fatalf("invalid auth was registered: %#v", auths)
			}
		})
	}
}

type quotaCallbackTestLoader struct {
	contexts []context.Context
	clients  []*lifecycleTestClient
	onOpen   func(*Host, context.Context) error
}

func (l *quotaCallbackTestLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	ctx := (dynamicHostCallbackEntry{host: host, pluginID: file.ID, callbacks: file.callbacks}).context()
	l.contexts = append(l.contexts, ctx)
	if l.onOpen != nil {
		if errOpen := l.onOpen(host, ctx); errOpen != nil {
			return nil, errOpen
		}
	}
	client := &lifecycleTestClient{call: func(context.Context, string, []byte) ([]byte, error) {
		return lifecycleRegistrationResult(validTestPlugin(file.ID))
	}}
	l.clients = append(l.clients, client)
	return client, nil
}

func TestHostLegacyAuthSaveGenerationAllowsStartupAndRejectsRetiredAndStopped(t *testing.T) {
	const id = "antigravity-priority"
	authDir := t.TempDir()
	request := []byte(`{"name":"legacy.json","json":{"type":"antigravity","priority":1}}`)
	loader := &quotaCallbackTestLoader{onOpen: func(h *Host, ctx context.Context) error {
		_, errSave := h.callFromPlugin(ctx, pluginabi.MethodHostAuthSave, request)
		return errSave
	}}
	h := NewForTest(loader)
	t.Cleanup(h.ShutdownAll)
	root, _ := makeVersionedPluginDir(t, id, "1.0.0")
	cfg := &config.Config{AuthDir: authDir, Plugins: config.PluginsConfig{
		Enabled: true, Dir: root, Configs: map[string]config.PluginInstanceConfig{id: enabledPluginConfigWithStoreVersion(t, "1.0.0")},
	}}
	h.ApplyConfig(context.Background(), cfg)
	if !h.PluginLoaded(id) || len(loader.contexts) != 1 {
		t.Fatal("generation fence rejected a legitimate Open callback before load publication")
	}
	writeVersionedPluginFile(t, root, id, "2.0.0")
	replacement := *cfg
	replacement.Plugins.Configs = map[string]config.PluginInstanceConfig{id: enabledPluginConfigWithStoreVersion(t, "2.0.0")}
	h.ApplyConfig(context.Background(), &replacement)
	if len(loader.contexts) != 2 {
		t.Fatal("replacement did not load")
	}
	staleRequest := []byte(`{"name":"legacy.json","json":{"type":"antigravity","priority":999}}`)
	if _, errSave := h.callFromPlugin(loader.contexts[0], pluginabi.MethodHostAuthSave, staleRequest); errSave == nil {
		t.Fatal("retired generation overwrote auth data")
	}
	if _, errSave := h.callFromPlugin(loader.contexts[1], pluginabi.MethodHostAuthSave, request); errSave != nil {
		t.Fatalf("current generation auth save = %v", errSave)
	}
	h.ApplyConfig(context.Background(), disabledQuotaTestConfig(&replacement, id))
	// Retired-only state must still block ownership even without a snapshot entry.
	h.mu.Lock()
	current := h.loaded[id]
	delete(h.loaded, id)
	h.mu.Unlock()
	assertQuotaOwnershipBlocked(t, h, nil)
	h.mu.Lock()
	h.loaded[id] = current
	h.mu.Unlock()
	if errStop := h.StopPlugin(context.Background(), id); errStop != nil {
		t.Fatal(errStop)
	}
	for index, ctx := range loader.contexts {
		if _, errSave := h.callFromPlugin(ctx, pluginabi.MethodHostAuthSave, staleRequest); errSave == nil {
			t.Fatalf("stopped generation %d overwrote auth data", index)
		}
		if loader.clients[index].shutdownCalls.Load() != 1 {
			t.Fatalf("generation %d was not shut down exactly once", index)
		}
	}
	data, errRead := os.ReadFile(filepath.Join(authDir, "legacy.json"))
	if errRead != nil || string(data) != `{"type":"antigravity","priority":1}` {
		t.Fatalf("stale generation mutated auth file: %s, %v", data, errRead)
	}
}

type quotaBlockingAuthHook struct {
	coreauth.NoopHook
	started chan struct{}
	release chan struct{}
}

func (h *quotaBlockingAuthHook) OnAuthRegistered(context.Context, *coreauth.Auth) {
	close(h.started)
	<-h.release
}

func TestHostLegacyStopWaitsForAlreadyAdmittedAuthSave(t *testing.T) {
	const id = "codex-quota-scheduler"
	loader := &quotaCallbackTestLoader{}
	h := NewForTest(loader)
	hook := &quotaBlockingAuthHook{started: make(chan struct{}), release: make(chan struct{})}
	var releaseHook sync.Once
	t.Cleanup(func() { releaseHook.Do(func() { close(hook.release) }); h.ShutdownAll() })
	h.SetAuthManager(coreauth.NewManager(nil, nil, hook))
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, id), Configs: enabledPluginConfigs(id)}}
	h.ApplyConfig(context.Background(), cfg)
	request := []byte(`{"name":"legacy.json","json":{"type":"codex","priority":1}}`)
	saved := make(chan error, 1)
	go func() {
		_, errSave := h.callFromPlugin(loader.contexts[0], pluginabi.MethodHostAuthSave, request)
		saved <- errSave
	}()
	waitForHostTestSignal(t, hook.started, "admitted auth save")
	h.ApplyConfig(context.Background(), disabledQuotaTestConfig(cfg, id))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if errStop := h.StopPlugin(ctx, id); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("stop acknowledged in-flight auth save: %v", errStop)
	}
	if loader.clients[0].shutdownCalls.Load() != 1 {
		t.Fatal("client shutdown was not started")
	}
	assertQuotaOwnershipBlocked(t, h, nil)
	if _, errSave := h.callFromPlugin(loader.contexts[0], pluginabi.MethodHostAuthSave, request); errSave == nil {
		t.Fatal("detached generation admitted another auth save")
	}
	releaseHook.Do(func() { close(hook.release) })
	if errSave := waitForQuotaTestError(t, saved); errSave != nil {
		t.Fatal(errSave)
	}
	if errStop := h.StopPlugin(context.Background(), id); errStop != nil {
		t.Fatal(errStop)
	}
	if len(h.NativeQuotaConflicts(nil)) != 0 {
		t.Fatal("drained auth save retained a conflict")
	}
}

func TestHostAuthSaveGenerationFenceIsNarrow(t *testing.T) {
	h := New()
	h.runtimeConfig = &config.Config{AuthDir: t.TempDir()}
	request := []byte(`{"name":"test.json","json":{"type":"demo"}}`)
	// Unidentified in-process callbacks and unrelated plugins retain compatibility.
	for _, ctx := range []context.Context{context.Background(), withHostCallbackPluginID(context.Background(), "unrelated")} {
		if _, errSave := h.callFromPlugin(ctx, pluginabi.MethodHostAuthSave, request); errSave != nil {
			t.Fatalf("unrelated callback was fenced: %v", errSave)
		}
	}
	ctx := withHostCallbackPluginID(context.Background(), "antigravity-priority")
	if _, errSave := h.callFromPlugin(ctx, pluginabi.MethodHostAuthSave, request); errSave == nil {
		t.Fatal("identified legacy callback without generation was accepted")
	}
	if _, errList := h.callFromPlugin(ctx, pluginabi.MethodHostAuthList, nil); errList != nil {
		t.Fatalf("read-only legacy callback was blocked: %v", errList)
	}
}

func TestHostAuthSaveCallbackWritesPhysicalFile(t *testing.T) {
	authDir := t.TempDir()
	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}
	host.SetAuthManager(coreauth.NewManager(nil, nil, nil))

	req, errMarshal := json.Marshal(pluginapi.HostAuthSaveRequest{
		Name: "saved.json",
		JSON: json.RawMessage(`{"type":"demo","email":"saved@example.com","api_key":"saved-key"}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostAuthSave, req)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostAuthSaveResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Name != "saved.json" {
		t.Fatalf("response = %#v, want saved file name", resp)
	}
	data, errRead := os.ReadFile(resp.Path)
	if errRead != nil {
		t.Fatalf("read saved file: %v", errRead)
	}
	if string(data) != `{"type":"demo","email":"saved@example.com","api_key":"saved-key"}` {
		t.Fatalf("saved file = %q, want credential json", string(data))
	}
	auths := host.currentAuthManager().List()
	if len(auths) != 1 || auths[0].FileName != "saved.json" {
		t.Fatalf("auths = %#v, want one registered auth", auths)
	}
}

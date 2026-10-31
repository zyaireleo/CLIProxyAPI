package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

const (
	devFPGoldenAccount1    = "e1e46a26-def3-4e3e-9ee2-185e59ed0444"
	devFPGoldenAccount2    = "3216ba40-955f-42a6-aabf-a9ac6ae94a30"
	devFPGoldenTeamUserAAA = "2d1c341a-5607-4894-ae5e-6cf59c2e7dde"
	devFPGoldenTeamUserBBB = "fd5de10e-f853-4dd0-92f4-ece041b115a9"
	devFPGoldenAuthID      = "e1adc2d3-5830-4ecd-b4ad-dcc4dd155b9a"

	devFPExplicitID = "11111111-2222-4333-8444-555555555555"
	devFPSIWCDevice = "aaaaaaaa-bbbb-4ccc-9ddd-eeeeeeeeeeee"
)

func devFPConfig(mode string) *config.Config {
	return &config.Config{Codex: config.CodexConfig{DeviceFingerprintMode: mode}}
}

func devFPConfigInject(mode string, inject *bool) *config.Config {
	cfg := devFPConfig(mode)
	cfg.Codex.DeviceFingerprintInjectMissing = inject
	return cfg
}

func devFPAuth(id string, accountID string) *cliproxyauth.Auth {
	metadata := map[string]any{"access_token": "oauth-token", "plan_type": "plus"}
	if accountID != "" {
		metadata["account_id"] = accountID
	}
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Metadata: metadata, Attributes: map[string]string{}}
}

func devFPIDToken(accountID string, userID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	authInfo := map[string]any{}
	if accountID != "" {
		authInfo["chatgpt_account_id"] = accountID
	}
	if userID != "" {
		authInfo["chatgpt_user_id"] = userID
	}
	payload, _ := json.Marshal(map[string]any{"email": "user@example.com", "https://api.openai.com/auth": authInfo})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func devFPAccountID(accountID string) string {
	return codexStableUUIDv4(codexDeviceFPSeedPrefix + "account\x00" + accountID)
}

func devFPResetState(t *testing.T) {
	t.Helper()
	codexDeviceStickyMu.Lock()
	codexDeviceSticky = map[string]codexDeviceStickyIdentity{}
	codexDeviceStickyMu.Unlock()
	codexDeviceIDTokenMu.Lock()
	codexDeviceIDTokenCache = map[string]codexDeviceIDTokenClaims{}
	codexDeviceIDTokenMu.Unlock()
	codexDeviceFPWarnOnce.Range(func(key, _ any) bool {
		codexDeviceFPWarnOnce.Delete(key)
		return true
	})
	codexDeviceFPInfoState.Range(func(key, _ any) bool {
		codexDeviceFPInfoState.Delete(key)
		return true
	})
}

var devFPLogMu sync.Mutex

func devFPCaptureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	devFPLogMu.Lock()
	logger := log.StandardLogger()
	previous := logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() {
		logger.ReplaceHooks(previous)
		devFPLogMu.Unlock()
	})
	return hook
}

func devFPHasWarn(hook *logtest.Hook, substr string) bool {
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, substr) {
			return true
		}
	}
	return false
}

func devFPResolveID(t *testing.T, cfg *config.Config, auth *cliproxyauth.Auth) codexDeviceIdentity {
	t.Helper()
	identity, ok := resolveCodexDeviceIdentity(cfg, auth, false)
	if !ok {
		t.Fatalf("device identity not resolved for auth %q", auth.ID)
	}
	return identity
}

// 1
func TestCodexDeviceFP_CollapsesInstallationsPerAuth(t *testing.T) {
	devFPResetState(t)
	auth := devFPAuth("codex-collapse.json", "acc-test-1")
	identity := devFPResolveID(t, devFPConfig("device"), auth)
	if identity.InstallationID != devFPGoldenAccount1 || identity.Source != codexDeviceFPSourceAccount {
		t.Fatalf("identity = %+v, want account golden %s", identity, devFPGoldenAccount1)
	}
	for _, installation := range []string{"install-user-1", "install-user-2"} {
		payload := []byte(`{"model":"gpt-5.3-codex","prompt_cache_key":"cache-1","client_metadata":{"x-codex-installation-id":"` + installation + `"}}`)
		body := applyCodexDeviceFingerprintBody(payload, identity.InstallationID, identity.AllowInject)
		if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != devFPGoldenAccount1 {
			t.Fatalf("installation for %q = %q, want %q", installation, got, devFPGoldenAccount1)
		}
		if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "cache-1" {
			t.Fatalf("prompt_cache_key rewritten: %q", got)
		}
	}
}

// 2
func TestCodexDeviceFP_InjectsMissingInstallationID(t *testing.T) {
	devFPResetState(t)
	auth := devFPAuth("codex-inject.json", "acc-test-1")
	payload := []byte(`{"model":"gpt-5.3-codex","input":[]}`)
	enabled, disabled := true, false
	for _, tc := range []struct {
		name   string
		inject *bool
		want   bool
	}{
		{"default", nil, false},
		{"explicit false", &disabled, false},
		{"explicit true", &enabled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := devFPResolveID(t, devFPConfigInject("device", tc.inject), auth)
			if identity.AllowInject != tc.want {
				t.Fatalf("AllowInject = %t, want %t", identity.AllowInject, tc.want)
			}
			body := applyCodexDeviceFingerprintBody(payload, identity.InstallationID, identity.AllowInject)
			got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String()
			if tc.want && got != devFPGoldenAccount1 {
				t.Fatalf("injected installation = %q, want %q", got, devFPGoldenAccount1)
			}
			if !tc.want && !bytes.Equal(body, payload) {
				t.Fatalf("body changed without inject: %s", body)
			}
		})
	}
}

// 3
func TestCodexDeviceFP_DistinguishesAuths(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	first := devFPResolveID(t, cfg, devFPAuth("codex-a.json", "acc-test-1"))
	second := devFPResolveID(t, cfg, devFPAuth("codex-b.json", "acc-test-2"))
	if first.InstallationID == second.InstallationID {
		t.Fatalf("different accounts share installation id %q", first.InstallationID)
	}
	if second.InstallationID != devFPGoldenAccount2 {
		t.Fatalf("second = %q, want %q", second.InstallationID, devFPGoldenAccount2)
	}
}

// 4
func TestCodexDeviceFP_BodyTurnMetadataRewritten(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.3-codex","client_metadata":{"x-codex-installation-id":"install-user-1","x-codex-turn-metadata":"{\"installation_id\":\"install-user-1\",\"turn_id\":\"turn-1\",\"sandbox\":\"seccomp\"}"}}`)
	body := applyCodexDeviceFingerprintBody(payload, devFPGoldenAccount1, false)
	raw := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	if got := gjson.Get(raw, "installation_id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("turn metadata installation_id = %q", got)
	}
	if gjson.Get(raw, "sandbox").String() != "seccomp" || gjson.Get(raw, "turn_id").String() != "turn-1" {
		t.Fatalf("turn metadata unrelated fields changed: %s", raw)
	}
	want := strings.ReplaceAll(string(payload), "install-user-1", devFPGoldenAccount1)
	if string(body) != want {
		t.Fatalf("body = %s\nwant  %s", body, want)
	}
}

// 5
func TestCodexDeviceFP_OffModes(t *testing.T) {
	devFPResetState(t)
	auth := devFPAuth("codex-off.json", "acc-test-1")
	exec := NewCodexExecutor(nil)
	payload := []byte(`{"model":"gpt-5.3-codex","client_metadata":{"x-codex-installation-id":"install-user-1","x-codex-turn-metadata":"{\"installation_id\":\"install-user-1\"}"}}`)
	baseline := devFPCacheHelperBody(t, exec, auth, "https://example.invalid/backend-api/codex/responses", payload, nil)
	for _, mode := range []string{"", "off", "session", "full", "bogus"} {
		if _, ok := resolveCodexDeviceIdentity(devFPConfig(mode), auth, false); ok {
			t.Fatalf("mode %q resolved a device identity", mode)
		}
		modeExec := NewCodexExecutor(devFPConfig(mode))
		body := devFPCacheHelperBody(t, modeExec, auth, "https://example.invalid/backend-api/codex/responses", payload, nil)
		if !bytes.Equal(body, baseline) {
			t.Fatalf("mode %q changed body:\n%s\nbaseline:\n%s", mode, body, baseline)
		}
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
		req.Header.Set("X-Codex-Installation-Id", "install-user-1")
		applyCodexDeviceFingerprintRequestHeaders(req.Header, devFPConfig(mode), auth, false)
		if req.Header.Get("X-Codex-Installation-Id") != "install-user-1" {
			t.Fatalf("mode %q touched headers", mode)
		}
	}
}

func devFPCacheHelperBody(t *testing.T, exec *CodexExecutor, auth *cliproxyauth.Auth, url string, payload []byte, headers http.Header) []byte {
	t.Helper()
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}
	_, body, err := exec.cacheHelper(context.Background(), sdktranslator.FromString("codex"), url, req, bytes.Clone(payload), auth, headers)
	if err != nil {
		t.Fatalf("cacheHelper: %v", err)
	}
	return body
}

// 6
func TestCodexDeviceFP_SessionIdentifiersUntouched(t *testing.T) {
	payload := []byte(`{"prompt_cache_key":"cache-1","client_metadata":{"session_id":"sess-1","thread_id":"thread-1","x-codex-window-id":"win-1","x-codex-installation-id":"install-1","x-codex-turn-metadata":"{\"installation_id\":\"install-1\",\"session_id\":\"sess-1\",\"turn_id\":\"turn-1\",\"thread_source\":\"user\"}"}}`)
	body := applyCodexDeviceFingerprintBody(payload, devFPGoldenAccount1, false)
	for path, want := range map[string]string{
		"prompt_cache_key":                  "cache-1",
		"client_metadata.session_id":        "sess-1",
		"client_metadata.thread_id":         "thread-1",
		"client_metadata.x-codex-window-id": "win-1",
	} {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	inner := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	if gjson.Get(inner, "session_id").String() != "sess-1" || gjson.Get(inner, "turn_id").String() != "turn-1" || gjson.Get(inner, "thread_source").String() != "user" {
		t.Fatalf("turn metadata session fields changed: %s", inner)
	}
	headers := http.Header{}
	headers.Set("Session-Id", "sess-1")
	headers.Set("Thread-Id", "thread-1")
	headers.Set("X-Codex-Window-Id", "win-1")
	headers.Set("X-Codex-Turn-State", "state-1")
	headers.Set("X-Codex-Turn-Metadata", `{"installation_id":"install-1","session_id":"sess-1","turn_id":"turn-1"}`)
	applyCodexDeviceFingerprintHeaders(headers, devFPGoldenAccount1)
	if headers.Get("Session-Id") != "sess-1" || headers.Get("Thread-Id") != "thread-1" || headers.Get("X-Codex-Window-Id") != "win-1" || headers.Get("X-Codex-Turn-State") != "state-1" {
		t.Fatalf("session headers changed: %v", headers)
	}
	turn := headers.Get("X-Codex-Turn-Metadata")
	if gjson.Get(turn, "session_id").String() != "sess-1" || gjson.Get(turn, "turn_id").String() != "turn-1" || gjson.Get(turn, "installation_id").String() != devFPGoldenAccount1 {
		t.Fatalf("turn metadata header = %s", turn)
	}
}

// 7
func TestCodexDeviceFP_SeedPriority(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")

	auth := devFPAuth("codex-priority.json", "acc-test-1")
	auth.Metadata["codex_installation_id"] = strings.ToUpper(devFPExplicitID)
	if identity := devFPResolveID(t, cfg, auth); identity.InstallationID != devFPExplicitID || identity.Source != codexDeviceFPSourceExplicit {
		t.Fatalf("metadata override = %+v, want explicit lowercased", identity)
	}
	attrOverride := "22222222-3333-4444-9555-666666666666"
	auth.Attributes["codex_installation_id"] = attrOverride
	if identity := devFPResolveID(t, cfg, auth); identity.InstallationID != attrOverride {
		t.Fatalf("attribute override did not win: %+v", identity)
	}

	accountOnly := devFPAuth("codex-priority-account.json", "acc-test-1")
	if identity := devFPResolveID(t, cfg, accountOnly); identity.InstallationID != devFPGoldenAccount1 || identity.Source != codexDeviceFPSourceAccount {
		t.Fatalf("account seed = %+v", identity)
	}

	authOnly := devFPAuth("codex-a.json", "")
	if identity := devFPResolveID(t, cfg, authOnly); identity.InstallationID != devFPGoldenAuthID || identity.Source != codexDeviceFPSourceAuthID {
		t.Fatalf("auth-id seed = %+v, want %s", identity, devFPGoldenAuthID)
	}

	if _, ok := resolveCodexDeviceIdentity(cfg, &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "t"}}, false); ok {
		t.Fatal("auth without any identity must not resolve")
	}
}

// 8
func TestCodexDeviceFP_StableAcrossRenameAndCase(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	first := devFPResolveID(t, cfg, devFPAuth("codex-user@example.com-pro.json", "acc-test-1"))
	renamed := devFPResolveID(t, cfg, devFPAuth("CODEX-renamed-plus.json", "acc-test-1"))
	lowered := devFPResolveID(t, cfg, devFPAuth("codex-renamed-plus.json", "acc-test-1"))
	if first.InstallationID != renamed.InstallationID || renamed.InstallationID != lowered.InstallationID {
		t.Fatalf("rename/case changed device: %s %s %s", first.InstallationID, renamed.InstallationID, lowered.InstallationID)
	}
}

// 9
func TestCodexDeviceFP_StableWhenRefreshClearsEmail(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-refresh-clears.json", "acc-test-1")
	auth.Metadata["email"] = "user@example.com"
	auth.Metadata["id_token"] = devFPIDToken("acc-test-1", "user-aaa")
	before := devFPResolveID(t, cfg, auth)
	auth.Metadata["email"] = ""
	auth.Metadata["id_token"] = ""
	after := devFPResolveID(t, cfg, auth)
	if before.InstallationID != after.InstallationID || after.InstallationID != devFPGoldenAccount1 {
		t.Fatalf("device changed after refresh cleared email/id_token: %s -> %s", before.InstallationID, after.InstallationID)
	}
}

// 10
func TestCodexDeviceFP_NoFlipWhenAccountIDAppears(t *testing.T) {
	devFPResetState(t)
	hook := devFPCaptureLogs(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-appears.json", "")
	auth.Metadata["id_token"] = devFPIDToken("acc-test-1", "")
	before := devFPResolveID(t, cfg, auth)
	if before.Source != codexDeviceFPSourceAccount || before.InstallationID != devFPGoldenAccount1 {
		t.Fatalf("id_token seed = %+v, want account golden", before)
	}
	auth.Metadata["account_id"] = "acc-test-1"
	after := devFPResolveID(t, cfg, auth)
	if after.InstallationID != before.InstallationID {
		t.Fatalf("device flipped when account_id appeared: %s -> %s", before.InstallationID, after.InstallationID)
	}

	bare := devFPAuth("codex-a.json", "")
	identity := devFPResolveID(t, cfg, bare)
	if identity.Source != codexDeviceFPSourceAuthID || identity.InstallationID != devFPGoldenAuthID {
		t.Fatalf("bare auth = %+v, want auth-id", identity)
	}
	if !devFPHasWarn(hook, "auth lacks account identity") {
		t.Fatal("auth-id fallback did not warn")
	}
}

// 11
func TestCodexDeviceFP_StickyCache(t *testing.T) {
	devFPResetState(t)
	hook := devFPCaptureLogs(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-sticky.json", "acc-test-1")
	auth.Metadata["id_token"] = devFPIDToken("acc-test-1", "")
	before := devFPResolveID(t, cfg, auth)
	delete(auth.Metadata, "account_id")
	auth.Metadata["id_token"] = ""
	cleared := devFPResolveID(t, cfg, auth)
	if cleared.InstallationID != before.InstallationID || cleared.Source != codexDeviceFPSourceAccount {
		t.Fatalf("sticky cache lost account: %+v (before %+v)", cleared, before)
	}
	auth.Metadata["account_id"] = "acc-test-2"
	changed := devFPResolveID(t, cfg, auth)
	if changed.InstallationID != devFPGoldenAccount2 {
		t.Fatalf("new account = %s, want %s", changed.InstallationID, devFPGoldenAccount2)
	}
	if !devFPHasWarn(hook, "account identity changed") {
		t.Fatal("account change did not warn")
	}
}

// 12
func TestCodexDeviceFP_TeamPlanUsesUserID(t *testing.T) {
	devFPResetState(t)
	hook := devFPCaptureLogs(t)
	cfg := devFPConfig("device")
	teamAuth := func(id string, userID string) *cliproxyauth.Auth {
		auth := devFPAuth(id, "acc-team-1")
		auth.Metadata["plan_type"] = "team"
		auth.Metadata["id_token"] = devFPIDToken("acc-team-1", userID)
		return auth
	}
	aaa := devFPResolveID(t, cfg, teamAuth("codex-team-a.json", "user-aaa"))
	bbb := devFPResolveID(t, cfg, teamAuth("codex-team-b.json", "user-bbb"))
	if aaa.InstallationID != devFPGoldenTeamUserAAA || aaa.Source != codexDeviceFPSourceAccountUser {
		t.Fatalf("team user aaa = %+v, want %s", aaa, devFPGoldenTeamUserAAA)
	}
	if bbb.InstallationID != devFPGoldenTeamUserBBB {
		t.Fatalf("team user bbb = %+v, want %s", bbb, devFPGoldenTeamUserBBB)
	}

	noUser := teamAuth("codex-team-nouser.json", "")
	identity := devFPResolveID(t, cfg, noUser)
	if identity.Source != codexDeviceFPSourceAccount || identity.InstallationID != devFPAccountID("acc-team-1") {
		t.Fatalf("team without user = %+v, want account fallback", identity)
	}
	if !devFPHasWarn(hook, "team auth without user id") {
		t.Fatal("team auth without user id did not warn")
	}

	attrPlan := devFPAuth("codex-team-attr.json", "acc-team-1")
	delete(attrPlan.Metadata, "plan_type")
	attrPlan.Attributes["plan_type"] = "Business"
	attrPlan.Metadata["id_token"] = devFPIDToken("acc-team-1", "user-aaa")
	if got := devFPResolveID(t, cfg, attrPlan); got.InstallationID != devFPGoldenTeamUserAAA {
		t.Fatalf("attribute plan_type ignored: %+v", got)
	}

	plus := devFPAuth("codex-plus-user.json", "acc-team-1")
	plus.Metadata["id_token"] = devFPIDToken("acc-team-1", "user-aaa")
	if got := devFPResolveID(t, cfg, plus); got.Source != codexDeviceFPSourceAccount || got.InstallationID != devFPAccountID("acc-team-1") {
		t.Fatalf("plus plan used user id: %+v", got)
	}
}

// 13
func TestCodexDeviceFP_GoldenVectors(t *testing.T) {
	for name, tc := range map[string]struct{ seed, want string }{
		"account acc-test-1":     {codexDeviceFPSeedPrefix + "account\x00acc-test-1", devFPGoldenAccount1},
		"account acc-test-2":     {codexDeviceFPSeedPrefix + "account\x00acc-test-2", devFPGoldenAccount2},
		"account-user user-aaa":  {codexDeviceFPSeedPrefix + "account-user\x00acc-team-1\x00user-aaa", devFPGoldenTeamUserAAA},
		"account-user user-bbb":  {codexDeviceFPSeedPrefix + "account-user\x00acc-team-1\x00user-bbb", devFPGoldenTeamUserBBB},
		"auth-id codex-a.json":   {codexDeviceFPSeedPrefix + "auth-id\x00codex-a.json", devFPGoldenAuthID},
		"same seed, same output": {codexDeviceFPSeedPrefix + "account\x00acc-test-1", devFPGoldenAccount1},
	} {
		got := codexStableUUIDv4(tc.seed)
		if got != tc.want {
			t.Fatalf("%s: got %s, want %s", name, got, tc.want)
		}
		parsed := uuid.MustParse(got)
		if parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
			t.Fatalf("%s: %s is not an RFC 4122 v4 UUID", name, got)
		}
	}
}

// 14
func TestCodexDeviceFP_ExplicitOverrideShape(t *testing.T) {
	if got, ok := codexValidV4(" " + strings.ToUpper(devFPExplicitID) + " "); !ok || got != devFPExplicitID {
		t.Fatalf("valid v4 rejected: %q %t", got, ok)
	}
	for name, raw := range map[string]string{
		"nil":          "00000000-0000-0000-0000-000000000000",
		"v1":           "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"v5":           "8ebad7cc-5ddb-5bd1-bb4c-c6e503ffbe20",
		"urn":          "urn:uuid:" + devFPExplicitID,
		"braces":       "{" + devFPExplicitID + "}",
		"no hyphens":   strings.ReplaceAll(devFPExplicitID, "-", ""),
		"bad variant":  "11111111-2222-4333-c444-555555555555",
		"ncs variant":  "11111111-2222-4333-0444-555555555555",
		"garbage":      "not-a-uuid",
		"empty":        "",
		"bad hyphens":  "11111111x2222-4333-8444-555555555555",
		"short":        "11111111-2222-4333-8444-55555555555",
		"invalid char": "1111111g-2222-4333-8444-555555555555",
	} {
		if got, ok := codexValidV4(raw); ok {
			t.Fatalf("%s: %q accepted as %q", name, raw, got)
		}
	}

	devFPResetState(t)
	hook := devFPCaptureLogs(t)
	auth := devFPAuth("codex-bad-override.json", "acc-test-1")
	auth.Metadata["codex_installation_id"] = "8ebad7cc-5ddb-5bd1-bb4c-c6e503ffbe20"
	if identity := devFPResolveID(t, devFPConfig("device"), auth); identity.Source != codexDeviceFPSourceAccount || identity.InstallationID != devFPGoldenAccount1 {
		t.Fatalf("invalid override not ignored: %+v", identity)
	}
	if !devFPHasWarn(hook, "invalid codex_installation_id") {
		t.Fatal("invalid override did not warn")
	}
}

// 15
func TestCodexDeviceFP_PerAuthOverride(t *testing.T) {
	devFPResetState(t)
	auth := devFPAuth("codex-per-auth.json", "acc-test-1")
	auth.Metadata["codex_device_fingerprint"] = "device"
	if _, ok := resolveCodexDeviceIdentity(devFPConfig("off"), auth, false); !ok {
		t.Fatal("metadata device mode did not enable convergence")
	}
	if _, ok := resolveCodexDeviceIdentity(nil, auth, false); !ok {
		t.Fatal("metadata device mode did not work without config")
	}
	auth.Metadata["codex_device_fingerprint"] = "off"
	if _, ok := resolveCodexDeviceIdentity(devFPConfig("device"), auth, false); ok {
		t.Fatal("metadata off did not disable convergence")
	}
	auth.Attributes["codex_device_fingerprint"] = "DEVICE"
	if _, ok := resolveCodexDeviceIdentity(devFPConfig("off"), auth, false); !ok {
		t.Fatal("attribute did not take precedence over metadata")
	}
	auth.Attributes["codex_device_fingerprint"] = "off"
	auth.Metadata["codex_device_fingerprint"] = "device"
	if _, ok := resolveCodexDeviceIdentity(devFPConfig("device"), auth, false); ok {
		t.Fatal("attribute off did not take precedence")
	}
}

// 16
func TestCodexDeviceFP_SkipsAPIKey(t *testing.T) {
	cfg := devFPConfig("device")
	key := &cliproxyauth.Auth{ID: "key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": "https://example.invalid/v1", "codex_device_fingerprint": "device"}}
	if _, ok := resolveCodexDeviceIdentity(cfg, key, false); ok {
		t.Fatal("API-key auth resolved a device identity")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v1/responses", nil)
	applyCodexHeadersWithSIWC(req, key, false, "sk-test", true, cfg, http.Header{"X-Codex-Turn-Metadata": {`{"installation_id":"client"}`}})
	if got := gjson.Get(req.Header.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got != "client" {
		t.Fatalf("API-key header rewritten: %q", got)
	}

	parsed, err := config.ParseConfigBytes([]byte("oauth: {providers: {codex: {device-fingerprint-mode: device, device-fingerprint-inject-missing: true}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Codex.DeviceFingerprintMode != "device" {
		t.Fatalf("OAuth config not parsed: %+v", parsed.Codex)
	}
	scoped := parsed.ForAPIKey()
	if scoped.Codex.DeviceFingerprintMode != "" || scoped.Codex.DeviceFingerprintInjectMissing != nil {
		t.Fatalf("ForAPIKey kept OAuth-only device settings: %+v", scoped.Codex)
	}
}

// 17
func TestCodexDeviceFP_SIWCRewritesExistingOnly(t *testing.T) {
	devFPResetState(t)
	enabled := true
	cfg := devFPConfigInject("device", &enabled)
	siwcAuth := func(deviceID string) *cliproxyauth.Auth {
		auth := devFPAuth("codex-siwc-devfp.json", "acc-test-1")
		auth.Metadata["use_siwc"] = true
		auth.Metadata["siwc"] = map[string]any{"access_token": "siwc-token", "device_id": deviceID}
		return auth
	}
	nonCodexClient := http.Header{"User-Agent": {"curl/8.0"}}
	exec := NewCodexExecutor(cfg)
	url := "https://api.openai.com/v1/responses"

	auth := siwcAuth(devFPSIWCDevice)
	if !codexUseSIWCForRequest(context.Background(), auth, nonCodexClient) {
		t.Fatal("test auth does not use SIWC")
	}
	withKey := []byte(`{"model":"gpt-5.3-codex","client_metadata":{"x-codex-installation-id":"client-1"}}`)
	body := devFPCacheHelperBody(t, exec, auth, url, withKey, nonCodexClient)
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != devFPSIWCDevice {
		t.Fatalf("SIWC installation = %q, want siwc device %q", got, devFPSIWCDevice)
	}
	withoutKey := []byte(`{"model":"gpt-5.3-codex"}`)
	body = devFPCacheHelperBody(t, exec, auth, url, withoutKey, nonCodexClient)
	if gjson.GetBytes(body, "client_metadata").Exists() {
		t.Fatalf("SIWC request injected installation: %s", body)
	}

	invalid := siwcAuth("not-a-v4")
	body = devFPCacheHelperBody(t, exec, invalid, url, withKey, nonCodexClient)
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("invalid siwc device_id fallback = %q, want derived %q", got, devFPGoldenAccount1)
	}

	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	clientHeaders := nonCodexClient.Clone()
	clientHeaders.Set("X-Codex-Installation-Id", "client-1")
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header["x-codex-installation-id"] = []string{"client-raw"}
	applyCodexHeadersWithSIWC(req, auth, true, "siwc-token", true, cfg, clientHeaders)
	for key := range req.Header {
		if strings.EqualFold(key, "X-Codex-Installation-Id") {
			t.Fatalf("SIWC request kept %s header: %v", key, req.Header)
		}
	}
}

// 18
func TestCodexDeviceFP_HTTPHeaderUsesParamSIWC(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-param-siwc.json", "acc-test-1")
	auth.Metadata["use_siwc"] = true
	auth.Metadata["siwc"] = map[string]any{"access_token": "siwc-token", "device_id": devFPSIWCDevice}
	auth.Attributes["header:X-Codex-Turn-Metadata"] = "$X-Codex-Turn-Metadata"
	turn := `{"installation_id":"client-1","turn_id":"t1"}`

	for _, tc := range []struct {
		useSIWC bool
		want    string
	}{
		{true, devFPSIWCDevice},
		{false, devFPGoldenAccount1},
	} {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
		applyCodexHeadersWithSIWC(req, auth, tc.useSIWC, "token", true, cfg, http.Header{"X-Codex-Turn-Metadata": {turn}, "User-Agent": {"curl/8.0"}})
		if got := gjson.Get(req.Header.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got != tc.want {
			t.Fatalf("useSIWC=%t installation = %q, want %q", tc.useSIWC, got, tc.want)
		}
	}

	// Direct image calls decide SIWC from the original client headers; the
	// headers passed down have User-Agent removed and must not flip the decision.
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/images/generations", nil)
	applyCodexDirectImageHeaders(req, auth, "token", false, cfg, http.Header{"User-Agent": {"codex_cli_rs/0.1"}, "X-Codex-Turn-Metadata": {turn}})
	if got := gjson.Get(req.Header.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("direct image installation = %q, want non-SIWC derived %q", got, devFPGoldenAccount1)
	}
}

// 19
func TestCodexDeviceFP_HeaderAllCaseVariantsAndValues(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-header-variants.json", "acc-test-1")
	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	client := http.Header{}
	client.Set("X-Codex-Installation-Id", "client-1")
	client.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-1","turn_id":"t1"}`)

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	req.Header["x-codex-turn-metadata"] = []string{`{"installation_id":"client-2"}`, `not-json`, `{"turn_id":"t3"}`}
	req.Header["x-codex-installation-id"] = []string{"client-raw"}
	applyCodexHeadersWithSIWC(req, auth, false, "token", true, cfg, client)

	turnValues := 0
	for key, values := range req.Header {
		if strings.EqualFold(key, "X-Codex-Installation-Id") {
			t.Fatalf("installation header %s survived: %v", key, values)
		}
		if !strings.EqualFold(key, "X-Codex-Turn-Metadata") {
			continue
		}
		for _, value := range values {
			turnValues++
			if !gjson.Valid(value) {
				t.Fatalf("invalid turn metadata value kept under %s: %q", key, value)
			}
			if id := gjson.Get(value, "installation_id"); id.Exists() && id.String() != devFPGoldenAccount1 {
				t.Fatalf("turn metadata %s value not rewritten: %s", key, value)
			}
		}
	}
	if turnValues != 3 {
		t.Fatalf("turn metadata values = %d, want 3 (canonical + two valid lowercase): %v", turnValues, req.Header)
	}
	if got := req.Header["x-codex-turn-metadata"]; len(got) != 2 || got[1] != `{"turn_id":"t3"}` {
		t.Fatalf("lowercase variant values = %v", got)
	}

	onlyInvalid := http.Header{"x-codex-turn-metadata": {"[1,2]", `"str"`}}
	applyCodexDeviceFingerprintHeaders(onlyInvalid, devFPGoldenAccount1)
	if len(onlyInvalid) != 0 {
		t.Fatalf("key with only invalid values not removed: %v", onlyInvalid)
	}
}

// 20
func TestCodexDeviceFP_HTTPHeaderIdempotent(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-idempotent.json", "acc-test-1")
	client := http.Header{"X-Codex-Turn-Metadata": {`{"installation_id":"client-1","turn_id":"t1"}`}}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	applyCodexHeadersWithSIWC(req, auth, false, "token", true, cfg, client)
	first := req.Header.Clone()
	applyCodexHeadersWithSIWC(req, auth, false, "token", true, cfg, client)
	applyCodexDeviceFingerprintHeaders(req.Header, devFPGoldenAccount1)
	if len(first) != len(req.Header) {
		t.Fatalf("header count changed: %v vs %v", first, req.Header)
	}
	for key, values := range first {
		if strings.Join(req.Header[key], "\n") != strings.Join(values, "\n") {
			t.Fatalf("header %s changed on reapply: %v -> %v", key, values, req.Header[key])
		}
	}
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"c","x-codex-turn-metadata":"{\"installation_id\":\"c\"}"}}`)
	once := applyCodexDeviceFingerprintBody(body, devFPGoldenAccount1, false)
	if twice := applyCodexDeviceFingerprintBody(once, devFPGoldenAccount1, false); !bytes.Equal(once, twice) {
		t.Fatalf("body rewrite not idempotent: %s vs %s", once, twice)
	}
}

type devFPCapturedRequest struct {
	path    string
	headers http.Header
	body    []byte
}

// 21
func TestCodexDeviceFP_HTTPEndToEnd(t *testing.T) {
	devFPResetState(t)
	captured := make(chan devFPCapturedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- devFPCapturedRequest{path: r.URL.Path, headers: r.Header.Clone(), body: body}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_c","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.3-codex\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	cfg := devFPConfig("device")
	exec := NewCodexExecutor(cfg)
	auth := devFPAuth("codex-http-e2e.json", "acc-e2e")
	auth.Attributes["base_url"] = server.URL
	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	want := devFPAccountID("acc-e2e")
	payload := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"client_metadata":{"x-codex-installation-id":"client-1","x-codex-turn-metadata":"{\"installation_id\":\"client-1\",\"turn_id\":\"t1\"}"}}`)
	clientHeaders := http.Header{}
	clientHeaders.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-1","turn_id":"t1"}`)
	clientHeaders.Set("X-Codex-Installation-Id", "client-1")

	check := func(name string, got devFPCapturedRequest, wantPath string) {
		t.Helper()
		if got.path != wantPath {
			t.Fatalf("%s: path = %s, want %s", name, got.path, wantPath)
		}
		if id := gjson.GetBytes(got.body, "client_metadata.x-codex-installation-id").String(); id != want {
			t.Fatalf("%s: body installation = %q, want %q; body=%s", name, id, want, got.body)
		}
		inner := gjson.GetBytes(got.body, "client_metadata.x-codex-turn-metadata").String()
		if id := gjson.Get(inner, "installation_id").String(); id != want {
			t.Fatalf("%s: body turn metadata installation = %q", name, id)
		}
		if id := gjson.Get(got.headers.Get("X-Codex-Turn-Metadata"), "installation_id").String(); id != want {
			t.Fatalf("%s: header turn metadata installation = %q", name, id)
		}
		if got.headers.Get("X-Codex-Installation-Id") != "" {
			t.Fatalf("%s: X-Codex-Installation-Id forwarded", name)
		}
	}

	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Headers: clientHeaders}
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	check("execute", <-captured, "/responses")

	streamOpts := opts
	streamOpts.Stream = true
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}, streamOpts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk: %v", chunk.Err)
		}
	}
	check("stream", <-captured, "/responses")

	compactOpts := opts
	compactOpts.Alt = "responses/compact"
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}, compactOpts); err != nil {
		t.Fatalf("compact: %v", err)
	}
	check("compact", <-captured, "/responses/compact")

	// zstd only applies to the ChatGPT backend host, so inspect the wire body directly.
	zstdCfg := devFPConfig("device")
	zstdCfg.Codex.UpstreamRequestCompression = true
	zstdCfg.Codex.UpstreamRequestCompressionMinBytes = 1
	zstdExec := NewCodexExecutor(zstdCfg)
	large := []byte(`{"model":"gpt-5.3-codex","input":"` + strings.Repeat("compressible ", 400) + `","client_metadata":{"x-codex-installation-id":"client-1"}}`)
	httpReq, upstreamBody, err := zstdExec.cacheHelper(context.Background(), sdktranslator.FromString("codex"), "https://chatgpt.com/backend-api/codex/responses", cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: large}, bytes.Clone(large), auth, nil)
	if err != nil {
		t.Fatalf("cacheHelper zstd: %v", err)
	}
	if httpReq.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("request was not zstd encoded")
	}
	wire, _ := io.ReadAll(httpReq.Body)
	decoder, _ := zstd.NewReader(nil)
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(wire, nil)
	if err != nil {
		t.Fatalf("decode zstd: %v", err)
	}
	if !bytes.Equal(decoded, upstreamBody) {
		t.Fatal("zstd wire body differs from upstream body used for retries/logs")
	}
	if id := gjson.GetBytes(decoded, "client_metadata.x-codex-installation-id").String(); id != want {
		t.Fatalf("zstd body installation = %q, want %q", id, want)
	}
}

// 22
func TestCodexDeviceFP_DirectImagesBodyUntouched(t *testing.T) {
	devFPResetState(t)
	exec := NewCodexExecutor(devFPConfig("device"))
	auth := devFPAuth("codex-images.json", "acc-test-1")
	payload := []byte(`{"model":"gpt-image-2","prompt":"cat","client_metadata":{"x-codex-installation-id":"client-1"}}`)
	for _, url := range []string{"https://chatgpt.com/backend-api/codex/images/generations", "https://chatgpt.com/backend-api/codex/images/edits"} {
		body := devFPCacheHelperBody(t, exec, auth, url, payload, nil)
		if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != "client-1" {
			t.Fatalf("%s body rewritten: %q", url, got)
		}
	}
	for url, want := range map[string]bool{
		"https://chatgpt.com/backend-api/codex/responses":          true,
		"https://chatgpt.com/backend-api/codex/responses/":         true,
		"https://chatgpt.com/backend-api/codex/responses/compact":  true,
		"https://api.openai.com/v1/responses":                      true,
		"https://chatgpt.com/backend-api/codex/images/generations": false,
		"https://chatgpt.com/backend-api/codex/alpha/search":       false,
		"://bad url": false,
	} {
		if got := codexDeviceFPBodyEligible(url); got != want {
			t.Fatalf("eligible(%q) = %t, want %t", url, got, want)
		}
	}
}

// 23
func TestCodexDeviceFP_DuplicateKeys(t *testing.T) {
	payload := []byte(`{"model":"m","client_metadata":{"x-codex-installation-id":"a","session_id":"s-1","x-codex-installation-id":"b","x-codex-turn-metadata":"{\"installation_id\":\"a\",\"turn_id\":\"t\",\"installation_id\":\"b\",\"n\":[1, 2]}","extra":{"k": [1,2]}},"tail":true}`)
	body := applyCodexDeviceFingerprintBody(payload, devFPGoldenAccount1, false)
	if !gjson.ValidBytes(body) {
		t.Fatalf("invalid JSON: %s", body)
	}
	installations := 0
	gjson.GetBytes(body, "client_metadata").ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "x-codex-installation-id":
			installations++
			if value.String() != devFPGoldenAccount1 {
				t.Fatalf("duplicate installation not rewritten: %s", value.Raw)
			}
		case "session_id":
			if value.Raw != `"s-1"` {
				t.Fatalf("session_id raw changed: %s", value.Raw)
			}
		case "extra":
			if value.Raw != `{"k": [1,2]}` {
				t.Fatalf("extra raw changed: %s", value.Raw)
			}
		case "x-codex-turn-metadata":
			inner := 0
			gjson.Parse(value.String()).ForEach(func(k, v gjson.Result) bool {
				switch k.String() {
				case "installation_id":
					inner++
					if v.String() != devFPGoldenAccount1 {
						t.Fatalf("inner duplicate not rewritten: %s", value.String())
					}
				case "n":
					if v.Raw != "[1, 2]" {
						t.Fatalf("inner raw changed: %s", v.Raw)
					}
				case "turn_id":
					if v.Raw != `"t"` {
						t.Fatalf("turn_id raw changed: %s", v.Raw)
					}
				}
				return true
			})
			if inner != 2 {
				t.Fatalf("inner installation keys = %d, want 2", inner)
			}
		}
		return true
	})
	if installations != 2 {
		t.Fatalf("outer installation keys = %d, want 2", installations)
	}
	if !bytes.HasPrefix(body, []byte(`{"model":"m","client_metadata":`)) || !bytes.HasSuffix(body, []byte(`,"tail":true}`)) {
		t.Fatalf("bytes outside client_metadata changed: %s", body)
	}

	headerInner, keep := rewriteCodexTurnMetadataInstallationID(`{"installation_id":"a","installation_id":"b"}`, devFPGoldenAccount1)
	if !keep || headerInner != `{"installation_id":"`+devFPGoldenAccount1+`","installation_id":"`+devFPGoldenAccount1+`"}` {
		t.Fatalf("header duplicate rewrite = %q keep=%t", headerInner, keep)
	}
}

// 24
func TestCodexDeviceFP_RefreshKeepsCustomKeys(t *testing.T) {
	proxyURL, teardown := startCodexMockOAuthServers(t, makeTestCodexRefreshJWT("plus", "acc-refresh"))
	defer teardown()
	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-devfp-refresh",
		Provider: "codex",
		ProxyURL: proxyURL,
		Metadata: map[string]any{
			"refresh_token":            "valid-refresh-token",
			"account_id":               "acc-refresh",
			"codex_device_fingerprint": "device",
			"codex_installation_id":    devFPExplicitID,
		},
		Attributes: map[string]string{"plan_type": "plus"},
	}
	refreshed, err := exec.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.Metadata["codex_device_fingerprint"] != "device" || refreshed.Metadata["codex_installation_id"] != devFPExplicitID {
		t.Fatalf("refresh dropped custom keys from metadata: %v", refreshed.Metadata)
	}
	store := newDevFPFileStore(t)
	saved := store.save(t, refreshed)
	if saved["codex_device_fingerprint"] != "device" || saved["codex_installation_id"] != devFPExplicitID {
		t.Fatalf("persisted auth file dropped custom keys: %v", saved)
	}
}

// 25
func TestCodexDeviceFP_InvalidInnerTurnMetadataRemoved(t *testing.T) {
	for name, inner := range map[string]string{
		"invalid json": `"{not json"`,
		"array":        `"[1,2]"`,
		"string":       `"\"plain\""`,
		"number":       `"42"`,
		"empty":        `""`,
		"non-string":   `{"installation_id":"client"}`,
	} {
		payload := []byte(`{"client_metadata":{"x-codex-installation-id":"client","x-codex-turn-metadata":` + inner + `,"session_id":"s"}}`)
		body := applyCodexDeviceFingerprintBody(payload, devFPGoldenAccount1, false)
		if gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Exists() {
			t.Fatalf("%s: invalid turn metadata kept: %s", name, body)
		}
		if gjson.GetBytes(body, "client_metadata.session_id").String() != "s" || gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String() != devFPGoldenAccount1 {
			t.Fatalf("%s: other keys damaged: %s", name, body)
		}
	}
	noKey := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"t\"}"}}`)
	if body := applyCodexDeviceFingerprintBody(noKey, devFPGoldenAccount1, false); !bytes.Equal(body, noKey) {
		t.Fatalf("object without installation_id changed: %s", body)
	}
	headers := http.Header{"X-Codex-Turn-Metadata": {"{bad", `{"installation_id":"c"}`}}
	applyCodexDeviceFingerprintHeaders(headers, devFPGoldenAccount1)
	if values := headers.Values("X-Codex-Turn-Metadata"); len(values) != 1 || gjson.Get(values[0], "installation_id").String() != devFPGoldenAccount1 {
		t.Fatalf("header invalid value not removed: %v", values)
	}
}

// 26
func TestCodexDeviceFP_OuterEncodingRoundTrip(t *testing.T) {
	inner := `{"installation_id":"client-1","workspaces":{"/tmp/a&b<c>d":{"x":"q\"uote\\"}},"path":"a/b"}`
	outer := encodeTurnMetadataOuter(inner)
	payload := []byte(`{"client_metadata":{"x-codex-turn-metadata":` + string(outer) + `}}`)
	if !gjson.ValidBytes(payload) {
		t.Fatalf("encoder produced invalid JSON: %s", outer)
	}
	if gjson.GetBytes(payload, "client_metadata.x-codex-turn-metadata").Raw != string(outer) {
		t.Fatal("gjson Raw is not the quoted encoder output")
	}
	before := codexDeviceFPOuterEncodingMismatch.Load()
	body := applyCodexDeviceFingerprintBody(payload, devFPGoldenAccount1, false)
	want := strings.Replace(string(payload), "client-1", devFPGoldenAccount1, 1)
	if string(body) != want {
		t.Fatalf("only the UUID segment may change:\n got %s\nwant %s", body, want)
	}
	if codexDeviceFPOuterEncodingMismatch.Load() != before {
		t.Fatal("native outer encoding counted as mismatch")
	}

	goStyle := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"client-1\",\"p\":\"a\u0026b\u003c\"}"}}`)
	body = applyCodexDeviceFingerprintBody(goStyle, devFPGoldenAccount1, false)
	if !gjson.ValidBytes(body) {
		t.Fatalf("Go-style outer rewrite produced invalid JSON: %s", body)
	}
	innerOut := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	if gjson.Get(innerOut, "installation_id").String() != devFPGoldenAccount1 || gjson.Get(innerOut, "p").String() != "a&b<" {
		t.Fatalf("Go-style inner = %s", innerOut)
	}
	if codexDeviceFPOuterEncodingMismatch.Load() != before+1 {
		t.Fatal("Go-style outer encoding not counted")
	}
	if got := string(encodeTurnMetadataOuter("a\n\x01\t")); got != `"a\n\u0001\t"` {
		t.Fatalf("control escape = %s", got)
	}
}

// 27
func TestCodexDeviceFP_WebsocketHeaders(t *testing.T) {
	devFPResetState(t)
	cfg := devFPConfig("device")
	auth := devFPAuth("codex-ws-headers.json", "acc-test-1")
	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	client := http.Header{}
	client.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-1","turn_id":"t1"}`)
	client.Set("X-Codex-Installation-Id", "client-1")
	client.Set("Session_id", "sess-1")
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "token", cfg, false, client)
	if got := gjson.Get(headerValueCaseInsensitive(headers, "x-codex-turn-metadata"), "installation_id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("websocket turn metadata installation = %q", got)
	}
	if headerValueCaseInsensitive(headers, "X-Codex-Installation-Id") != "" {
		t.Fatalf("websocket installation header kept: %v", headers)
	}

	siwc := devFPAuth("codex-ws-headers-siwc.json", "acc-test-1")
	siwc.Metadata["use_siwc"] = true
	siwc.Metadata["siwc"] = map[string]any{"access_token": "siwc-token", "device_id": devFPSIWCDevice}
	siwc.Attributes["header:X-Codex-Turn-Metadata"] = "$X-Codex-Turn-Metadata"
	siwc.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	siwcClient := client.Clone()
	siwcClient.Set("User-Agent", "curl/8.0")
	headers = applyCodexWebsocketHeaders(context.Background(), http.Header{}, siwc, "siwc-token", cfg, false, siwcClient)
	if got := gjson.Get(headerValueCaseInsensitive(headers, "X-Codex-Turn-Metadata"), "installation_id").String(); got != devFPSIWCDevice {
		t.Fatalf("SIWC websocket turn metadata installation = %q, want %q", got, devFPSIWCDevice)
	}
	if headerValueCaseInsensitive(headers, "X-Codex-Installation-Id") != "" {
		t.Fatalf("SIWC websocket installation header kept: %v", headers)
	}
}

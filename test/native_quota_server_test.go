package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestNativeQuotaCandidateServerHTTPManagement(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	key := "isolated-native-quota-test-only"
	hash, errHash := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	cfg := &config.Config{Host: "127.0.0.1", NativeQuota: config.DefaultNativeQuotaConfig()}
	cfg.CommercialMode = true
	cfg.RemoteManagement.SecretKey = string(hash)
	cfg.RemoteManagement.DisableControlPanel = false
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	root := t.TempDir()
	cfg.AuthDir = filepath.Join(root, "auth")
	path := filepath.Join(root, "config.yaml")
	raw, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	server := api.NewServer(cfg, manager, sdkaccess.NewManager(), path)
	upstream := httptest.NewServer(server.Handler())
	defer upstream.Close()
	client := upstream.Client()
	call := func(method, path string, body []byte, authorized bool) (int, []byte, http.Header) {
		t.Helper()
		req, errRequest := http.NewRequest(method, upstream.URL+path, bytes.NewReader(body))
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, errDo := client.Do(req)
		if errDo != nil {
			t.Fatal(errDo)
		}
		defer func() { _ = res.Body.Close() }()
		out, errRead := io.ReadAll(res.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		return res.StatusCode, out, res.Header
	}
	status, html, headers := call("GET", "/native-quota.html", nil, false)
	if status != 200 || !strings.Contains(string(html), "native-quota") {
		t.Fatalf("UI shell status=%d body=%s", status, html)
	}
	if strings.Contains(string(html), key) || headers.Get("Content-Security-Policy") == "" {
		t.Fatal("unsafe UI shell")
	}
	status, _, _ = call("GET", "/v0/management/native-quota/status", nil, false)
	if status != 401 {
		t.Fatalf("unauthenticated status=%d", status)
	}
	status, body, _ := call("GET", "/v0/management/native-quota/status", nil, true)
	if status != 200 {
		t.Fatalf("authenticated status=%d body=%s", status, body)
	}
	var state struct {
		RuntimeMode string `json:"runtime_mode"`
		Total       int    `json:"total"`
	}
	if errDecode := json.Unmarshal(body, &state); errDecode != nil {
		t.Fatal(errDecode)
	}
	if state.RuntimeMode != "off" || state.Total != 0 {
		t.Fatalf("unexpected state %+v", state)
	}
	status, _, _ = call("POST", "/v0/management/native-quota/refresh", []byte(`{"auth_id":"not-a-real-account"}`), true)
	if status != 404 {
		t.Fatalf("unknown account status=%d", status)
	}
	status, _, _ = call("POST", "/v0/management/native-quota/plugins/stop", []byte(`{"id":"../../unsafe"}`), true)
	if status != 400 {
		t.Fatalf("invalid plugin status=%d", status)
	}
	status, _, _ = call("PUT", "/v0/management/native-quota/config", []byte(`{"mode":"enforce"}`), true)
	if status != 400 {
		t.Fatalf("partial config unexpectedly accepted status=%d", status)
	}
}

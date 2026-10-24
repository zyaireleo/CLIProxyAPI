package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNativeQuotaSnapshotPathsAreInstanceSpecificAndOutsideAuth(t *testing.T) {
	root := t.TempDir()
	auth := filepath.Join(root, "auth")
	first, errPath := nativeQuotaSnapshotPath(filepath.Join(root, "cpa1.yaml"), auth)
	if errPath != nil {
		t.Fatal(errPath)
	}
	second, errPath := nativeQuotaSnapshotPath(filepath.Join(root, "cpa2.yaml"), auth)
	if errPath != nil {
		t.Fatal(errPath)
	}
	if first == second || strings.HasPrefix(first, auth+string(filepath.Separator)) {
		t.Fatalf("snapshot isolation failed: %q / %q", first, second)
	}
	if path, errPath := nativeQuotaSnapshotPath("", auth); path != "" || errPath != nil {
		t.Fatalf("anonymous service should not persist: %q %v", path, errPath)
	}
}

func TestNativeQuotaPersistenceOffHasNoFilesystemSideEffects(t *testing.T) {
	root := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	s := &Service{coreManager: manager, configPath: filepath.Join(root, "test.yaml")}
	cfg := &config.Config{AuthDir: filepath.Join(root, "auth")}
	if err := s.configureNativeQuotaPersistence(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	entries, errRead := os.ReadDir(root)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if len(entries) != 0 || s.nativeQuotaPersistence != nil {
		t.Fatal("off mode created persistence state")
	}
}

func TestNativeQuotaPersistenceFlushAndRestoreWithoutCredentialFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	cfg := nativequota.DefaultConfig()
	cfg.Mode = nativequota.ModeShadow
	e := nativequota.New(cfg, nil)
	e.SetAccounts([]nativequota.Account{{ID: "fixture", Provider: "antigravity", Revision: "principal"}})
	value := 0.0049
	ob := nativequota.Observation{AuthID: "fixture", Provider: "antigravity", Source: "antigravity", ObservedAt: time.Now(), Pools: []nativequota.Pool{{ID: "gemini", Windows: []nativequota.Window{{ID: "weekly", Remaining: &value, ResetAt: time.Now().Add(time.Hour)}}}}}
	if !e.Observe(ob) {
		t.Fatal("observation rejected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go runNativeQuotaPersistence(ctx, e, path, done, time.Hour)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("persistence failed to stop")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, disallowed := range []string{"access_token", "refresh_token", "\"priority\"", "\"disabled\""} {
		if strings.Contains(string(raw), disallowed) {
			t.Fatalf("snapshot leaked field %s", disallowed)
		}
	}
	restored := nativequota.New(cfg, nil)
	restored.SetAccounts([]nativequota.Account{{ID: "fixture", Provider: "antigravity", Revision: "principal"}})
	if errLoad := restored.LoadSnapshot(path); errLoad != nil {
		t.Fatal(errLoad)
	}
	view := restored.Assess("fixture", "antigravity", "gemini-3.8-flash", time.Now())
	if len(view.Pools) != 1 || view.Pools[0].Windows[0].Remaining == nil || *view.Pools[0].Windows[0].Remaining != value {
		t.Fatalf("snapshot lost fractional evidence: %+v", view)
	}
}

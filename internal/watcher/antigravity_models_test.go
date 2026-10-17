package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/stretchr/testify/require"
)

func TestAntigravityCatalogRefreshTargetsOnlyItsProvider(t *testing.T) {
	a := &coreauth.Auth{ID: "ag", Provider: "antigravity"}
	b := &coreauth.Auth{ID: "codex", Provider: "codex"}
	w := &Watcher{currentAuths: map[string]*coreauth.Auth{a.ID: a, b.ID: b}, authQueue: make(chan AuthUpdate, 4)}
	updates := w.prepareAuthUpdatesLocked([]*coreauth.Auth{a, b}, false, "antigravity")
	require.Len(t, updates, 1)
	require.Equal(t, "ag", updates[0].ID)
	require.Equal(t, AuthUpdateActionModify, updates[0].Action)
}

func TestAntigravityCatalogInvalidReloadRetainsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	old := &config.Config{AntigravityModels: config.AntigravityModelsConfig{DiscoveryEnabled: true}}
	w := &Watcher{configPath: path}
	w.SetConfig(old)
	require.NoError(t, os.WriteFile(path, []byte("antigravity-models:\n  refresh-interval: invalid\n"), 0600))
	require.False(t, w.reloadConfig())
	require.Same(t, old, w.config)
}

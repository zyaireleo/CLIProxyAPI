package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileTokenStoreListExcludesModelCatalogSnapshots(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(directory, "nested"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(directory, ".antigravity-models"), 0o700))
	for path, document := range map[string]string{
		"antigravity-real.json":  `{"type":"antigravity","refresh_token":"test-refresh"}`,
		"nested/codex-real.json": `{"type":"codex","access_token":"test-access"}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, path), []byte(document), 0o600))
	}
	for index := 0; index < 192; index++ {
		path := filepath.Join(directory, ".antigravity-models", fmt.Sprintf("%064x.json", index))
		require.NoError(t, os.WriteFile(path, []byte(`{"models":[],"route":"test-route","success":"2026-10-18T00:00:00Z"}`), 0o600))
	}
	store := NewFileTokenStore()
	store.SetBaseDir(directory)
	files, err := store.List(context.Background())
	require.NoError(t, err)
	require.Len(t, files, 2)
	providers := make([]string, 0, len(files))
	for _, file := range files {
		providers = append(providers, file.Provider)
	}
	require.ElementsMatch(t, []string{"antigravity", "codex"}, providers)
	entries, err := os.ReadDir(filepath.Join(directory, ".antigravity-models"))
	require.NoError(t, err)
	require.Len(t, entries, 192)
}

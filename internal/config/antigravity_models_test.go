package config

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAntigravityModelsConfigRoundTrip(t *testing.T) {
	data := []byte("antigravity-models:\n  discovery-enabled: true\n  refresh-interval: 3h\n  additional-models:\n    - id: new-model\n      display-name: New model\n      auth-ids: [account-a]\n")
	cfg, err := ParseConfigBytes(data)
	require.NoError(t, err)
	require.Equal(t, 3*time.Hour, cfg.AntigravityModels.Interval())
	clone := cfg.CloneForRuntime()
	clone.AntigravityModels.AdditionalModels[0].AuthIDs[0] = "other"
	require.Equal(t, "account-a", cfg.AntigravityModels.AdditionalModels[0].AuthIDs[0])
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, data, 0600))
	require.NoError(t, SaveConfigPreserveComments(path, cfg))
	loaded, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, cfg.AntigravityModels, loaded.AntigravityModels)
	defaults, err := ParseConfigBytes([]byte("port: 8317\n"))
	require.NoError(t, err)
	require.False(t, defaults.AntigravityModels.DiscoveryEnabled)
	require.Equal(t, 3*time.Hour, defaults.AntigravityModels.Interval())
}

func TestAntigravityModelsRejectInvalidConfiguration(t *testing.T) {
	for _, body := range []string{"refresh-interval: wrong", "refresh-interval: 0s", "refresh-interval: 2s", "additional-models: [{id: ''}]", "additional-models: [{id: 'bad model'}]", "additional-models: [{id: valid, auth-ids: ['']}]"} {
		data := []byte("antigravity-models:\n  " + body + "\n")
		_, err := ParseConfigBytes(data)
		require.Error(t, err)
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, data, 0600))
		_, err = LoadConfig(path)
		require.Error(t, err)
	}
}

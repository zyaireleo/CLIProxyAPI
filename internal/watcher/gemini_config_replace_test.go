package watcher

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	"gopkg.in/yaml.v3"
)

func TestConfigWatchSurvivesReplacementAndAppliesGeminiBudget(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	cfg := &config.Config{Port: 8080, AuthDir: authDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()}
	write := func(budget int, replace bool) {
		t.Helper()
		cfg.AntigravityGeminiMaxAttempts = budget
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		target := path
		if replace {
			target += ".next"
		}
		if err = os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if replace {
			if runtime.GOOS == "windows" {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Rename(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(0, false)
	reloaded := make(chan int, 16)
	w, err := NewWatcher(path, authDir, func(current *config.Config) { reloaded <- current.AntigravityGeminiMaxAttempts })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	w.SetConfig(&config.Config{Port: 8080, AuthDir: authDir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{4, 2, 4} {
		write(budget, true)
		deadline := time.After(3 * time.Second)
		for {
			select {
			case current := <-reloaded:
				if current == budget {
					goto applied
				}
			case <-deadline:
				t.Fatalf("replacement did not apply generation budget %d", budget)
			}
		}
	applied:
	}
}

func TestGeminiBudgetAppearsInConfigDiff(t *testing.T) {
	changes := diff.BuildConfigChangeDetails(&config.Config{}, &config.Config{AntigravityGeminiMaxAttempts: 4})
	if !strings.Contains(strings.Join(changes, "\n"), "antigravity-gemini-max-attempts: 0 -> 4") {
		t.Fatal("generation budget changes are missing from safe configuration diagnostics")
	}
}

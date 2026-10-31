package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildConfigChangeDetailsCodexDeviceFingerprint(t *testing.T) {
	enabled := true
	oldCfg := &config.Config{}
	newCfg := &config.Config{Codex: config.CodexConfig{DeviceFingerprintMode: "device", DeviceFingerprintInjectMissing: &enabled}}
	joined := strings.Join(BuildConfigChangeDetails(oldCfg, newCfg), "\n")
	for _, want := range []string{
		`codex.device-fingerprint-mode: "" -> "device"`,
		"codex.device-fingerprint-inject-missing: inherit -> true",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing change %q in:\n%s", want, joined)
		}
	}
	reverse := strings.Join(BuildConfigChangeDetails(newCfg, oldCfg), "\n")
	if !strings.Contains(reverse, `codex.device-fingerprint-mode: "device" -> ""`) {
		t.Fatalf("missing reverse change in:\n%s", reverse)
	}
	same := &config.Config{Codex: config.CodexConfig{DeviceFingerprintMode: " device "}}
	for _, line := range BuildConfigChangeDetails(newCfg, same) {
		if strings.HasPrefix(line, "codex.device-fingerprint-mode") {
			t.Fatalf("whitespace-only mode change reported: %s", line)
		}
	}
}

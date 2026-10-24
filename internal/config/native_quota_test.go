package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"gopkg.in/yaml.v3"
)

func TestNativeQuotaConfigDefaultsAndIsolation(t *testing.T) {
	c := NativeQuotaConfig{}
	r, err := c.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != nativequota.ModeOff || r.MaxWait != 2*time.Second {
		t.Fatalf("unsafe defaults: %+v", r)
	}
	c.Providers = []string{"codex"}
	r, err = c.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Providers[0] = "antigravity"
	if r.Providers[0] != "codex" {
		t.Fatal("runtime aliases caller providers")
	}
	full := &Config{NativeQuota: c}
	clone := full.CloneForRuntime()
	clone.NativeQuota.Providers[0] = "codex"
	if full.NativeQuota.Providers[0] != "antigravity" {
		t.Fatal("config clone aliases providers")
	}
	empty, err := (NativeQuotaConfig{Providers: []string{}}).RuntimeConfig()
	if err != nil || len(empty.Providers) != 0 {
		t.Fatalf("explicit empty providers lost: %+v %v", empty, err)
	}
}

func TestNativeQuotaProvidersSavePreservesEmptyVersusDefault(t *testing.T) {
	for _, source := range []string{"port: 1234\n", "native-quota:\n  providers: null\n", "native-quota:\n  providers: [codex]\n"} {
		for _, providers := range [][]string{nil, {}, {"codex"}} {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{NativeQuota: NativeQuotaConfig{Providers: providers}}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var restored Config
			if err = yaml.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if (restored.NativeQuota.Providers == nil) != (providers == nil) || len(restored.NativeQuota.Providers) != len(providers) {
				t.Fatalf("coverage changed: before=%#v after=%#v YAML=%s", providers, restored.NativeQuota.Providers, raw)
			}
		}
	}
}

func TestNativeQuotaConfigRejectsInvalidValues(t *testing.T) {
	for _, c := range []NativeQuotaConfig{
		{Mode: "active"}, {Mode: "ENFORCE"}, {MaxWait: "3s"}, {MaxWait: "0s"},
		{CodexInterval: "never"}, {AntigravityInterval: "-1s"}, {RefreshConcurrency: -1},
		{QueueLimit: -1}, {MaxInFlight: -1}, {Providers: []string{"http://localhost"}},
		{Providers: []string{"codex", "codex"}}, {MaxInFlight: 1, UnknownMaxInFlight: 2},
	} {
		if _, err := c.RuntimeConfig(); err == nil {
			t.Errorf("accepted invalid config: %+v", c)
		}
	}
	if _, err := ParseConfigBytes([]byte("native-quota:\n  mode: enforce\n  max-wait: 3s\n")); err == nil {
		t.Fatal("YAML parser accepted invalid quota config")
	}
}

func TestNativeQuotaConfigConflictsAreProviderScoped(t *testing.T) {
	on := true
	for _, test := range []struct {
		providers []string
		plugin    string
		conflict  bool
	}{
		{[]string{"antigravity"}, "codex-quota-scheduler", false},
		{[]string{"codex"}, "antigravity-priority", false},
		{[]string{"antigravity"}, "antigravity-priority", true},
		{[]string{"codex"}, "codex-quota-scheduler", true},
		{[]string{}, "antigravity-priority", false},
		{nil, "antigravity-priority", true},
	} {
		cfg := &Config{NativeQuota: NativeQuotaConfig{Mode: "enforce", Providers: test.providers}, Plugins: PluginsConfig{Enabled: true, Configs: map[string]PluginInstanceConfig{test.plugin: {Enabled: &on}}}}
		if got := cfg.ValidateNativeQuota() != nil; got != test.conflict {
			t.Fatalf("providers=%#v plugin=%s conflict=%v, want %v", test.providers, test.plugin, got, test.conflict)
		}
	}
}

func TestNativeQuotaConfigConflictsAndRoundTrip(t *testing.T) {
	on := true
	for _, id := range []string{"antigravity-priority", "codex-quota-scheduler", "antigravity-quota"} {
		c := &Config{NativeQuota: NativeQuotaConfig{Mode: "enforce"}, Plugins: PluginsConfig{Enabled: true, Configs: map[string]PluginInstanceConfig{id: {Enabled: &on}}}}
		if err := c.ValidateNativeQuota(); err == nil {
			t.Errorf("accepted enabled %s", id)
		}
		c.NativeQuota.Mode = "shadow"
		if err := c.ValidateNativeQuota(); err != nil {
			t.Fatal(err)
		}
	}
	home := &Config{NativeQuota: NativeQuotaConfig{Mode: "enforce"}}
	home.Home.Enabled = true
	if err := home.ValidateNativeQuota(); err == nil {
		t.Fatal("Home enforce accepted")
	}
	c := &Config{NativeQuota: DefaultNativeQuotaConfig()}
	c.NativeQuota.Mode = "shadow"
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "native-quota:") {
		t.Fatal("quota config omitted from YAML")
	}
	var restored Config
	if err = yaml.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.NativeQuota.Mode != "shadow" || restored.NativeQuota.MaxWait != "2s" {
		t.Fatalf("quota roundtrip: %+v", restored.NativeQuota)
	}
}

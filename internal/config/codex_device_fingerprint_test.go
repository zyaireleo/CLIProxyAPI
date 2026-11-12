package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexDeviceFingerprintConfig(t *testing.T) {
	v8 := []byte("oauth: {providers: {codex: {device-fingerprint-mode: device, device-fingerprint-inject-missing: true}}}\n")
	if errValidate := ValidateV8Config(v8); errValidate != nil {
		t.Fatalf("validate v8 layout: %v", errValidate)
	}
	cfg, errParse := ParseConfigBytes(v8)
	if errParse != nil {
		t.Fatalf("parse v8 layout: %v", errParse)
	}
	if cfg.Codex.DeviceFingerprintMode != "device" || cfg.Codex.DeviceFingerprintInjectMissing == nil || !*cfg.Codex.DeviceFingerprintInjectMissing {
		t.Fatalf("v8 values not decoded: %+v", cfg.Codex)
	}
	for _, field := range []string{"codex.device-fingerprint-mode", "codex.device-fingerprint-inject-missing"} {
		if !cfg.OAuthOnlyFields[field] {
			t.Fatalf("%s not marked OAuth-only: %v", field, cfg.OAuthOnlyFields)
		}
	}
	api := cfg.ForAPIKey()
	if api.Codex.DeviceFingerprintMode != "" || api.Codex.DeviceFingerprintInjectMissing != nil {
		t.Fatalf("API-key scope kept device settings: %+v", api.Codex)
	}
	if cfg.Codex.DeviceFingerprintMode != "device" {
		t.Fatal("ForAPIKey mutated shared configuration")
	}
	snapshot, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatalf("snapshot: %v", errMarshal)
	}
	var restored Config
	if errUnmarshal := yaml.Unmarshal(snapshot, &restored); errUnmarshal != nil {
		t.Fatalf("restore snapshot: %v", errUnmarshal)
	}
	if restored.Codex.DeviceFingerprintMode != "device" || restored.ForAPIKey().Codex.DeviceFingerprintMode != "" {
		t.Fatalf("snapshot round trip lost OAuth scope: %s", snapshot)
	}

	omitted, errOmitted := ParseConfigBytes([]byte("config-version: 8\n"))
	if errOmitted != nil {
		t.Fatalf("parse omitted: %v", errOmitted)
	}
	if omitted.Codex.DeviceFingerprintMode != "" || omitted.Codex.DeviceFingerprintInjectMissing != nil {
		t.Fatalf("defaults not off: %+v", omitted.Codex)
	}

	legacy := []byte("codex:\n  device-fingerprint-mode: device\n")
	legacyCfg, errLegacy := ParseConfigBytes(legacy)
	if errLegacy != nil {
		t.Fatalf("parse legacy: %v", errLegacy)
	}
	if legacyCfg.Codex.DeviceFingerprintMode != "device" {
		t.Fatalf("legacy flat value not decoded: %+v", legacyCfg.Codex)
	}
	migrated, _, errNormalize := NormalizeConfigLayout(legacy, true)
	if errNormalize != nil {
		t.Fatalf("normalize: %v", errNormalize)
	}
	if !strings.Contains(string(migrated), "device-fingerprint-mode: device") {
		t.Fatalf("migration dropped the setting: %s", migrated)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("validate migrated: %v; data=%s", errValidate, migrated)
	}
	var migratedRoot yaml.Node
	if errUnmarshal := yaml.Unmarshal(migrated, &migratedRoot); errUnmarshal != nil {
		t.Fatalf("decode migrated: %v", errUnmarshal)
	}
	if yamlPath(migratedRoot.Content[0], "oauth.providers.codex.device-fingerprint-mode") == nil {
		t.Fatalf("migrated setting not under oauth.providers.codex: %s", migrated)
	}
	migratedCfg, errMigrated := ParseConfigBytes(migrated)
	if errMigrated != nil || migratedCfg.Codex.DeviceFingerprintMode != "device" {
		t.Fatalf("migrated value = %+v, err=%v", migratedCfg, errMigrated)
	}

	for _, raw := range []string{
		"upstream: {codex: {device-fingerprint-mode: device}}\n",
		"upstream: {codex: {device-fingerprint-inject-missing: true}}\n",
		"oauth: {providers: {codex: {device-fingerprint-inject-missing: maybe}}}\n",
	} {
		if errValidate := ValidateV8Config([]byte(raw)); errValidate == nil {
			t.Fatalf("v8 accepted %q", raw)
		}
	}
}

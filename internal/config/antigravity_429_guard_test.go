package config

import "testing"

func TestAntigravity429GuardV8ConfigOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		value := "false"
		if enabled {
			value = "true"
		}
		cfg, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  antigravity-429-guard: " + value + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Antigravity429Guard != enabled {
			t.Fatalf("routing opt in=%v want %v", cfg.Antigravity429Guard, enabled)
		}
	}
}

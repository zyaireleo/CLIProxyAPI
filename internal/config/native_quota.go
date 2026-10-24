package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"gopkg.in/yaml.v3"
)

// NativeQuotaConfig is the serializable, opt-in quota policy. RuntimeConfig
// returns an independent validated value; callers must not publish mutable slices.
type NativeQuotaConfig struct {
	Mode                string   `yaml:"mode" json:"mode"`
	Providers           []string `yaml:"providers" json:"providers"`
	RefreshConcurrency  int      `yaml:"refresh-concurrency" json:"refresh-concurrency"`
	AntigravityInterval string   `yaml:"antigravity-interval" json:"antigravity-interval"`
	CodexInterval       string   `yaml:"codex-interval" json:"codex-interval"`
	RefreshMinInterval  string   `yaml:"refresh-min-interval" json:"refresh-min-interval"`
	MaxWait             string   `yaml:"max-wait" json:"max-wait"`
	MaxInFlight         int      `yaml:"max-in-flight" json:"max-in-flight"`
	UnknownMaxInFlight  int      `yaml:"unknown-max-in-flight" json:"unknown-max-in-flight"`
	QueueLimit          int      `yaml:"queue-limit" json:"queue-limit"`
}

// MarshalYAML preserves the semantic distinction between default coverage (nil)
// and explicitly no coverage ([]). yaml.v3 otherwise encodes both as [].
func (c NativeQuotaConfig) MarshalYAML() (any, error) {
	type plain NativeQuotaConfig
	var node yaml.Node
	if err := node.Encode(plain(c)); err != nil {
		return nil, err
	}
	if c.Providers == nil {
		if index := findMapKeyIndex(&node, "providers"); index >= 0 {
			node.Content[index+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		}
	}
	return &node, nil
}

func DefaultNativeQuotaConfig() NativeQuotaConfig {
	c := nativequota.DefaultConfig()
	return NativeQuotaConfig{
		Mode: string(c.Mode), Providers: append([]string(nil), c.Providers...),
		RefreshConcurrency: c.RefreshConcurrency, AntigravityInterval: c.AntigravityInterval.String(),
		CodexInterval: c.CodexInterval.String(), RefreshMinInterval: c.RefreshMinInterval.String(),
		MaxWait: c.MaxWait.String(), MaxInFlight: c.MaxInFlight,
		UnknownMaxInFlight: c.UnknownMaxInFlight, QueueLimit: c.QueueLimit,
	}
}

func (c NativeQuotaConfig) RuntimeConfig() (nativequota.Config, error) {
	r := nativequota.DefaultConfig()
	if c.Mode != "" {
		r.Mode = nativequota.Mode(c.Mode)
	}
	if c.Providers != nil {
		r.Providers = append([]string{}, c.Providers...)
	}
	for _, field := range []struct {
		name   string
		value  int
		target *int
	}{
		{"refresh-concurrency", c.RefreshConcurrency, &r.RefreshConcurrency},
		{"max-in-flight", c.MaxInFlight, &r.MaxInFlight},
		{"unknown-max-in-flight", c.UnknownMaxInFlight, &r.UnknownMaxInFlight},
		{"queue-limit", c.QueueLimit, &r.QueueLimit},
	} {
		if field.value < 0 {
			return r, fmt.Errorf("native-quota.%s must not be negative", field.name)
		}
		if field.value != 0 {
			*field.target = field.value
		}
	}
	for _, field := range []struct {
		name, value string
		target      *time.Duration
	}{
		{"antigravity-interval", c.AntigravityInterval, &r.AntigravityInterval},
		{"codex-interval", c.CodexInterval, &r.CodexInterval},
		{"refresh-min-interval", c.RefreshMinInterval, &r.RefreshMinInterval},
		{"max-wait", c.MaxWait, &r.MaxWait},
	} {
		if field.value == "" {
			continue
		}
		d, err := time.ParseDuration(field.value)
		if err != nil || d <= 0 {
			return r, fmt.Errorf("native-quota.%s must be a valid positive duration", field.name)
		}
		*field.target = d
	}
	if r.MaxWait > 2*time.Second {
		return r, fmt.Errorf("native-quota.max-wait must not exceed 2s")
	}
	if err := r.Validate(); err != nil {
		return r, fmt.Errorf("native-quota: %w", err)
	}
	return r, nil
}

func (c NativeQuotaConfig) Validate() error {
	_, err := c.RuntimeConfig()
	return err
}

// ValidateNativeQuota rejects dual quota ownership before any config is saved or
// published. No implicit plugin shutdown or migration is performed.
func (cfg *Config) ValidateNativeQuota() error {
	if cfg == nil {
		return nil
	}
	r, err := cfg.NativeQuota.RuntimeConfig()
	if err != nil {
		return err
	}
	if r.Mode != nativequota.ModeEnforce {
		return nil
	}
	if cfg.Home.Enabled {
		return fmt.Errorf("native-quota enforce is not supported in Home mode")
	}
	if !cfg.Plugins.Enabled {
		return nil
	}
	for id, plugin := range cfg.Plugins.Configs {
		if plugin.Enabled == nil || !*plugin.Enabled {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(id))
		provider := strings.Contains(key, "antigravity") || strings.Contains(key, "codex") || strings.HasPrefix(key, "ag-")
		if key == "antigravity-priority" || (provider && (strings.Contains(key, "quota") || strings.Contains(key, "schedul"))) {
			return fmt.Errorf("native-quota enforce conflicts with enabled plugin %q; explicit handoff is required", id)
		}
	}
	return nil
}

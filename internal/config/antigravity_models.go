package config

import (
	"fmt"
	"strings"
	"time"
)

// AntigravityModelsConfig controls account-scoped catalogs independently of the static updater.
type AntigravityModelsConfig struct {
	DiscoveryEnabled bool                         `yaml:"discovery-enabled" json:"discovery-enabled"`
	RefreshInterval  string                       `yaml:"refresh-interval,omitempty" json:"refresh-interval,omitempty"`
	AdditionalModels []AntigravityAdditionalModel `yaml:"additional-models,omitempty" json:"additional-models,omitempty"`
}

type AntigravityAdditionalModel struct {
	ID          string   `yaml:"id" json:"id"`
	DisplayName string   `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	AuthIDs     []string `yaml:"auth-ids,omitempty" json:"auth-ids,omitempty"`
}

func (c AntigravityModelsConfig) Interval() time.Duration {
	if c.RefreshInterval == "" {
		return 3 * time.Hour
	}
	d, err := time.ParseDuration(c.RefreshInterval)
	if err != nil || d <= 0 {
		return 3 * time.Hour
	}
	return d
}

func (c AntigravityModelsConfig) Validate() error {
	if c.RefreshInterval != "" {
		d, err := time.ParseDuration(c.RefreshInterval)
		if err != nil || d < time.Minute {
			return fmt.Errorf("antigravity-models.refresh-interval must be at least 1m")
		}
	}
	for i, m := range c.AdditionalModels {
		if strings.TrimSpace(m.ID) == "" || strings.ContainsAny(m.ID, " \t\r\n") {
			return fmt.Errorf("antigravity-models.additional-models[%d]: id must be nonempty and contain no whitespace", i)
		}
		for _, id := range m.AuthIDs {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("antigravity-models.additional-models[%d]: auth-ids must not contain an empty ID", i)
			}
		}
	}
	return nil
}

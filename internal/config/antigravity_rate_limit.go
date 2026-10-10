package config

// AntigravityRateLimitModelRule overrides rate-limit seed/bounds for models
// matching Name (single "*" wildcard segments, same semantics as payload rules).
type AntigravityRateLimitModelRule struct {
	// Name is the model name pattern ("gemini-3-flash", "gemini-*-flash-image", "*").
	Name string `yaml:"name" json:"name"`

	// InitialRPM seeds the per-(auth, model) bucket rate in requests-per-minute.
	// Zero means inherit the global default.
	InitialRPM float64 `yaml:"initial-rpm" json:"initial-rpm"`

	// MinRPM is the adaptive rate floor for matching models. Zero inherits the global default.
	MinRPM float64 `yaml:"min-rpm" json:"min-rpm"`

	// MaxRPM is the adaptive rate ceiling for matching models. Zero inherits the global default.
	MaxRPM float64 `yaml:"max-rpm" json:"max-rpm"`

	// Burst is the token bucket capacity. Zero inherits the global default.
	Burst float64 `yaml:"burst" json:"burst"`
}

// AntigravityRateLimitConfig configures per-(auth, model) adaptive AIMD token-bucket
// pacing for the Antigravity executor. The executor consults it before each upstream
// request: when the bucket for (auth, model) has no token it returns a local 429 with
// Retry-After, reusing the existing cooldown/rotation machinery instead of hitting the
// upstream. Rates adapt multiplicatively: halved per upstream 429, slowly increased
// after sustained successes. All values are hot-reloadable; adaptive state is kept and
// only re-clamped against new bounds.
type AntigravityRateLimitConfig struct {
	// Enabled toggles pacing. Default false: zero behavior change.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// InitialRPMPerAuth seeds each bucket's rate in requests-per-minute per auth per model.
	InitialRPMPerAuth float64 `yaml:"initial-rpm-per-auth" json:"initial-rpm-per-auth"`

	// MinRPMPerAuth is the adaptive rate floor.
	MinRPMPerAuth float64 `yaml:"min-rpm-per-auth" json:"min-rpm-per-auth"`

	// MaxRPMPerAuth is the adaptive rate ceiling.
	MaxRPMPerAuth float64 `yaml:"max-rpm-per-auth" json:"max-rpm-per-auth"`

	// Burst is the bucket capacity (max tokens accumulated while idle).
	Burst float64 `yaml:"burst" json:"burst"`

	// DecreaseFactor is the multiplicative rate decrease on upstream 429. Default 0.5.
	DecreaseFactor float64 `yaml:"decrease-factor" json:"decrease-factor"`

	// IncreaseRatio is the multiplicative rate increase after SuccessesPerIncrease
	// consecutive successes. Default 1.15.
	IncreaseRatio float64 `yaml:"increase-ratio" json:"increase-ratio"`

	// SuccessesPerIncrease is the consecutive-success count required before an increase.
	SuccessesPerIncrease int `yaml:"successes-per-increase" json:"successes-per-increase"`

	// MinIncreaseInterval caps increase frequency ("30s", "1m"). Default 1m.
	MinIncreaseInterval string `yaml:"min-increase-interval" json:"min-increase-interval"`

	// IdleReset resets a bucket back to the initial rate after this idle duration ("15m").
	IdleReset string `yaml:"idle-reset" json:"idle-reset"`

	// Models holds per-model seed/bound overrides, first matching rule wins.
	Models []AntigravityRateLimitModelRule `yaml:"models" json:"models"`
}

// Default seeds for AntigravityRateLimitConfig zero values.
const (
	DefaultAntigravityRateInitialRPM = 10
	DefaultAntigravityRateMinRPM     = 1
	DefaultAntigravityRateMaxRPM     = 30
	DefaultAntigravityRateBurst      = 3
	DefaultAntigravityRateDecrease   = 0.5
	DefaultAntigravityRateIncrease   = 1.15
	// DefaultAntigravityRateSuccesses is the consecutive-success count per increase step.
	DefaultAntigravityRateSuccesses = 8
	// DefaultAntigravityRateMinIncreaseInterval bounds how often the rate may increase.
	DefaultAntigravityRateMinIncreaseInterval = "1m"
	// DefaultAntigravityRateIdleReset resets stale learned rates after sustained idleness.
	DefaultAntigravityRateIdleReset = "15m"
)

// Normalize fills zero/invalid values with defaults and clamps into valid ranges.
// Invalid values are silently corrected (never fail config load for pacing knobs).
func (c *AntigravityRateLimitConfig) Normalize() {
	if c.InitialRPMPerAuth <= 0 {
		c.InitialRPMPerAuth = DefaultAntigravityRateInitialRPM
	}
	if c.MinRPMPerAuth <= 0 {
		c.MinRPMPerAuth = DefaultAntigravityRateMinRPM
	}
	if c.MaxRPMPerAuth < c.MinRPMPerAuth {
		c.MaxRPMPerAuth = DefaultAntigravityRateMaxRPM
	}
	if c.MaxRPMPerAuth < c.MinRPMPerAuth {
		c.MinRPMPerAuth = c.MaxRPMPerAuth
	}
	if c.InitialRPMPerAuth < c.MinRPMPerAuth {
		c.InitialRPMPerAuth = c.MinRPMPerAuth
	}
	if c.InitialRPMPerAuth > c.MaxRPMPerAuth {
		c.InitialRPMPerAuth = c.MaxRPMPerAuth
	}
	if c.Burst < 1 {
		c.Burst = DefaultAntigravityRateBurst
	}
	if c.DecreaseFactor <= 0 || c.DecreaseFactor >= 1 {
		c.DecreaseFactor = DefaultAntigravityRateDecrease
	}
	if c.IncreaseRatio <= 1 {
		c.IncreaseRatio = DefaultAntigravityRateIncrease
	}
	if c.SuccessesPerIncrease < 1 {
		c.SuccessesPerIncrease = DefaultAntigravityRateSuccesses
	}
	if c.MinIncreaseInterval == "" {
		c.MinIncreaseInterval = DefaultAntigravityRateMinIncreaseInterval
	}
	if c.IdleReset == "" {
		c.IdleReset = DefaultAntigravityRateIdleReset
	}
	for i := range c.Models {
		rule := &c.Models[i]
		if rule.Name == "" {
			continue
		}
		if rule.MinRPM <= 0 {
			rule.MinRPM = c.MinRPMPerAuth
		}
		if rule.MaxRPM < rule.MinRPM {
			rule.MaxRPM = c.MaxRPMPerAuth
		}
		if rule.MaxRPM < rule.MinRPM {
			rule.MinRPM = rule.MaxRPM
		}
		if rule.InitialRPM == 0 {
			rule.InitialRPM = c.InitialRPMPerAuth
		}
		if rule.InitialRPM < rule.MinRPM {
			rule.InitialRPM = rule.MinRPM
		}
		if rule.InitialRPM > rule.MaxRPM {
			rule.InitialRPM = rule.MaxRPM
		}
		if rule.Burst < 1 {
			rule.Burst = c.Burst
		}
	}
}

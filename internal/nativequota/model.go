// Package nativequota provides credential-free quota evidence and local admission.
// It never performs HTTP requests or generation calls; the owner supplies Fetcher.
package nativequota

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

const (
	ProviderAntigravity = "antigravity"
	ProviderCodex       = "codex"
	SourceAntigravity   = "antigravity"
	SourceCodexUsage    = "codex_usage"
	SourceCodexHeaders  = "codex_headers"
	maxPools            = 64
	maxWindows          = 64
	maxModels           = 512
	maxPayload          = 4 << 20
	maxEvents           = 256
)

type Config struct {
	Mode                Mode          `json:"mode" yaml:"mode"`
	Providers           []string      `json:"providers" yaml:"providers"`
	RefreshConcurrency  int           `json:"refresh_concurrency" yaml:"refresh-concurrency"`
	AntigravityInterval time.Duration `json:"-" yaml:"-"`
	CodexInterval       time.Duration `json:"-" yaml:"-"`
	RefreshMinInterval  time.Duration `json:"-" yaml:"-"`
	MaxWait             time.Duration `json:"-" yaml:"-"`
	MaxInFlight         int           `json:"max_in_flight" yaml:"max-in-flight"`
	UnknownMaxInFlight  int           `json:"unknown_max_in_flight" yaml:"unknown-max-in-flight"`
	QueueLimit          int           `json:"queue_limit" yaml:"queue-limit"`
}

func DefaultConfig() Config {
	return Config{
		Mode: ModeOff, Providers: []string{ProviderAntigravity, ProviderCodex},
		RefreshConcurrency: 2, AntigravityInterval: 5 * time.Minute,
		CodexInterval: 15 * time.Minute, RefreshMinInterval: 30 * time.Second,
		MaxWait: 2 * time.Second, MaxInFlight: 4, UnknownMaxInFlight: 1, QueueLimit: 64,
	}
}

// Zero values select safe defaults. An explicitly empty Providers slice disables
// coverage; a nil slice selects the default providers. Negative values are errors.
func (c Config) normalized() Config {
	d := DefaultConfig()
	if c.Mode == "" {
		c.Mode = d.Mode
	}
	if c.Providers == nil {
		c.Providers = d.Providers
	} else {
		c.Providers = append([]string{}, c.Providers...)
	}
	if c.RefreshConcurrency == 0 {
		c.RefreshConcurrency = d.RefreshConcurrency
	}
	if c.AntigravityInterval == 0 {
		c.AntigravityInterval = d.AntigravityInterval
	}
	if c.CodexInterval == 0 {
		c.CodexInterval = d.CodexInterval
	}
	if c.RefreshMinInterval == 0 {
		c.RefreshMinInterval = d.RefreshMinInterval
	}
	if c.MaxWait == 0 {
		c.MaxWait = d.MaxWait
	}
	if c.MaxInFlight == 0 {
		c.MaxInFlight = d.MaxInFlight
	}
	if c.UnknownMaxInFlight == 0 {
		c.UnknownMaxInFlight = d.UnknownMaxInFlight
	}
	if c.QueueLimit == 0 {
		c.QueueLimit = d.QueueLimit
	}
	return c
}

func (c Config) Validate() error {
	c = c.normalized()
	if c.Mode != ModeOff && c.Mode != ModeShadow && c.Mode != ModeEnforce {
		return errors.New("native quota: invalid mode")
	}
	seen := make(map[string]bool)
	for _, p := range c.Providers {
		if !supportedProvider(p) || seen[p] {
			return errors.New("native quota: invalid or duplicate provider")
		}
		seen[p] = true
	}
	if c.RefreshConcurrency < 1 || c.RefreshConcurrency > 32 {
		return errors.New("native quota: refresh concurrency must be 1..32")
	}
	for _, d := range []time.Duration{c.AntigravityInterval, c.CodexInterval, c.RefreshMinInterval} {
		if d <= 0 || d > 7*24*time.Hour {
			return errors.New("native quota: refresh intervals must be positive and at most seven days")
		}
	}
	if c.MaxWait <= 0 || c.MaxWait > 2*time.Second {
		return errors.New("native quota: max wait must be positive and at most two seconds")
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 1024 || c.UnknownMaxInFlight < 1 || c.UnknownMaxInFlight > c.MaxInFlight {
		return errors.New("native quota: invalid in-flight limits")
	}
	if c.QueueLimit < 1 || c.QueueLimit > 10000 {
		return errors.New("native quota: queue limit must be 1..10000")
	}
	return nil
}

type Account struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Disabled bool   `json:"disabled"`
	Priority int    `json:"priority"`
	// Revision is an opaque stable-principal revision, never a token or secret.
	Revision string `json:"-"`
}

type Window struct {
	ID              string    `json:"id"`
	DurationSeconds int64     `json:"duration_seconds"`
	Remaining       *float64  `json:"remaining"`
	ResetAt         time.Time `json:"reset_at"`
	Blocked         bool      `json:"blocked"`
	// Disabled means this window is explicitly not applicable, not exhausted.
	Disabled   bool      `json:"disabled,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Source     string    `json:"source,omitempty"`
}

type Pool struct {
	ID      string   `json:"id"`
	Models  []string `json:"models"`
	Windows []Window `json:"windows"`
	Blocked bool     `json:"blocked"`
	// BlockedKnown distinguishes omitted passive metadata from explicit false.
	BlockedKnown bool      `json:"blocked_known,omitempty"`
	ObservedAt   time.Time `json:"observed_at,omitempty"`
	Source       string    `json:"source,omitempty"`
}

type Observation struct {
	AuthID     string    `json:"auth_id"`
	Provider   string    `json:"provider"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	Pools      []Pool    `json:"pools"`
	// Partial merges only present windows. A full observation replaces pools,
	// but absent quota evidence is never interpreted as a healthy balance.
	Partial bool `json:"partial,omitempty"`
}

type View struct {
	AuthID        string    `json:"auth_id"`
	Provider      string    `json:"provider"`
	Mode          Mode      `json:"mode"`
	State         string    `json:"state"`
	Reason        string    `json:"reason"`
	ObservedAt    time.Time `json:"observed_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Pools         []Pool    `json:"pools"`
	InFlight      int       `json:"in_flight"`
	AtCapacity    bool      `json:"at_capacity"`
	Stale         bool      `json:"stale"`
	RefreshFailed bool      `json:"refresh_failed"`
	Weight        float64   `json:"weight"`
	Allowed       bool      `json:"allowed"`
	Disabled      bool      `json:"disabled"`
}

type Fetcher func(context.Context, Account) (Observation, error)

// Event contains fixed classification strings, never upstream bodies, errors,
// headers, credentials, request models, or caller-supplied refresh reasons.
type Event struct {
	At         time.Time `json:"at"`
	AuthID     string    `json:"auth_id,omitempty"`
	ProposedID string    `json:"proposed_id,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Kind       string    `json:"kind"`
	Reason     string    `json:"reason"`
}

var (
	ErrNoQuota         = errors.New("native quota: no quota evidence")
	ErrInvalidQuota    = errors.New("native quota: invalid quota evidence")
	ErrAdmissionDenied = errors.New("native quota: local admission denied")
	ErrQueueFull       = errors.New("native quota: admission queue full")
	ErrWaitBudget      = errors.New("native quota: admission wait budget exhausted")
	ErrDisabled        = errors.New("native quota: account disabled")
	ErrModeOff         = errors.New("native quota: mode off")
)

// AdmissionError is a local refusal, not an upstream 429. Use errors.Is to
// classify it; no HTTP status is attached to avoid triggering upstream health.
type AdmissionError struct{ Reason string }

func (e *AdmissionError) Error() string {
	return fmt.Sprintf("native quota: local admission denied (%s)", e.Reason)
}
func (e *AdmissionError) Is(target error) bool {
	return target == ErrAdmissionDenied ||
		(target == ErrQueueFull && e.Reason == "queue_full") ||
		(target == ErrWaitBudget && e.Reason == "wait_budget") ||
		(target == ErrDisabled && e.Reason == "manual_disabled")
}

func supportedProvider(p string) bool { return p == ProviderAntigravity || p == ProviderCodex }
func (c Config) covers(p string) bool {
	for _, v := range c.Providers {
		if v == p {
			return true
		}
	}
	return false
}
func (c Config) interval(p string) time.Duration {
	if p == ProviderAntigravity {
		return c.AntigravityInterval
	}
	return c.CodexInterval
}
func validID(s string) bool {
	if s == "" || len(s) > 256 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func validModel(s string) bool {
	if !validID(s) {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:/", c)) {
			return false
		}
	}
	return true
}
func safeSource(s string) string {
	switch s {
	case SourceAntigravity, SourceCodexUsage, SourceCodexHeaders:
		return s
	default:
		return "external"
	}
}
func validFraction(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
func fraction(v float64) *float64  { return &v }
func clonePools(pools []Pool) []Pool {
	out := make([]Pool, len(pools))
	for i, p := range pools {
		out[i] = p
		out[i].Models = append([]string{}, p.Models...)
		out[i].Windows = append([]Window{}, p.Windows...)
		for j := range out[i].Windows {
			if p.Windows[j].Remaining != nil {
				out[i].Windows[j].Remaining = fraction(*p.Windows[j].Remaining)
			}
		}
	}
	return out
}
func canonicalAG(model string) string {
	switch {
	case strings.HasPrefix(model, "gemini-"):
		return "gemini"
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-"):
		return "claude_gpt"
	default:
		return ""
	}
}
func knownCodexModel(model string) bool {
	return strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "codex-") || model == "codex" || model == "o3" || model == "o4-mini" || strings.HasPrefix(model, "o3-")
}

// matchingPools never guesses membership when explicit model associations exist.
func matchingPools(pools []Pool, provider, model string) []int {
	var result []int
	for i, p := range pools {
		if model == "" {
			result = append(result, i)
			continue
		}
		match := false
		if len(p.Models) > 0 {
			for _, m := range p.Models {
				if m == model {
					match = true
					break
				}
			}
		} else if provider == ProviderAntigravity {
			match = canonicalAG(model) != "" && canonicalAG(model) == p.ID
		} else if provider == ProviderCodex {
			match = p.ID == "codex" && knownCodexModel(model)
		}
		if match {
			result = append(result, i)
		}
	}
	return result
}

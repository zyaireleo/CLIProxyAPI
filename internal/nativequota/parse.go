package nativequota

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type object map[string]json.RawMessage

func decodeObject(raw []byte) (object, error) {
	if len(raw) == 0 || len(raw) > maxPayload {
		return nil, ErrInvalidQuota
	}
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, ErrInvalidQuota
	}
	return obj, nil
}
func present(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
func field(o object, names ...string) json.RawMessage {
	for _, n := range names {
		if present(o[n]) {
			return o[n]
		}
	}
	return nil
}
func number(raw json.RawMessage) (*big.Rat, error) {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 || len(s) > 128 || strings.ContainsAny(s, "\"/xX_") || !json.Valid([]byte(s)) {
		return nil, ErrInvalidQuota
	}
	// Bound exponent before arbitrary-precision allocation, even for tiny JSON.
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil || exponent < -400 || exponent > 400 {
			return nil, ErrInvalidQuota
		}
	}
	n, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, ErrInvalidQuota
	}
	return n, nil
}
func parseFraction(raw json.RawMessage, usedPercent bool) (*float64, error) {
	if !present(raw) {
		return nil, nil
	}
	n, err := number(raw)
	if err != nil {
		return nil, err
	}
	max := big.NewRat(1, 1)
	if usedPercent {
		max = big.NewRat(100, 1)
	}
	if n.Sign() < 0 || n.Cmp(max) > 0 {
		return nil, ErrInvalidQuota
	}
	if usedPercent {
		n.Sub(max, n)
		n.Quo(n, max)
	}
	v, _ := n.Float64()
	// Even a subnormal positive balance must never become exhaustion.
	if v == 0 && n.Sign() > 0 {
		v = math.SmallestNonzeroFloat64
	}
	return fraction(v), nil
}
func integer(raw json.RawMessage) (int64, error) {
	n, err := number(raw)
	if err != nil || !n.IsInt() || !n.Num().IsInt64() {
		return 0, ErrInvalidQuota
	}
	return n.Num().Int64(), nil
}
func optionalBool(raw json.RawMessage) (*bool, error) {
	if !present(raw) {
		return nil, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, ErrInvalidQuota
	}
	return &b, nil
}
func text(raw json.RawMessage) (string, error) {
	if !present(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ErrInvalidQuota
	}
	return s, nil
}
func resetTime(raw json.RawMessage) (time.Time, error) {
	if !present(raw) {
		return time.Time{}, nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		s, err := text(raw)
		if err != nil {
			return time.Time{}, err
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, ErrInvalidQuota
		}
		return t.UTC(), nil
	}
	n, err := integer(raw)
	if err != nil || n < 0 || n > 253402300799 {
		return time.Time{}, ErrInvalidQuota
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return time.Unix(n, 0).UTC(), nil
}
func newObservation(id, provider, source string, at time.Time) (Observation, error) {
	if !validID(id) || at.IsZero() {
		return Observation{}, ErrInvalidQuota
	}
	return Observation{AuthID: id, Provider: provider, Source: source, ObservedAt: at.UTC()}, nil
}

// ParseAntigravity prioritizes the authoritative quota-summary groups/buckets.
// fetchAvailableModels is a partial fallback with unknown window duration. Any
// malformed quota invalidates the response. Missing remaining is never zero.
func ParseAntigravity(raw []byte, id string, observedAt time.Time) (Observation, error) {
	ob, err := newObservation(id, ProviderAntigravity, SourceAntigravity, observedAt)
	if err != nil {
		return Observation{}, err
	}
	root, err := decodeObject(raw)
	if err != nil {
		return Observation{}, err
	}
	if _, exists := root["groups"]; exists {
		return parseAGSummary(root, ob)
	}
	if _, exists := root["buckets"]; exists {
		return parseAGSummary(root, ob)
	}
	ob.Partial = true
	models, err := decodeObject(root["models"])
	if err != nil {
		return Observation{}, err
	}
	if len(models) == 0 {
		return Observation{}, ErrNoQuota
	}
	if len(models) > maxModels {
		return Observation{}, ErrInvalidQuota
	}
	groups := map[string]*Pool{}
	keys := make([]string, 0, len(models))
	for m := range models {
		keys = append(keys, m)
	}
	sort.Strings(keys)
	evidence := false
	for _, model := range keys {
		if !validModel(model) {
			return Observation{}, ErrInvalidQuota
		}
		group := canonicalAG(model)
		if group == "" {
			continue
		}
		entry, errEntry := decodeObject(models[model])
		if errEntry != nil {
			return Observation{}, errEntry
		}
		p := groups[group]
		if p == nil {
			p = &Pool{ID: group, ObservedAt: ob.ObservedAt, Source: ob.Source}
			groups[group] = p
		}
		p.Models = append(p.Models, model)
		w := Window{ID: model, ObservedAt: ob.ObservedAt, Source: ob.Source}
		if present(entry["quotaInfo"]) {
			quota, errQuota := decodeObject(entry["quotaInfo"])
			if errQuota != nil {
				return Observation{}, errQuota
			}
			w.Remaining, err = parseFraction(quota["remainingFraction"], false)
			if err != nil {
				return Observation{}, err
			}
			w.ResetAt, err = resetTime(quota["resetTime"])
			if err != nil {
				return Observation{}, err
			}
			evidence = evidence || w.Remaining != nil
		}
		p.Windows = append(p.Windows, w)
		if len(p.Windows) > maxWindows {
			return Observation{}, ErrInvalidQuota
		}
	}
	if !evidence {
		return Observation{}, ErrNoQuota
	}
	for _, group := range []string{"gemini", "claude_gpt"} {
		if p := groups[group]; p != nil {
			ob.Pools = append(ob.Pools, *p)
		}
	}
	return ob, nil
}

func windowID(name string, seconds int64) string {
	switch {
	case seconds == 7*24*3600:
		return "weekly"
	case seconds >= 28*24*3600 && seconds <= 31*24*3600:
		return "monthly"
	case seconds > 0 && seconds <= 24*3600:
		return "short"
	default:
		return name
	}
}
func parseCodexWindow(raw json.RawMessage, name string, at time.Time, source string) (Window, error) {
	w := Window{ID: name, ObservedAt: at, Source: source}
	o, err := decodeObject(raw)
	if err != nil {
		return w, err
	}
	w.Remaining, err = parseFraction(field(o, "used_percent", "usedPercent"), true)
	if err != nil {
		return w, err
	}
	if remaining := field(o, "remaining_fraction", "remainingFraction"); present(remaining) {
		v, errRemaining := parseFraction(remaining, false)
		if errRemaining != nil || w.Remaining != nil {
			return w, ErrInvalidQuota
		}
		w.Remaining = v
	}
	seconds := field(o, "limit_window_seconds", "window_seconds")
	minutes := field(o, "window_minutes", "windowMinutes")
	if present(seconds) {
		w.DurationSeconds, err = integer(seconds)
	} else if present(minutes) {
		var n int64
		n, err = integer(minutes)
		if n <= 0 || n > 366*24*60 {
			return w, ErrInvalidQuota
		}
		w.DurationSeconds = n * 60
	}
	if err != nil || w.DurationSeconds < 0 || w.DurationSeconds > 366*24*3600 {
		return w, ErrInvalidQuota
	}
	w.ID = windowID(name, w.DurationSeconds)
	w.ResetAt, err = resetTime(field(o, "reset_at", "resetAt"))
	if err != nil {
		return w, err
	}
	if after := field(o, "reset_after_seconds", "resetAfterSeconds"); present(after) {
		n, errAfter := integer(after)
		if errAfter != nil || n < 0 || n > 366*24*3600 {
			return w, ErrInvalidQuota
		}
		if w.ResetAt.IsZero() {
			w.ResetAt = at.Add(time.Duration(n) * time.Second)
		}
	}
	blocked, err := optionalBool(field(o, "limit_reached", "limitReached"))
	if err != nil {
		return w, err
	}
	allowed, err := optionalBool(o["allowed"])
	if err != nil {
		return w, err
	}
	w.Blocked = blocked != nil && *blocked || allowed != nil && !*allowed
	return w, nil
}
func parseCodexPool(raw json.RawMessage, id string, at time.Time, source string) (Pool, bool, error) {
	p := Pool{ID: id, ObservedAt: at, Source: source}
	o, err := decodeObject(raw)
	if err != nil {
		return p, false, err
	}
	allowed, err := optionalBool(o["allowed"])
	if err != nil {
		return p, false, err
	}
	reached, err := optionalBool(field(o, "limit_reached", "limitReached"))
	if err != nil {
		return p, false, err
	}
	p.Blocked = allowed != nil && !*allowed || reached != nil && *reached
	p.BlockedKnown = allowed != nil || reached != nil
	evidence := p.Blocked
	seen := map[string]bool{}
	for _, name := range []string{"primary", "secondary", "weekly", "monthly", "short"} {
		window := field(o, name+"_window", name)
		if !present(window) {
			continue
		}
		w, errWindow := parseCodexWindow(window, name, at, source)
		if errWindow != nil || seen[w.ID] {
			return p, false, ErrInvalidQuota
		}
		seen[w.ID] = true
		p.Windows = append(p.Windows, w)
		evidence = evidence || w.Remaining != nil || w.Blocked
	}
	return p, evidence, nil
}

func modelAssociations(o object, fallback string) ([]string, error) {
	var models []string
	if raw := field(o, "models", "model_names"); present(raw) {
		if err := json.Unmarshal(raw, &models); err != nil {
			return nil, ErrInvalidQuota
		}
	} else if raw := field(o, "model", "model_name"); present(raw) {
		m, err := text(raw)
		if err != nil {
			return nil, err
		}
		models = []string{m}
	} else if m := strings.ToLower(fallback); validModel(m) && knownCodexModel(m) {
		// Only model-shaped names imply exact membership; opaque namespaces such
		// as bengalfox are deliberately not guessed from a private mapping.
		models = []string{m}
	}
	if len(models) > maxModels {
		return nil, ErrInvalidQuota
	}
	seen := map[string]bool{}
	for _, m := range models {
		if !validModel(m) || seen[m] {
			return nil, ErrInvalidQuota
		}
		seen[m] = true
	}
	sort.Strings(models)
	return models, nil
}

// ParseCodexUsage handles short, weekly and monthly windows and additional exact
// model limits. Credits and code-review limits never authorize generation.
func ParseCodexUsage(raw []byte, id string, observedAt time.Time) (Observation, error) {
	ob, err := newObservation(id, ProviderCodex, SourceCodexUsage, observedAt)
	if err != nil {
		return Observation{}, err
	}
	root, err := decodeObject(raw)
	if err != nil {
		return Observation{}, err
	}
	evidence := false
	if rawLimit := field(root, "rate_limit", "rate_limits", "rateLimit"); present(rawLimit) {
		p, has, errPool := parseCodexPool(rawLimit, "codex", ob.ObservedAt, ob.Source)
		if errPool != nil {
			return Observation{}, errPool
		}
		ob.Pools = append(ob.Pools, p)
		evidence = evidence || has
	}
	additional := field(root, "additional_rate_limits", "additionalRateLimits")
	if present(additional) {
		type limit struct {
			name string
			raw  json.RawMessage
		}
		var limits []limit
		if bytes.HasPrefix(bytes.TrimSpace(additional), []byte("[")) {
			var list []json.RawMessage
			if errList := json.Unmarshal(additional, &list); errList != nil {
				return Observation{}, ErrInvalidQuota
			}
			for _, item := range list {
				o, errItem := decodeObject(item)
				if errItem != nil {
					return Observation{}, errItem
				}
				name, errName := text(field(o, "limit_name", "limitName", "name"))
				if errName != nil {
					return Observation{}, errName
				}
				limits = append(limits, limit{name, item})
			}
		} else {
			o, errAdditional := decodeObject(additional)
			if errAdditional != nil {
				return Observation{}, errAdditional
			}
			for name, item := range o {
				limits = append(limits, limit{name, item})
			}
		}
		if len(limits) >= maxPools {
			return Observation{}, ErrInvalidQuota
		}
		sort.Slice(limits, func(i, j int) bool { return limits[i].name < limits[j].name })
		seen := map[string]bool{}
		for _, l := range limits {
			if !validModel(l.name) {
				return Observation{}, ErrInvalidQuota
			}
			o, errItem := decodeObject(l.raw)
			if errItem != nil {
				return Observation{}, errItem
			}
			rate := field(o, "rate_limit", "rateLimit")
			if !present(rate) {
				rate = l.raw
			}
			poolID := "codex:" + strings.ToLower(l.name)
			if seen[poolID] {
				return Observation{}, ErrInvalidQuota
			}
			seen[poolID] = true
			p, has, errPool := parseCodexPool(rate, poolID, ob.ObservedAt, ob.Source)
			if errPool != nil {
				return Observation{}, errPool
			}
			p.Models, err = modelAssociations(o, l.name)
			if err != nil {
				return Observation{}, err
			}
			ob.Pools = append(ob.Pools, p)
			evidence = evidence || has
		}
	}
	if !evidence {
		return Observation{}, ErrNoQuota
	}
	return ob, nil
}

// ParseCodexHeaders only consumes allowlisted quota headers. Passive observations
// are partial: omitted weekly/monthly/model limits retain their original age.
func ParseCodexHeaders(headers http.Header, id string, observedAt time.Time) (Observation, error) {
	ob, err := newObservation(id, ProviderCodex, SourceCodexHeaders, observedAt)
	if err != nil {
		return Observation{}, err
	}
	ob.Partial = true
	h := make(map[string]string)
	if len(headers) > 4096 {
		return Observation{}, ErrInvalidQuota
	}
	for key, values := range headers {
		key = strings.ToLower(key)
		if !strings.HasPrefix(key, "x-codex-") {
			continue
		}
		if len(key) > 512 || len(values) != 1 || len(values[0]) > 256 {
			return Observation{}, ErrInvalidQuota
		}
		if _, exists := h[key]; exists {
			return Observation{}, ErrInvalidQuota
		}
		h[key] = strings.TrimSpace(values[0])
	}
	prefixes := map[string]bool{}
	for key := range h {
		for _, suffix := range []string{"primary-used-percent", "secondary-used-percent", "primary-window-minutes", "secondary-window-minutes", "primary-reset-at", "secondary-reset-at", "primary-reset-after-seconds", "secondary-reset-after-seconds", "allowed", "limit-reached"} {
			if strings.HasSuffix(key, suffix) {
				prefixes[strings.TrimSuffix(key, suffix)] = true
			}
		}
	}
	if len(prefixes) > maxPools {
		return Observation{}, ErrInvalidQuota
	}
	keys := make([]string, 0, len(prefixes))
	for p := range prefixes {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	evidence := false
	for _, prefix := range keys {
		if prefix == "x-codex-code-review-" || !strings.HasSuffix(prefix, "-") {
			continue
		}
		poolID := "codex"
		name := ""
		if prefix != "x-codex-" {
			name = strings.TrimSuffix(strings.TrimPrefix(prefix, "x-codex-"), "-")
			name = strings.TrimPrefix(name, "additional-")
			if !validModel(name) {
				return Observation{}, ErrInvalidQuota
			}
			poolID += ":" + name
		}
		o := object{}
		for _, key := range []string{"allowed", "limit-reached"} {
			if v, ok := h[prefix+key]; ok {
				o[strings.ReplaceAll(key, "-", "_")] = json.RawMessage(v)
			}
		}
		for _, name := range []string{"primary", "secondary"} {
			w := object{}
			for _, key := range []string{"used-percent", "window-minutes", "reset-at", "reset-after-seconds"} {
				if v, ok := h[prefix+name+"-"+key]; ok {
					w[strings.ReplaceAll(key, "-", "_")] = json.RawMessage(v)
				}
			}
			if len(w) > 0 {
				b, errMarshal := json.Marshal(w)
				if errMarshal != nil {
					return Observation{}, ErrInvalidQuota
				}
				o[name] = b
			}
		}
		b, errMarshal := json.Marshal(o)
		if errMarshal != nil {
			return Observation{}, ErrInvalidQuota
		}
		p, has, errPool := parseCodexPool(b, poolID, ob.ObservedAt, ob.Source)
		if errPool != nil {
			return Observation{}, errPool
		}
		if name != "" {
			if label := h[prefix+"limit-name"]; label != "" {
				name = label
			}
			p.Models, err = modelAssociations(nil, name)
			if err != nil {
				return Observation{}, err
			}
			// A model-shaped label provides a common identity for HTTP and WS.
			if len(p.Models) == 1 {
				p.ID = "codex:" + p.Models[0]
			}
		}
		if has {
			evidence = true
			ob.Pools = append(ob.Pools, p)
		}
	}
	if !evidence {
		return Observation{}, ErrNoQuota
	}
	return ob, nil
}

// ParseRetryAfter is a convenience for the owner; it performs no network I/O.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && n >= 0 && n <= 86400 {
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return min(at.Sub(now), 24*time.Hour)
	}
	return 0
}

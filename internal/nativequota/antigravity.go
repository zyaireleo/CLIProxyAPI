package nativequota

import (
	"encoding/json"
	"strings"
)

func agGroupName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gemini", "gemini models":
		return "gemini"
	case "claude_gpt", "claudegpt", "claude and gpt models", "claude and gpt", "claude/gpt", "claude gpt models":
		return "claude_gpt"
	default:
		return ""
	}
}
func agBucketGroup(id string) string {
	switch {
	case strings.HasPrefix(id, "gemini-"):
		return "gemini"
	case strings.HasPrefix(id, "3p-"), strings.HasPrefix(id, "claude-"):
		return "claude_gpt"
	default:
		return ""
	}
}
func agDuration(window string) int64 {
	switch window {
	case "weekly":
		return 7 * 24 * 3600
	case "5h":
		return 5 * 3600
	case "daily", "24h":
		return 24 * 3600
	default:
		return 0
	}
}

// Descriptions and display labels are not stored. Only canonical group names,
// explicit model associations, numeric windows, and resets survive parsing.
func parseAGSummary(root object, ob Observation) (Observation, error) {
	groups := map[string]*Pool{}
	seenBuckets := map[string]bool{}
	evidence := false
	addBuckets := func(raw json.RawMessage, group string, models []string) error {
		var buckets []json.RawMessage
		if !present(raw) || json.Unmarshal(raw, &buckets) != nil || len(buckets) == 0 || len(buckets) > maxWindows {
			return ErrInvalidQuota
		}
		for _, rawBucket := range buckets {
			bucket, err := decodeObject(rawBucket)
			if err != nil {
				return err
			}
			id, err := text(field(bucket, "bucketId", "bucket_id", "id"))
			if err != nil || !validModel(id) || seenBuckets[id] {
				return ErrInvalidQuota
			}
			seenBuckets[id] = true
			canonical := group
			inferred := agBucketGroup(id)
			if canonical == "" {
				canonical = inferred
			}
			if canonical == "" {
				continue
			}
			if inferred != "" && inferred != canonical {
				return ErrInvalidQuota
			}
			p := groups[canonical]
			if p == nil {
				p = &Pool{ID: canonical, Models: models, ObservedAt: ob.ObservedAt, Source: ob.Source}
				groups[canonical] = p
			}
			w := Window{ID: id, ObservedAt: ob.ObservedAt, Source: ob.Source}
			w.Remaining, err = parseFraction(field(bucket, "remainingFraction", "remaining_fraction"), false)
			if err != nil {
				return err
			}
			w.ResetAt, err = resetTime(field(bucket, "resetTime", "reset_at"))
			if err != nil {
				return err
			}
			window, err := text(bucket["window"])
			if err != nil {
				return err
			}
			w.DurationSeconds = agDuration(window)
			if rawDuration := field(bucket, "durationSeconds", "duration_seconds"); present(rawDuration) {
				n, errDuration := integer(rawDuration)
				if errDuration != nil || n <= 0 || n > 366*24*3600 || w.DurationSeconds > 0 && w.DurationSeconds != n {
					return ErrInvalidQuota
				}
				w.DurationSeconds = n
			}
			disabled, err := optionalBool(bucket["disabled"])
			if err != nil {
				return err
			}
			w.Disabled = disabled != nil && *disabled
			blocked, err := optionalBool(bucket["blocked"])
			if err != nil {
				return err
			}
			w.Blocked = blocked != nil && *blocked
			p.Windows = append(p.Windows, w)
			if len(p.Windows) > maxWindows {
				return ErrInvalidQuota
			}
			evidence = evidence || !w.Disabled && (w.Remaining != nil || w.Blocked)
		}
		return nil
	}
	if raw, exists := root["groups"]; exists {
		var list []json.RawMessage
		if !present(raw) || json.Unmarshal(raw, &list) != nil {
			return Observation{}, ErrInvalidQuota
		}
		if len(list) == 0 {
			return Observation{}, ErrNoQuota
		}
		if len(list) > maxPools {
			return Observation{}, ErrInvalidQuota
		}
		seenGroups := map[string]bool{}
		for _, rawGroup := range list {
			group, err := decodeObject(rawGroup)
			if err != nil {
				return Observation{}, err
			}
			name, err := text(field(group, "groupId", "id", "displayName", "name"))
			if err != nil {
				return Observation{}, err
			}
			canonical := agGroupName(name)
			if canonical != "" && seenGroups[canonical] {
				return Observation{}, ErrInvalidQuota
			}
			seenGroups[canonical] = true
			var models []string
			if rawModels := group["models"]; present(rawModels) {
				if json.Unmarshal(rawModels, &models) != nil || len(models) > maxModels {
					return Observation{}, ErrInvalidQuota
				}
				seen := map[string]bool{}
				for _, m := range models {
					if !validModel(m) || seen[m] || canonical != "" && canonicalAG(m) != canonical {
						return Observation{}, ErrInvalidQuota
					}
					seen[m] = true
				}
			}
			if errBuckets := addBuckets(group["buckets"], canonical, models); errBuckets != nil {
				return Observation{}, errBuckets
			}
		}
	} else if err := addBuckets(root["buckets"], "", nil); err != nil {
		return Observation{}, err
	}
	if !evidence {
		return Observation{}, ErrNoQuota
	}
	for _, name := range []string{"gemini", "claude_gpt"} {
		if p := groups[name]; p != nil {
			ob.Pools = append(ob.Pools, *p)
		}
	}
	return ob, nil
}

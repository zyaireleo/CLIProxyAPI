package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

func TestLogFormatterPrintsSub2APITraceID(t *testing.T) {
	entry := log.NewEntry(log.New()).WithField("sub2api_trace_id", "sub2api:request-123")
	entry.Message = "request completed"
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "sub2api_trace_id=sub2api:request-123") {
		t.Fatalf("formatted log missing correlation field: %s", formatted)
	}
}

func TestLogFormatterPreservesImageAuditFields(t *testing.T) {
	for _, message := range []string{"antigravity image generation outcome", "antigravity image generation failed"} {
		t.Run(message, func(t *testing.T) {
			entry := log.NewEntry(log.New()).WithFields(log.Fields{
				"request_id":       "cpa-request-123",
				"sub2api_trace_id": "sub2api:client:request-123",
				"model":            "gemini-3.1-flash-image",
				"auth_ref":         "0a1b2c3d4e5f",
				"attempt":          4,
				"outcome":          "upstream_quota_exhausted",
				"images":           0,
				"finish_reason":    "STOP",
				"block_reason":     "BLOCK_REASON_UNSPECIFIED",
				"status":           429,
				"duration_ms":      int64(1234),
				"cooldown_until":   "2026-10-09T09:00:00Z",
				"auth_id":          "private-account-identity",
				"access_token":     "private-access-token",
				"prompt":           "private-buyer-prompt",
				"reference_image":  "private-reference-image",
				"raw_response":     "private-upstream-response",
			})
			entry.Message = message
			formatted, errFormat := (&LogFormatter{}).Format(entry)
			if errFormat != nil {
				t.Fatal(errFormat)
			}
			line := string(formatted)
			for _, want := range []string{
				"[cpa-request-123]", "model=gemini-3.1-flash-image",
				"sub2api_trace_id=sub2api:client:request-123", "auth_ref=0a1b2c3d4e5f",
				"attempt=4", "outcome=upstream_quota_exhausted", "images=0",
				"finish_reason=STOP", "block_reason=BLOCK_REASON_UNSPECIFIED",
				"status=429", "duration_ms=1234", "cooldown_until=2026-10-09T09:00:00Z",
			} {
				if !strings.Contains(line, want) {
					t.Fatalf("image audit log %q is missing %s", line, want)
				}
			}
			if strings.Contains(line, "private-") {
				t.Fatalf("image audit log exposed an unapproved private field: %q", line)
			}
			if strings.Count(line, "\n") != 1 {
				t.Fatalf("image audit log must occupy one line: %q", line)
			}
		})
	}
}

func TestLogFormatterPrintsMediaForwardingFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 7, 25, 7, 36, 4, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "codex live remote media forwarding started"
	entry.Data["credential"] = "Voice credential\nsecondary"
	entry.Data["connection"] = "via socks5 proxy"
	entry.Data["proxy_scheme"] = "socks5"
	entry.Data["remote_transport"] = "tcp"
	entry.Data["media_session_id"] = "media-session-id"
	entry.Data["call_id"] = "call-id"
	entry.Data["peer"] = "remote"
	entry.Data["state"] = "connected"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		`credential="Voice credential\nsecondary"`,
		`connection="via socks5 proxy"`,
		`proxy_scheme="socks5"`,
		`remote_transport="tcp"`,
		`media_session_id="media-session-id"`,
		`call_id="call-id"`,
		`peer="remote"`,
		`state="connected"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %s", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("formatted line contains an unescaped newline: %q", line)
	}
}

func TestLogFormatterPrintsPluginFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 25, 20, 10, 0, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "pluginhost: plugin loaded"
	entry.Data["plugin_id"] = "sample-provider"
	entry.Data["plugin_name"] = "Sample Provider"
	entry.Data["version"] = "0.2.0"
	entry.Data["active_version"] = "0.1.0"
	entry.Data["retired_version"] = "0.2.0"
	entry.Data["path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"
	entry.Data["active_path"] = "plugins/windows/amd64/sample-provider-v0.1.0.dll"
	entry.Data["retired_path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"plugin_id=sample-provider",
		"plugin_name=Sample Provider",
		"version=0.2.0",
		"active_version=0.1.0",
		"retired_version=0.2.0",
		"path=plugins/windows/amd64/sample-provider-v0.2.0.dll",
		"active_path=plugins/windows/amd64/sample-provider-v0.1.0.dll",
		"retired_path=plugins/windows/amd64/sample-provider-v0.2.0.dll",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %s", line, want)
		}
	}
}

func TestLogFormatterOmitsGenericPathField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 25, 20, 20, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "failed to roll back token"
	entry.Data["path"] = "auths/private-token.json"
	entry.Data["active_path"] = "plugins/windows/amd64/sample-provider-v0.1.0.dll"
	entry.Data["retired_path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, forbidden := range []string{"path=", "active_path=", "retired_path="} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("formatted line %q contains generic %s field", line, forbidden)
		}
	}
}

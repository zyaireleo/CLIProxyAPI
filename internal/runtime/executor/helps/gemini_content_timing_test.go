package helps

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestGeminiFirstContentTimingExcludesUsageOnlyFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"usageMetadata\":{\"promptTokenCount\":1}}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(35 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n"))
	}))
	defer server.Close()
	reporter := &UsageReporter{requestedAt: time.Now()}
	client := reporter.TrackHTTPClientRoundTripOnly(server.Client())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	frames := 0
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		payload := JSONPayload(scanner.Bytes())
		if payload == nil {
			continue
		}
		ObserveGeminiContent(reporter, payload)
		frames++
		if frames == 1 && (!reporter.IsFirstPacketSet() || reporter.IsTTFTSet()) {
			t.Fatal("metadata was counted as generated content")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if frames != 2 || !reporter.IsTTFTSet() || reporter.ttftDuration() < 30*time.Millisecond {
		t.Fatal("first answer timing did not reflect delayed effective content")
	}
}

// Final metadata is observed even when it carries no additional generated text.
func TestGeminiTerminationMetadataDoesNotSetContentTimeOrExposeUnknownReason(t *testing.T) {
	reporter := &UsageReporter{}
	ObserveGeminiContent(reporter, []byte(`{"candidates":[{"finishReason":"STOP"}]}`))
	if reporter.geminiTermination != "STOP" || reporter.IsTTFTSet() {
		t.Fatalf("terminal metadata = %q; TTFT set = %v", reporter.geminiTermination, reporter.IsTTFTSet())
	}
	ObserveGeminiContent(reporter, []byte(`{"candidates":[{"finishReason":"user supplied arbitrary payload"}]}`))
	if reporter.geminiTermination != "UNKNOWN" {
		t.Fatal("unknown termination reason must not be logged verbatim")
	}
}

func TestGeminiNativeOutcomesDistinguishPolicyAndOutputLimitFromSuccess(t *testing.T) {
	originalHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(originalHooks) })
	for _, tc := range []struct {
		name, payload, outcome string
	}{
		{"policy", `{"promptFeedback":{"blockReason":"SAFETY"}}`, "content_policy_block"},
		{"thought only limit", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"thought":true,"text":"private thinking"}]}}]}`, "upstream_output_limit"},
		{"partial answer limit", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"partial answer"}]}}]}`, "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			reporter := &UsageReporter{requestedAt: time.Now()}
			ObserveGeminiContent(reporter, []byte(tc.payload))
			LogGeminiOutcome(context.Background(), "gemini-fixture", nil, reporter)
			entry := hook.LastEntry()
			if entry == nil || entry.Message != "gemini_generation_outcome" || entry.Data["outcome"] != tc.outcome {
				t.Fatalf("native outcome entry = %v, want %s", entry, tc.outcome)
			}
		})
	}
}

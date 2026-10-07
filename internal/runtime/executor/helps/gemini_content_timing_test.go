package helps

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

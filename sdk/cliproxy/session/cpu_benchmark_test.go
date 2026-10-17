package session

import (
	"net/http"
	"strings"
	"testing"
)

func BenchmarkExtractLargeGeminiSession(b *testing.B) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + strings.Repeat("A", 4<<20) + `"}}]}],"session_id":"sample","metadata":{"parent_session_id":"parent"}}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		info, ok := ExtractSessionInfo(http.Header{}, payload, nil)
		if !ok || info.SessionID == "" {
			b.Fatal(info)
		}
	}
}

package session

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	executor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRequestAnalysisReuseAndInvalidation(t *testing.T) {
	ctx := WithAnalysisCache(context.Background())
	payload := []byte(`{"contents":[{"parts":[{"text":"hello"}]}],"metadata":{"user_id":"user","parent_session_id":"parent"},"session_id":"first"}`)
	a := analysisFor(ctx, payload)
	for i := 0; i < 3; i++ {
		if !a.hasExplicitSession(nil) {
			t.Fatal("missing explicit identity")
		}
		a.claudeMetadataIdentities()
		a.extractInfo(nil, nil)
		if got := analysisFor(ctx, bytes.Clone(payload)); got != a {
			t.Fatal("cloned unchanged body rebuilt index")
		}
	}
	if a.rootIndexBuilds != 1 {
		t.Fatalf("indexes=%d", a.rootIndexBuilds)
	}
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"changed"}}
	got, _ := ExtractSessionInfoContext(ctx, headers, payload, nil)
	want, _ := ExtractSessionInfo(headers, payload, nil)
	if got.SessionID != want.SessionID {
		t.Fatal("stale header identity")
	}
	for _, scope := range []string{"a", "b"} {
		info, _ := ExtractSessionInfoContext(ctx, nil, payload, map[string]any{executor.CallerScopeMetadataKey: scope})
		if info.CallerScope != scope {
			t.Fatal("stale caller scope")
		}
	}
	next := bytes.Replace(payload, []byte("first"), []byte("other"), 1)
	if analysisFor(ctx, next) == a {
		t.Fatal("changed body reused index")
	}
	InvalidateAnalysis(ctx)
	if analysisFor(ctx, payload) == a {
		t.Fatal("explicit invalidation failed")
	}
	other := WithAnalysisCache(context.Background())
	if analysisFor(other, payload) == analysisFor(ctx, payload) {
		t.Fatal("cross-request cache")
	}
}

func TestRequestAnalysisNegativeAndNested(t *testing.T) {
	for _, body := range []string{`{"contents":[],"metadata":{}}`, `{"request":{"session_id":"nested","metadata":{"user_id":"{\"session_id\":\"claude\"}"}}}`, `{"session_id":"first","session_id":"second"}`} {
		a := newRequestAnalysis([]byte(body))
		for i := 0; i < 3; i++ {
			a.hasExplicitSession(nil)
			a.extractInfo(nil, nil)
			a.claudeMetadataIdentities()
		}
		if a.rootIndexBuilds != 1 {
			t.Fatalf("indexes=%d", a.rootIndexBuilds)
		}
	}
}

func BenchmarkSessionAnalysisPipeline(b *testing.B) {
	for _, tc := range []struct {
		name    string
		size    int
		session bool
	}{{"small", 128, true}, {"image4MiB", 4 << 20, true}, {"long_missing", 4 << 20, false}} {
		body := `{"contents":[{"parts":[{"inlineData":{"data":"` + strings.Repeat("A", tc.size) + `"}}]}]`
		if tc.session {
			body += `,"session_id":"sample"`
		}
		body += `}`
		payload := []byte(body)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			total := 0
			for i := 0; i < b.N; i++ {
				ctx := WithAnalysisCache(context.Background())
				a := analysisFor(ctx, payload)
				a.extractInfo(nil, nil)
				a.hasExplicitSession(nil)
				a.claudeMetadataIdentities()
				a.extractInfo(nil, nil)
				total += a.rootIndexBuilds
			}
			b.ReportMetric(float64(total)/float64(b.N), "indexes/op")
		})
	}
}

func TestRequestAnalysisConcurrentRetryAndPluginRewrite(t *testing.T) {
	ctx := WithAnalysisCache(context.Background())
	body := []byte(`{"session_id":"first","session_id":"second"}`)
	a := analysisFor(ctx, body)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, ok := ExtractSessionInfoContext(ctx, nil, body, nil)
			if !ok || info.SessionID != "session:first" {
				t.Errorf("unexpected identity: %+v", info)
			}
			analysisFor(ctx, body).claudeMetadataIdentities()
		}()
	}
	wg.Wait()
	if a.rootIndexBuilds != 1 {
		t.Fatal("concurrent retries rebuilt index")
	}
	ReconcileAnalysis(ctx, bytes.Clone(body))
	if analysisFor(ctx, body) != a {
		t.Fatal("unchanged plugin body invalidated index")
	}
	changed := []byte(`{"session_id":"third"}`)
	ReconcileAnalysis(ctx, changed)
	if analysisFor(ctx, changed) == a {
		t.Fatal("plugin replacement retained stale index")
	}
	info, _ := ExtractSessionInfoContext(ctx, nil, changed, nil)
	if info.SessionID != "session:third" {
		t.Fatalf("stale plugin identity: %+v", info)
	}
}

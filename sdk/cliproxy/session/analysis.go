package session

import (
	"bytes"
	"context"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Request JSON is immutable while analyzed. Only body-derived facts are cached;
// headers and metadata are evaluated afresh, including negative identity results.
type requestAnalysis struct {
	payload                                  []byte
	rootIndexBuilds                          int
	once                                     sync.Once
	root, request                            indexedSessionObject
	nested                                   bool
	claudeOnce                               sync.Once
	claudeSession, claudeParent, claudeAgent string
}

func newRequestAnalysis(payload []byte) *requestAnalysis { return &requestAnalysis{payload: payload} }
func (a *requestAnalysis) objects() (indexedSessionObject, indexedSessionObject, bool) {
	a.once.Do(func() {
		a.rootIndexBuilds++
		a.root = indexSessionObject(util.ParseGJSONBytesNoCopy(a.payload))
		a.request = a.root
		req := a.root.Get("request")
		a.nested = req.Exists() && !a.root.Get("contents").Exists()
		if a.nested {
			a.request = indexSessionObject(req)
		}
	})
	return a.root, a.request, a.nested
}
func (a *requestAnalysis) claudeMetadataIdentities() (string, string, string) {
	a.claudeOnce.Do(func() { a.claudeSession, a.claudeParent, a.claudeAgent = a.extractClaudeMetadataIdentities() })
	return a.claudeSession, a.claudeParent, a.claudeAgent
}

type analysisContextKey struct{}
type analysisCache struct {
	mu      sync.Mutex
	current *requestAnalysis
}

// WithAnalysisCache installs a bounded cache scoped to one downstream request.
func WithAnalysisCache(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(analysisContextKey{}).(*analysisCache); ok {
		return ctx
	}
	return context.WithValue(ctx, analysisContextKey{}, &analysisCache{})
}
func analysisFor(ctx context.Context, payload []byte) *requestAnalysis {
	if ctx == nil {
		return newRequestAnalysis(payload)
	}
	cache, _ := ctx.Value(analysisContextKey{}).(*analysisCache)
	if cache == nil {
		return newRequestAnalysis(payload)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.current == nil || !bytes.Equal(cache.current.payload, payload) {
		cache.current = newRequestAnalysis(payload)
	}
	return cache.current
}

// ReconcileAnalysis discards stale facts after a plugin returns a replacement
// body, while retaining the index when a retry returns identical bytes. Plugins
// receive isolated body copies, so they cannot mutate the indexed buffer.
func ReconcileAnalysis(ctx context.Context, payload []byte) {
	if ctx == nil {
		return
	}
	if cache, ok := ctx.Value(analysisContextKey{}).(*analysisCache); ok {
		cache.mu.Lock()
		if cache.current != nil && !bytes.Equal(cache.current.payload, payload) {
			cache.current = nil
		}
		cache.mu.Unlock()
	}
}

// InvalidateAnalysis must precede reuse after an in-place body mutation.
func InvalidateAnalysis(ctx context.Context) {
	if ctx == nil {
		return
	}
	if cache, ok := ctx.Value(analysisContextKey{}).(*analysisCache); ok {
		cache.mu.Lock()
		cache.current = nil
		cache.mu.Unlock()
	}
}
func ExtractSessionInfoContext(ctx context.Context, headers http.Header, payload []byte, metadata map[string]any) (SessionInfo, bool) {
	return analysisFor(ctx, payload).extractInfo(headers, metadata)
}
func EnrichContext(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	ctx = WithAnalysisCache(ctx)
	req, opts = enrich(ctx, req, opts)
	return ctx, req, opts
}

// ConversationAliasContext reuses the same immutable body index as identity extraction.
func ConversationAliasContext(ctx context.Context, payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root, req, nested := analysisFor(ctx, payload).objects()
	conversation := root.Get("conversation")
	if !conversation.Exists() && nested {
		conversation = req.Get("conversation")
	}
	if sid := normalizedSessionCandidate(conversation.Get("id").String()); sid != "" {
		return "conv:" + sid
	}
	if conversation.Type == gjson.String {
		if sid := normalizedSessionCandidate(conversation.String()); sid != "" {
			return "conv:" + sid
		}
	}
	return ""
}

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestStrictAuditSharesOneGenerationAttemptAcrossRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer upstream.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:streamGenerateContent", nil)
			c.Request.Header.Set("X-Sub2API-Audit-Hop", "strict_v1")
			h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
			ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
			defer cancel()
			// Normal runtime configuration must not enlarge the audit budget.
			ctx = geminiresponse.WithBudget(ctx, 4)
			for attempt := 0; attempt < 3; attempt++ {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := geminiresponse.Do(ctx, upstream.Client(), req)
				if attempt == 0 {
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					geminiresponse.SaveFailure(ctx, &geminiresponse.Error{Code: "upstream_failure", Message: "fixture", Status: status})
				} else if err == nil {
					resp.Body.Close()
					t.Fatal("audit retry reached the upstream")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("generation calls=%d, want 1", calls.Load())
			}
		})
	}
}

func TestOrdinaryRequestsKeepConfiguredGenerationBudget(t *testing.T) {
	for _, marker := range []string{"", "other_policy"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:streamGenerateContent", nil)
		c.Request.Header.Set("X-Sub2API-Audit-Hop", marker)
		h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
		ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
		if geminiresponse.GetBudget(ctx) != nil {
			t.Fatal("ordinary request received audit budget")
		}
		ctx = geminiresponse.WithBudget(ctx, 2)
		for i := 0; i < 2; i++ {
			if _, err := geminiresponse.TakeAttempt(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := geminiresponse.TakeAttempt(ctx); err == nil {
			t.Fatal("configured budget not enforced")
		}
		cancel()
	}
}

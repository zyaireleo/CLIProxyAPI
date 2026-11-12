package helps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAntigravityBudgetCountsRedirectSends(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/again")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	ctx := cliproxyexecutor.WithAntigravityGuard(context.Background())
	client := WithAntigravityAttemptBudget(ctx, server.Client(), "gemini-test")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"contents":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var guard *cliproxyexecutor.AntigravityGuardError
	if !errors.As(err, &guard) || guard.Class != cliproxyexecutor.GuardClassBudget {
		t.Fatalf("redirect budget not enforced: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("actual requests=%d", calls.Load())
	}
}

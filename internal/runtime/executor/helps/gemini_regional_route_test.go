package helps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
)

func TestGeminiRegionalIsolationIsCredentialAndExitScoped(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"User location is not supported","status":"PERMISSION_DENIED"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer server.Close()
	ctx := geminiresponse.WithRoute(geminiresponse.WithBudget(context.Background(), 4), t.Name(), "exit-one", true)
	invoke := func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		response, err := DoGeminiGeneration(ctx, server.Client(), req, "gemini-3.8-flash")
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}
	var fault *geminiresponse.Error
	if err := invoke(ctx); !errors.As(err, &fault) || !fault.RouteFault || fault.Status != 503 || fault.Stop {
		t.Fatalf("regional rejection must permit recovery on another route: %v", err)
	}
	if err := invoke(ctx); err == nil || calls.Load() != 1 || geminiresponse.Attempts(ctx) != 1 {
		t.Fatalf("known rejected pair was called or consumed generation budget: %v calls=%d attempts=%d", err, calls.Load(), geminiresponse.Attempts(ctx))
	}
	changedExit := geminiresponse.WithRoute(ctx, t.Name(), "exit-two", true)
	if err := invoke(changedExit); err != nil {
		t.Fatal(err)
	}
	changedCredential := geminiresponse.WithRoute(ctx, t.Name()+"-other", "exit-one", true)
	if err := invoke(changedCredential); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || geminiresponse.Attempts(ctx) != 3 {
		t.Fatal("unaffected credential/exit pairs were isolated")
	}
}

func TestGeminiOtherForbiddenErrorsDoNotIsolateExit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Permission denied"}}`))
	}))
	defer server.Close()
	ctx := geminiresponse.WithRoute(context.Background(), t.Name(), "exit", true)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		response, err := DoGeminiGeneration(ctx, server.Client(), req, "gemini-3.8-flash")
		if err != nil || response == nil || response.StatusCode != 403 {
			t.Fatalf("unrelated forbidden response changed: %v", err)
		}
		_ = response.Body.Close()
	}
	if calls.Load() != 2 {
		t.Fatal("unrelated 403 isolated the credential/exit pair")
	}
}

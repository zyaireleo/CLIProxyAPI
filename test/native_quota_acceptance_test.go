package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type nativeQuotaFixtureTransport struct {
	base   *url.URL
	client *http.Transport
}

func (r *nativeQuotaFixtureTransport) RoundTripperFor(*cliproxyauth.Auth) http.RoundTripper { return r }

func (r *nativeQuotaFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Hostname() {
	case "chatgpt.com", "daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "cloudcode-pa.googleapis.com":
	default:
		return nil, fmt.Errorf("fixture refuses external host %s", req.URL.Hostname())
	}
	copyReq := req.Clone(req.Context())
	copyURL := *req.URL
	copyURL.Scheme = r.base.Scheme
	copyURL.Host = r.base.Host
	copyReq.URL = &copyURL
	copyReq.Host = ""
	return r.client.RoundTrip(copyReq)
}

func TestNativeQuotaIsolatedProviderDispatch(t *testing.T) {
	for _, provider := range []string{"antigravity", "codex"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, stream), func(t *testing.T) {
				const good = "fixture-good"
				const exhausted = "fixture-exhausted"
				var mu sync.Mutex
				var generated []string
				var quotaCalls int
				reset := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					if strings.Contains(r.URL.Path, "retrieveUserQuotaSummary") || r.URL.Path == "/backend-api/wham/usage" {
						mu.Lock()
						quotaCalls++
						mu.Unlock()
						remaining := 0.8
						if account == exhausted {
							remaining = 0
						}
						w.Header().Set("Content-Type", "application/json")
						if provider == "antigravity" {
							if r.Header.Get("User-Agent") != "antigravity/hub/2.9.1 darwin/arm64" || r.Header.Get("Accept") != "application/json" {
								t.Errorf("incorrect quota headers: UA=%q Accept=%q", r.Header.Get("User-Agent"), r.Header.Get("Accept"))
							}
							_, _ = fmt.Fprintf(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":%g,"resetTime":%q},{"bucketId":"gemini-5h","window":"5h","remainingFraction":1,"resetTime":%q}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1,"resetTime":%q},{"bucketId":"3p-5h","window":"5h","remainingFraction":1,"resetTime":%q}]}]}`, remaining, reset, reset, reset, reset)
						} else {
							_, _ = fmt.Fprintf(w, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`, (1-remaining)*100)
						}
						return
					}
					if strings.Contains(r.URL.Path, "generateContent") || strings.Contains(r.URL.Path, "streamGenerateContent") || strings.HasSuffix(r.URL.Path, "/responses") {
						mu.Lock()
						generated = append(generated, account)
						mu.Unlock()
						if account != good {
							t.Errorf("exhausted high-priority account reached generation: %q", account)
						}
						if provider == "codex" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fixture-response\"}}\n\n")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
						} else {
							response := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-flash-lite"}}`
							if strings.Contains(r.URL.Path, "streamGenerateContent") {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprintf(w, "data: %s\n\n", response)
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, response)
							}
						}
						return
					}
					t.Errorf("unexpected upstream operation %s %s", r.Method, r.URL.Path)
					http.Error(w, "fixture refuses unexpected operation", http.StatusBadRequest)
				}))
				defer server.Close()
				base, _ := url.Parse(server.URL)
				transport := &nativeQuotaFixtureTransport{base: base, client: &http.Transport{Proxy: nil}}
				defer transport.client.CloseIdleConnections()
				cfg := &config.Config{}
				manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
				manager.SetRetryConfig(0, 0, 2)
				manager.SetRoundTripperProvider(transport)
				model := "gemini-2.5-flash-lite"
				if provider == "codex" {
					model = "gpt-5.4"
					manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
				} else {
					manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(cfg))
				}
				ids := []string{provider + "-exhausted", provider + "-good"}
				for i, id := range ids {
					token, priority := exhausted, "10"
					if i == 1 {
						token, priority = good, "1"
					}
					registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					_, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
						ID: id, Provider: provider, Status: cliproxyauth.StatusActive,
						Attributes: map[string]string{"priority": priority},
						Metadata:   map[string]any{"type": provider, "access_token": token, "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "fixture-project", "account_id": "fixture-account", "email": "fixture@example.test"},
					})
					if errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				if errConfig := manager.ApplyNativeQuotaConfig(context.Background(), config.NativeQuotaConfig{Mode: "enforce", Providers: []string{provider}}); errConfig != nil {
					t.Fatal(errConfig)
				}
				defer func() {
					if errStop := manager.StopNativeQuota(context.Background()); errStop != nil {
						t.Error(errStop)
					}
				}()
				deadline := time.Now().Add(10 * time.Second)
				for {
					high := manager.NativeQuota().Assess(ids[0], provider, model, time.Now())
					low := manager.NativeQuota().Assess(ids[1], provider, model, time.Now())
					if !high.ObservedAt.IsZero() && !low.ObservedAt.IsZero() && !high.Allowed && low.Allowed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("collector not ready: high=%+v low=%+v", high, low)
					}
					time.Sleep(10 * time.Millisecond)
				}
				var payload []byte
				opts := cliproxyexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FromString("gemini")}
				if provider == "codex" {
					opts.SourceFormat = sdktranslator.FromString("openai-response")
					payload = []byte(fmt.Sprintf(`{"model":%q,"input":"fixture"}`, model))
				} else {
					payload = []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}],"generationConfig":{"maxOutputTokens":8}}`)
				}
				request := cliproxyexecutor.Request{Model: model, Payload: payload}
				if stream {
					response, errStream := manager.ExecuteStream(context.Background(), []string{provider}, request, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					_, errExecute := manager.Execute(context.Background(), []string{provider}, request, opts)
					if errExecute != nil {
						t.Fatal(errExecute)
					}
				}
				mu.Lock()
				if len(generated) != 1 || generated[0] != good {
					t.Errorf("generation attempts=%v", generated)
				}
				if quotaCalls < 2 {
					t.Errorf("quota reads=%d", quotaCalls)
				}
				mu.Unlock()
				for _, id := range ids {
					auth, _ := manager.GetByID(id)
					if auth.Disabled {
						t.Errorf("quota changed persistent disabled for %s", id)
					}
					view := manager.NativeQuota().Assess(id, provider, model, time.Now())
					if view.InFlight != 0 {
						data, _ := json.Marshal(view)
						t.Errorf("leaked admission %s", data)
					}
				}
			})
		}
	}
}

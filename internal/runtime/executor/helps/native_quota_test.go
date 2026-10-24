package helps

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type quotaRedirectTransport struct {
	calls int
}

func (r *quotaRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://unrelated.example/credential-capture"}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestNativeQuotaRequestAllowlist(t *testing.T) {
	for _, test := range []struct {
		provider string
		method   string
		url      string
		valid    bool
	}{
		{"antigravity", "POST", "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", true},
		{"antigravity", "POST", "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels", true},
		{"antigravity", "POST", "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", true},
		{"codex", "GET", "https://chatgpt.com/backend-api/wham/usage", true},
		{"codex", "GET", "https://chatgpt.com.evil.example/backend-api/wham/usage", false},
		{"codex", "GET", "https://user:pass@chatgpt.com/backend-api/wham/usage", false},
		{"codex", "GET", "https://chatgpt.com:443/backend-api/wham/usage", false},
		{"codex", "GET", "https://chatgpt.com/backend-api/wham/usage?redirect=x", false},
		{"codex", "POST", "https://chatgpt.com/backend-api/wham/usage", false},
		{"codex", "GET", "https://chatgpt.com/backend-api/%77ham/usage", false},
		{"codex", "GET", "http://127.0.0.1/backend-api/wham/usage", false},
		{"antigravity", "POST", "https://cloudcode-pa.googleapis.com/v1internal:generateContent", false},
		{"antigravity", "GET", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", false},
	} {
		t.Run(test.provider+"/"+test.url, func(t *testing.T) {
			req, errRequest := http.NewRequest(test.method, test.url, nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			if got := ValidateNativeQuotaRequest(req, test.provider); (got == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, got)
			}
		})
	}
}

func TestNativeQuotaClientDoesNotRedirectOrMutateSharedClient(t *testing.T) {
	transport := &quotaRedirectTransport{}
	shared := &http.Client{Transport: transport}
	client := NativeQuotaHTTPClient(shared)
	if shared.CheckRedirect != nil {
		t.Fatal("shared client redirect policy was mutated")
	}
	req, errRequest := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req.Header.Set("Authorization", "Bearer test-only")
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if transport.calls != 1 || resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: calls=%d status=%d", transport.calls, resp.StatusCode)
	}
}

package helps

import (
	"fmt"
	"net/http"
	"strings"
)

const NativeAntigravityQuotaUserAgent = "antigravity/hub/2.9.1 darwin/arm64"

func ValidateNativeQuotaRequest(req *http.Request, provider string) error {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" || req.URL.RawQuery != "" || req.URL.Fragment != "" || req.URL.Opaque != "" {
		return fmt.Errorf("native quota: invalid upstream request")
	}
	host := strings.ToLower(req.URL.Host)
	switch provider {
	case "antigravity":
		if host != "daily-cloudcode-pa.googleapis.com" && host != "daily-cloudcode-pa.sandbox.googleapis.com" && host != "cloudcode-pa.googleapis.com" {
			return fmt.Errorf("native quota: unsupported antigravity host")
		}
		if req.Method == http.MethodPost && (req.URL.Path == "/v1internal:retrieveUserQuotaSummary" || req.URL.Path == "/v1internal:fetchAvailableModels") && req.URL.RawPath == "" {
			return nil
		}
	case "codex":
		if host == "chatgpt.com" && req.Method == http.MethodGet && req.URL.Path == "/backend-api/wham/usage" && req.URL.RawPath == "" {
			return nil
		}
	}
	return fmt.Errorf("native quota: unsupported upstream operation")
}

func NativeQuotaHTTPClient(client *http.Client) *http.Client {
	copyClient := *client
	// Quota credentials must never follow an upstream redirect to another resource.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copyClient
}

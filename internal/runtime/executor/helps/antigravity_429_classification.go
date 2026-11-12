package helps

import (
	"github.com/MoeForge/moego/ojson"
	"net/http"
	"strings"
)

// AntigravityFailureClass uses structured provider evidence only. A retry hint
// describes timing; it does not establish exhausted quota or a policy refusal.
func AntigravityFailureClass(status int, body []byte) string {
	// Authentication and account authorization failures retain credential-health
	// protection even when an upstream payload also contains policy metadata.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ""
	}
	root := ojson.Parse(string(body))
	if response := root.Get("response"); response.Exists() {
		root = response
	}
	quota, rate, policy := false, false, false
	for _, detail := range root.Get("error.details").Array() {
		switch detail.Get("@type").String() {
		case "type.googleapis.com/google.rpc.QuotaFailure":
			quota = len(detail.Get("violations").Array()) > 0 || quota
		case "type.googleapis.com/google.rpc.ErrorInfo":
			switch strings.ToUpper(detail.Get("reason").String()) {
			case "QUOTA_EXHAUSTED", "INSUFFICIENT_G1_CREDITS_BALANCE":
				quota = true
			case "RATE_LIMIT_EXCEEDED":
				rate = true
			case "SAFETY", "PROHIBITED_CONTENT", "CONTENT_POLICY_VIOLATION", "CONTENT_POLICY_BLOCK":
				policy = true
			}
		}
	}
	// Credential/quota evidence outranks request policy when evidence conflicts.
	if quota {
		return "upstream_quota_exhausted"
	}
	if rate {
		return "upstream_rate_limited"
	}
	if reason := root.Get("promptFeedback.blockReason").String(); reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
		policy = true
	}
	for _, path := range []string{"error.reason", "error.code"} {
		policy = policy || antigravityPolicyCode(root.Get(path).String())
	}
	for _, candidate := range root.Get("candidates").Array() {
		policy = policy || antigravityPolicyCode(candidate.Get("finishReason").String())
	}
	if policy {
		return "upstream_request_rejected"
	}
	if status == http.StatusTooManyRequests {
		return "upstream_429_ambiguous"
	}
	return ""
}

func antigravityPolicyCode(code string) bool {
	switch strings.ToUpper(code) {
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "IMAGE_SAFETY", "RECITATION", "CONTENT_POLICY_VIOLATION", "CONTENT_POLICY_BLOCK":
		return true
	}
	return false
}

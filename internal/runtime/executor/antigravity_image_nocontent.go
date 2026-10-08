package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// antigravityNoImageContentCooldown keeps a credential that answered an image
// model with text-only content out of image rotation briefly so sibling
// credentials absorb image traffic. The window mirrors the default transient
// error cooldown and only applies to image models.
const antigravityNoImageContentCooldown = time.Minute

// antigravityImageModelName reports whether the model is an image-generation
// model whose responses are expected to carry image content.
func antigravityImageModelName(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "image")
}

// antigravityImageOutputDisabled reports whether the client explicitly asked
// an image model for a text-only response. Such replies are expected and must
// not trigger credential rotation.
func antigravityImageOutputDisabled(originalRequest []byte) bool {
	return !cliproxyexecutor.ImageOutputRequested(originalRequest)
}

// antigravityResponseContainsImage scans a completed response payload for the
// keys that carry generated image data across the response formats CPA emits
// (gemini inlineData, openai image_url/b64_json, claude image blocks). A false
// positive only skips rotation, which is the pre-existing behavior.
func antigravityResponseContainsImage(payload []byte) bool {
	for _, marker := range [...][]byte{
		[]byte(`"inlineData"`),
		[]byte(`"inline_data"`),
		[]byte(`"image_url"`),
		[]byte(`"b64_json"`),
		[]byte(`"type":"image"`),
	} {
		if bytes.Contains(payload, marker) {
			return true
		}
	}
	return false
}

// antigravityNoImageContentSignal reports a completed image-model response
// that carries no image content. The conductor rotates to another credential
// and returns a classified failure when the Flash Image budget is exhausted.
func antigravityNoImageContentSignal(model string) *cliproxyexecutor.NoImageContentError {
	cooldown := antigravityNoImageContentCooldown
	return &cliproxyexecutor.NoImageContentError{Model: model, Cooldown: &cooldown}
}

// validateFlashImageOutput runs before translation and success usage publication.
func validateFlashImageOutput(ctx context.Context, auth *cliproxyauth.Auth, model string, request []byte, summary helps.ImageResponseSummary) error {
	if !cliproxyexecutor.FlashImageModel(model) || !cliproxyexecutor.ImageOutputRequested(request) {
		return nil
	}
	category := "image"
	var err error
	if summary.Images == 0 {
		category = "empty"
		if summary.Text {
			category = "text_only"
		}
		switch {
		case summary.Block != "" && summary.Block != "BLOCK_REASON_UNSPECIFIED", imageSafetyFinish(summary.Finish):
			category = "content_policy"
			err = &cliproxyexecutor.ImageFailure{Code: "content_policy_violation", Message: "Upstream declined this image request", Status: 400, Stop: true}
		case summary.Finish == "MAX_TOKENS":
			category = "output_incomplete"
			err = &cliproxyexecutor.ImageFailure{Code: "upstream_output_incomplete", Message: "Upstream image output was truncated", Status: 502, Stop: true}
		default:
			err = antigravityNoImageContentSignal(model)
		}
	}
	id := ""
	if auth != nil {
		id = auth.ID
	}
	hash := sha256.Sum256([]byte(id))
	log.WithFields(log.Fields{"request_id": logging.GetRequestID(ctx), "sub2api_trace_id": logging.GetSub2APITraceID(ctx), "auth_ref": fmt.Sprintf("%x", hash[:6]), "model": model, "attempt": cliproxyexecutor.ImageGenerationAttempts(ctx), "outcome": category, "images": summary.Images, "finish_reason": safeImageReason(summary.Finish), "block_reason": safeImageReason(summary.Block)}).Info("antigravity image generation outcome")
	return err
}

// Upstream reason enums are logged without arbitrary response text.
func safeImageReason(reason string) string {
	if len(reason) > 64 {
		return "UNKNOWN"
	}
	for _, r := range reason {
		if r != '_' && (r < 'A' || r > 'Z') {
			return "UNKNOWN"
		}
	}
	return reason
}

func imageSafetyFinish(reason string) bool {
	switch reason {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return true
	}
	return false
}

func antigravityImageStatusError(model string, status int, body []byte) error {
	if !cliproxyexecutor.FlashImageModel(model) {
		return newAntigravityStatusErr(status, body)
	}
	if status == http.StatusBadRequest {
		return &cliproxyexecutor.ImageFailure{Code: "invalid_request_error", Message: "Upstream rejected this image request", Status: status, Stop: true}
	}
	if status == 429 && decideAntigravity429(body).kind == antigravity429DecisionFullQuotaExhausted {
		retry, _ := helps.ParseRetryDelay(body)
		return &cliproxyexecutor.ImageFailure{Code: "upstream_quota_exhausted", Message: "An upstream image generation attempt exhausted its model quota", Status: 429, Cooldown: retry}
	}
	code, message := "upstream_error", "Upstream image generation failed"
	switch status {
	case http.StatusUnauthorized:
		code, message = "upstream_authentication_error", "Upstream image authentication failed"
	case http.StatusForbidden:
		code, message = "upstream_access_denied", "Upstream image access was denied"
	case http.StatusTooManyRequests:
		code, message = "upstream_rate_limited", "Upstream image generation is temporarily rate limited"
	case http.StatusServiceUnavailable:
		code, message = "upstream_capacity_unavailable", "Upstream image generation capacity is unavailable"
	}
	retry, _ := helps.ParseRetryDelay(body)
	return &cliproxyexecutor.ImageFailure{Code: code, Message: message, Status: status, Cooldown: retry}
}

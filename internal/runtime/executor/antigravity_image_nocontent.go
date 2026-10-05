package executor

import (
	"bytes"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
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
	mods := gjson.GetBytes(originalRequest, "generationConfig.responseModalities").Array()
	if len(mods) == 0 {
		return false
	}
	for _, m := range mods {
		if strings.EqualFold(strings.TrimSpace(m.String()), "IMAGE") {
			return false
		}
	}
	return true
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
// and falls back to the original response when rotation is exhausted.
func antigravityNoImageContentSignal(model string) *cliproxyexecutor.NoImageContentError {
	cooldown := antigravityNoImageContentCooldown
	return &cliproxyexecutor.NoImageContentError{Model: model, Cooldown: &cooldown}
}

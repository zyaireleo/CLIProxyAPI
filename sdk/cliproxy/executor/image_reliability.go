package executor

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

// FlashImageModel normalizes the deployed native model and its public alias.
func FlashImageModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	model = strings.TrimSuffix(model, "-preview")
	return model == "gemini-3.1-flash-image"
}

// ImageOutputRequested preserves an explicit text-only request on the image model.
func ImageOutputRequested(payload []byte) bool {
	mods := gjson.GetBytes(payload, "generationConfig.responseModalities")
	if !mods.Exists() {
		mods = gjson.GetBytes(payload, "request.generationConfig.responseModalities")
	}
	values := mods.Array()
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value.String()), "IMAGE") {
			return true
		}
	}
	return false
}

// ImageFailure keeps client diagnostics separate from per-credential retry timing.
type ImageFailure struct {
	Code     string
	Message  string
	Status   int
	Stop     bool
	Cooldown *time.Duration
}

func (e *ImageFailure) Error() string {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": e.Message, "type": "upstream_error", "code": e.Code, "retryable": !e.Stop,
	}})
	return string(body)
}
func (e *ImageFailure) StatusCode() int            { return e.Status }
func (e *ImageFailure) IsRequestScoped() bool      { return e.Stop }
func (e *ImageFailure) RetryAfter() *time.Duration { return e.Cooldown }

type imageBudgetKey struct{}
type imageBudget struct{ used atomic.Int32 }

// WithImageGenerationBudget never resets a budget inherited by nested execution.
func WithImageGenerationBudget(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(imageBudgetKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, imageBudgetKey{}, &imageBudget{})
}

// ConsumeImageGeneration is called at the generation transport boundary, not during auth refresh.
func ConsumeImageGeneration(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(imageBudgetKey{}).(*imageBudget)
	if b == nil {
		return nil
	}
	for {
		n := b.used.Load()
		if n >= 4 {
			return &ImageFailure{Code: "upstream_image_budget_exhausted", Message: "Image generation attempt budget exhausted", Status: 503, Stop: true}
		}
		if b.used.CompareAndSwap(n, n+1) {
			return nil
		}
	}
}
func ImageGenerationAttempts(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if b, _ := ctx.Value(imageBudgetKey{}).(*imageBudget); b != nil {
		return int(b.used.Load())
	}
	return 0
}

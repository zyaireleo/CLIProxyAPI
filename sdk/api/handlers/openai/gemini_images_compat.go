package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

type geminiImagePart struct {
	Text       string                 `json:"text,omitempty"`
	InlineData *geminiInlineImageData `json:"inlineData,omitempty"`
}

type geminiInlineImageData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiImageContent struct {
	Role  string            `json:"role"`
	Parts []geminiImagePart `json:"parts"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
	ImageSize   string `json:"imageSize,omitempty"`
}

type geminiImageGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	ImageConfig        *geminiImageConfig `json:"imageConfig,omitempty"`
}

type geminiImageRequest struct {
	Contents         []geminiImageContent        `json:"contents"`
	GenerationConfig geminiImageGenerationConfig `json:"generationConfig"`
}

func isGeminiNativeImagesModel(model string) bool {
	base := imagesModelBase(model)
	switch base {
	case "gemini-3.1-flash-image", "gemini-3.1-flash-image-preview", "gemini-3-pro-image", "gemini-3-pro-image-preview":
		return true
	default:
		return false
	}
}

func geminiImageAspectRatio(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1024x1024", "2048x2048", "1:1", "square":
		return "1:1"
	case "1792x1024", "16:9", "landscape":
		return "16:9"
	case "1024x1792", "9:16", "portrait":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	case "1344x768":
		return "16:9"
	case "768x1344":
		return "9:16"
	default:
		return ""
	}
}

func geminiImageSize(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1k", "2k", "4k":
		return strings.ToUpper(strings.TrimSpace(size))
	default:
		return ""
	}
}

func geminiImageDataPart(value string) (geminiImagePart, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return geminiImagePart{}, fmt.Errorf("image input is empty")
	}
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		return geminiImagePart{}, fmt.Errorf("image input must be a base64 data URL")
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return geminiImagePart{}, fmt.Errorf("image data URL is malformed")
	}
	header := value[:comma]
	data := value[comma+1:]
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return geminiImagePart{}, fmt.Errorf("image data URL must be base64 encoded")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return geminiImagePart{}, fmt.Errorf("image data URL is invalid: %w", err)
	}
	mimeType := strings.SplitN(header[5:], ";", 2)[0]
	if mimeType == "" {
		mimeType = "image/png"
	}
	return geminiImagePart{InlineData: &geminiInlineImageData{MimeType: mimeType, Data: data}}, nil
}

func validateGeminiImageOptions(responseFormat string, n int64, quality, background string) error {
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format != "" && format != "b64_json" && format != "url" {
		return fmt.Errorf("response_format %q is not supported", responseFormat)
	}
	if n > 1 {
		return fmt.Errorf("n=%d is not supported; Gemini image generation supports one image per request", n)
	}
	if n < 0 {
		return fmt.Errorf("n=%d is invalid", n)
	}
	if strings.TrimSpace(quality) != "" {
		return fmt.Errorf("quality is not supported for Gemini image generation")
	}
	if strings.TrimSpace(background) != "" {
		return fmt.Errorf("background is not supported for Gemini image generation")
	}
	return nil
}

func validateGeminiImageSize(size string) error {
	size = strings.TrimSpace(size)
	if size != "" && geminiImageAspectRatio(size) == "" && geminiImageSize(size) == "" {
		return fmt.Errorf("size %q is not supported", size)
	}
	return nil
}

func buildGeminiImageRequest(model, prompt, size string, images []string) ([]byte, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	parts := []geminiImagePart{{Text: strings.TrimSpace(prompt)}}
	for _, image := range images {
		part, err := geminiImageDataPart(image)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	config := geminiImageGenerationConfig{ResponseModalities: []string{"IMAGE", "TEXT"}}
	imageConfig := &geminiImageConfig{AspectRatio: geminiImageAspectRatio(size), ImageSize: geminiImageSize(size)}
	if imageConfig.AspectRatio != "" || imageConfig.ImageSize != "" {
		config.ImageConfig = imageConfig
	}
	return json.Marshal(geminiImageRequest{
		Contents:         []geminiImageContent{{Role: "user", Parts: parts}},
		GenerationConfig: config,
	})
}

func parseGeminiImageResponse(payload []byte, responseFormat string) ([]byte, error) {
	if !json.Valid(payload) {
		return nil, fmt.Errorf("upstream returned invalid Gemini image response JSON")
	}
	output := map[string]any{"created": time.Now().Unix(), "data": []any{}}
	data := output["data"].([]any)
	for _, candidate := range gjson.GetBytes(payload, "candidates").Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			inline := part.Get("inlineData")
			if !inline.Exists() {
				inline = part.Get("inline_data")
			}
			encoded := strings.TrimSpace(inline.Get("data").String())
			if encoded == "" {
				continue
			}
			mimeType := inline.Get("mimeType").String()
			if mimeType == "" {
				mimeType = inline.Get("mime_type").String()
			}
			if mimeType == "" {
				mimeType = "image/png"
			}
			item := map[string]any{}
			if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
				item["url"] = "data:" + mimeType + ";base64," + encoded
			} else {
				item["b64_json"] = encoded
			}
			data = append(data, item)
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("upstream returned no image output")
	}
	output["data"] = data
	return json.Marshal(output)
}

func (h *OpenAIAPIHandler) handleGeminiNativeImages(c *gin.Context, model, prompt, size, responseFormat string, images []string, stream bool) {
	if stream {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "streaming is not supported for Gemini native image compatibility", Type: "invalid_request_error"}})
		return
	}
	rawJSON, err := buildGeminiImageRequest(model, prompt, size, images)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "Invalid request: " + err.Error(), Type: "invalid_request_error"}})
		return
	}
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	defer cliCancel()
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	defer stopKeepAlive()
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, "gemini", imagesModelBase(model), rawJSON, "")
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		return
	}
	body, err := parseGeminiImageResponse(resp, responseFormat)
	if err != nil {
		c.JSON(http.StatusBadGateway, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: err.Error(), Type: "upstream_error"}})
		return
	}
	c.Header("Content-Type", "application/json")
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(body)
}

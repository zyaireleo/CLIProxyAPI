package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiNativeImagesModels(t *testing.T) {
	for _, model := range []string{
		"gemini-3.1-flash-image",
		"gemini-3.1-flash-image-preview",
		"gemini-3-pro-image",
		"gemini-3-pro-image-preview",
		"gemini/gemini-3.1-flash-image",
	} {
		if !isGeminiNativeImagesModel(model) {
			t.Fatalf("expected %q to be supported", model)
		}
	}
	for _, model := range []string{"gemini-2.5-flash", "gpt-image-1.5"} {
		if isGeminiNativeImagesModel(model) {
			t.Fatalf("expected %q to be rejected", model)
		}
	}
}

func TestBuildGeminiImageRequest(t *testing.T) {
	req, err := buildGeminiImageRequest("gemini-3.1-flash-image", "edit this", "1792x1024", []string{"data:image/png;base64,AA=="})
	if err != nil {
		t.Fatalf("buildGeminiImageRequest error: %v", err)
	}
	if got := gjson.GetBytes(req, "contents.0.role").String(); got != "user" {
		t.Fatalf("role = %q, want user", got)
	}
	if got := gjson.GetBytes(req, "contents.0.parts.0.text").String(); got != "edit this" {
		t.Fatalf("prompt = %q, want edit this", got)
	}
	if got := gjson.GetBytes(req, "contents.0.parts.1.inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", got)
	}
	if got := gjson.GetBytes(req, "generationConfig.imageConfig.aspectRatio").String(); got != "16:9" {
		t.Fatalf("aspectRatio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "generationConfig.responseModalities.0").String(); got != "IMAGE" {
		t.Fatalf("response modality = %q, want IMAGE", got)
	}
}

func TestBuildGeminiImageRequestRejectsInvalidImageData(t *testing.T) {
	if _, err := buildGeminiImageRequest("gemini-3.1-flash-image", "draw", "", []string{"data:image/png;base64,not-base64"}); err == nil {
		t.Fatal("expected malformed image data URL to fail")
	}
}

func TestParseGeminiImageResponse(t *testing.T) {
	payload := []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}`)
	for _, format := range []string{"", "b64_json", "url"} {
		resp, err := parseGeminiImageResponse(payload, format)
		if err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		if format == "url" {
			if got := gjson.GetBytes(resp, "data.0.url").String(); got != "data:image/png;base64,AA==" {
				t.Fatalf("url = %q", got)
			}
		} else if got := gjson.GetBytes(resp, "data.0.b64_json").String(); got != "AA==" {
			t.Fatalf("b64_json = %q", got)
		}
	}
}

func TestValidateGeminiImageOptions(t *testing.T) {
	valid := []struct {
		format string
		n      int64
	}{
		{format: "b64_json", n: 1},
		{format: "url", n: 0},
	}
	for _, tc := range valid {
		if err := validateGeminiImageOptions(tc.format, tc.n, "", ""); err != nil {
			t.Fatalf("valid options rejected: %v", err)
		}
	}
	for _, tc := range []struct {
		format, quality, background string
		n                           int64
	}{
		{format: "json", n: 1},
		{format: "b64_json", n: 2},
		{format: "b64_json", quality: "hd", n: 1},
		{format: "b64_json", background: "transparent", n: 1},
	} {
		if err := validateGeminiImageOptions(tc.format, tc.n, tc.quality, tc.background); err == nil {
			t.Fatalf("expected options to fail: %+v", tc)
		}
	}
}

func TestValidateGeminiImageSize(t *testing.T) {
	for _, size := range []string{"", "1024x1024", "16:9", "2k"} {
		if err := validateGeminiImageSize(size); err != nil {
			t.Fatalf("valid size %q rejected: %v", size, err)
		}
	}
	if err := validateGeminiImageSize("500x500"); err == nil {
		t.Fatal("expected unsupported size to fail")
	}
}

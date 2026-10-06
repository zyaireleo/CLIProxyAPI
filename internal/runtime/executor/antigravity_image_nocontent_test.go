package executor

import (
	"testing"
	"time"
)

func TestAntigravityImageModelName(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-3.1-flash-image-preview", true},
		{"gemini-2.5-flash-image", true},
		{"Gemini-3.1-Flash-Image", true},
		{"gemini-3.6-flash", false},
		{"gemini-3-pro-preview", false},
		{"claude-sonnet-4-6", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := antigravityImageModelName(tc.model); got != tc.want {
			t.Errorf("antigravityImageModelName(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestAntigravityImageOutputDisabled(t *testing.T) {
	cases := []struct {
		name    string
		request string
		want    bool
	}{
		{"no modalities", `{"contents":[]}`, false},
		{"text and image", `{"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`, false},
		{"image only", `{"generationConfig":{"responseModalities":["IMAGE"]}}`, false},
		{"lowercase image", `{"generationConfig":{"responseModalities":["text","image"]}}`, false},
		{"text only", `{"generationConfig":{"responseModalities":["TEXT"]}}`, true},
		{"empty modalities", `{"generationConfig":{"responseModalities":[]}}`, false},
		{"invalid json", "not-json", false},
	}
	for _, tc := range cases {
		if got := antigravityImageOutputDisabled([]byte(tc.request)); got != tc.want {
			t.Errorf("%s: antigravityImageOutputDisabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAntigravityResponseContainsImage(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     bool
	}{
		{"gemini inline image", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"abc"}}]}}]}`, true},
		{"gemini snake case", `{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/jpeg","data":"abc"}}]}}]}`, true},
		{"openai image url", `{"choices":[{"message":{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}}]}`, true},
		{"openai b64", `{"data":[{"b64_json":"abc"}]}`, true},
		{"claude image block", `{"content":[{"type":"image","source":{"type":"base64","data":"abc"}}]}`, true},
		{"text only", `{"candidates":[{"content":{"parts":[{"text":"I cannot generate that."}]}}]}`, false},
		{"model name echo only", `{"modelVersion":"gemini-3.1-flash-image-preview","candidates":[{"content":{"parts":[{"text":"done"}]}}]}`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		if got := antigravityResponseContainsImage([]byte(tc.response)); got != tc.want {
			t.Errorf("%s: antigravityResponseContainsImage = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAntigravityNoImageContentSignal(t *testing.T) {
	signal := antigravityNoImageContentSignal("gemini-3.1-flash-image-preview")
	if signal == nil {
		t.Fatal("signal = nil")
	}
	if signal.Model != "gemini-3.1-flash-image-preview" {
		t.Errorf("Model = %q", signal.Model)
	}
	if signal.Cooldown == nil || *signal.Cooldown != time.Minute {
		t.Errorf("Cooldown = %v, want 1m", signal.Cooldown)
	}
	if ra := signal.RetryAfter(); ra == nil || *ra != time.Minute {
		t.Errorf("RetryAfter() = %v, want 1m", ra)
	}
	if signal.Error() == "" {
		t.Error("Error() is empty")
	}
}

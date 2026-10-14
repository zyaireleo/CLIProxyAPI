package openai

import (
	"github.com/tidwall/gjson"
	"testing"
)

func TestGeminiImageSeedWireContract(t *testing.T) {
	for _, raw := range []string{"", "null", "0", "-1", "2147483647", "-2147483648"} {
		t.Run(raw, func(t *testing.T) {
			seed, err := parseGeminiImageSeed(raw)
			if err != nil {
				t.Fatal(err)
			}
			body, err := buildGeminiImageRequest("gemini-3-pro-image", "draw", "auto", nil, seed)
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(body, "generationConfig.seed")
			if raw == "" || raw == "null" {
				if got.Exists() {
					t.Fatal("absent seed must stay absent")
				}
			} else if got.Raw != raw {
				t.Fatalf("seed = %s, want %s", got.Raw, raw)
			}
			if gjson.GetBytes(body, "generationConfig.imageConfig").Exists() {
				t.Fatal("auto must use upstream defaults")
			}
		})
	}
	for _, raw := range []string{"1.5", "2147483648", "-2147483649", "\"1\"", "true", "{}", "[]"} {
		if _, err := parseGeminiImageSeed(raw); err == nil {
			t.Fatalf("accepted invalid seed %s", raw)
		}
	}
}

func TestGeminiImageSizeWireContract(t *testing.T) {
	for _, tc := range []struct{ size, ratio, resolution string }{
		{"2048x2048", "1:1", "2K"}, {"4096x4096", "1:1", "4K"},
		{"2k", "", "2K"}, {"4K", "", "4K"}, {"3:4", "3:4", ""},
		{"4:3", "4:3", ""}, {"4:5", "4:5", ""}, {"5:4", "5:4", ""}, {"21:9", "21:9", ""},
	} {
		t.Run(tc.size, func(t *testing.T) {
			if err := validateGeminiImageSize(tc.size); err != nil {
				t.Fatal(err)
			}
			body, err := buildGeminiImageRequest("gemini-3-pro-image", "draw", tc.size, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "generationConfig.imageConfig.aspectRatio").String(); got != tc.ratio {
				t.Fatalf("ratio = %s", got)
			}
			if got := gjson.GetBytes(body, "generationConfig.imageConfig.imageSize").String(); got != tc.resolution {
				t.Fatalf("resolution = %s", got)
			}
		})
	}
}

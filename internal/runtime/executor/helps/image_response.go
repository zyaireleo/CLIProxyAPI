package helps

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	_ "golang.org/x/image/webp"
)

type ImageResponseSummary struct {
	Images int
	Text   bool
	Finish string
	Block  string
}

func ValidInlineImage(mime, data string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if !strings.HasPrefix(strings.ToLower(mime), "image/") || data == "" {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(b) == 0 {
		return false
	}
	if http.DetectContentType(b) != strings.ToLower(mime) {
		return false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 16384 || cfg.Height > 16384 {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(b))
	return err == nil
}

// InspectImageResponse reads native output before response translators synthesize metadata.
func InspectImageResponse(payload []byte) ImageResponseSummary {
	root := gjson.ParseBytes(payload)
	if v := root.Get("response"); v.Exists() {
		root = v
	}
	s := ImageResponseSummary{Block: root.Get("promptFeedback.blockReason").String()}
	for _, c := range root.Get("candidates").Array() {
		if f := c.Get("finishReason").String(); f != "" && (s.Finish == "" || f != "STOP") {
			s.Finish = f
		}
		for _, p := range c.Get("content.parts").Array() {
			if !p.Get("thought").Bool() && strings.TrimSpace(p.Get("text").String()) != "" {
				s.Text = true
			}
			d := p.Get("inlineData")
			if !d.Exists() {
				d = p.Get("inline_data")
			}
			mime := d.Get("mimeType").String()
			if mime == "" {
				mime = d.Get("mime_type").String()
			}
			if ValidInlineImage(mime, d.Get("data").String()) {
				s.Images++
			}
		}
	}
	return s
}

package helps

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"testing"
)

func TestInspectImageResponseValidatesNativeParts(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	for _, tc := range []struct {
		body   string
		images int
		text   bool
	}{
		{`{}`, 0, false}, {`{"candidates":[]}`, 0, false},
		{`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}`, 0, false},
		{`{"candidates":[{"content":{"parts":[{"text":"inlineData b64_json image_url"}]}}]}`, 0, true},
		{fmt.Sprintf(`{"response":{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/png","data":%q}},{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}}`, data, data), 2, false},
	} {
		s := InspectImageResponse([]byte(tc.body))
		if s.Images != tc.images || s.Text != tc.text {
			t.Fatalf("summary=%+v expected images=%d text=%v", s, tc.images, tc.text)
		}
	}
	if ValidInlineImage("image/jpeg", data) {
		t.Fatal("MIME mismatch accepted")
	}
	if ValidInlineImage("image/png", base64.StdEncoding.EncodeToString(buf.Bytes()[:40])) {
		t.Fatal("truncated image header accepted")
	}
}

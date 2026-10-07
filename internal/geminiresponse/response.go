// Package geminiresponse validates native Gemini generation outcomes before translation.
package geminiresponse

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type Error struct {
	Code       string
	Message    string
	Status     int
	Stop       bool
	NativeBody []byte
}

func (e *Error) Error() string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message, "type": "upstream_error"}})
	return string(b)
}
func (e *Error) StatusCode() int       { return e.Status }
func (e *Error) IsRequestScoped() bool { return e.Stop }
func (e *Error) IsRequestStop() bool   { return e.Stop }

type Summary struct {
	Answer, Thought, Image bool
	Block, Finish          string
	ErrorStatus            int
	ErrorMessage           string
	ErrorCode              string
	ErrorBody              []byte
}

func (s *Summary) Observe(body []byte) {
	r := gjson.ParseBytes(body)
	if r.IsArray() {
		for _, item := range r.Array() {
			s.Observe([]byte(item.Raw))
		}
		return
	}
	if inner := r.Get("response"); inner.IsObject() {
		r = inner
	}
	if object := r.Get("error"); object.IsObject() {
		s.ErrorStatus = int(object.Get("code").Int())
		if s.ErrorStatus < 400 || s.ErrorStatus > 599 {
			s.ErrorStatus = http.StatusBadGateway
		}
		s.ErrorMessage = object.Get("message").String()
		s.ErrorCode = object.Get("status").String()
		if code := object.Get("code"); code.Type == gjson.String {
			s.ErrorCode = code.String()
		}
		if s.ErrorCode == "" {
			s.ErrorCode = "upstream_error"
		}
		s.ErrorBody = []byte(r.Raw)
	}
	if block := r.Get("promptFeedback.blockReason").String(); block != "" && block != "BLOCKED_REASON_UNSPECIFIED" {
		s.Block = block
	}
	for _, candidate := range r.Get("candidates").Array() {
		if reason := candidate.Get("finishReason").String(); reason != "" {
			s.Finish = reason
			switch reason {
			case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
				s.Block = reason
			}
		}
		for _, part := range candidate.Get("content.parts").Array() {
			if strings.TrimSpace(part.Get("text").String()) != "" {
				if part.Get("thought").Bool() {
					s.Thought = true
				} else {
					s.Answer = true
				}
			}
			if strings.TrimSpace(part.Get("functionCall.name").String()) != "" {
				s.Answer = true
			}
			for _, key := range []string{"inlineData", "inline_data"} {
				image := part.Get(key)
				mime := image.Get("mimeType").String()
				if mime == "" {
					mime = image.Get("mime_type").String()
				}
				if strings.HasPrefix(mime, "image/") {
					if data, err := base64.StdEncoding.DecodeString(image.Get("data").String()); err == nil && len(data) > 0 {
						s.Image, s.Answer = true, true
					}
				}
			}
			if strings.TrimSpace(part.Get("fileData.fileUri").String()) != "" {
				s.Answer = true
			}
		}
	}
}

func (s Summary) Failure(requireImage, native bool) error {
	if s.ErrorStatus != 0 {
		return &Error{Code: s.ErrorCode, Message: s.ErrorMessage, Status: s.ErrorStatus, Stop: s.ErrorStatus < 500 && s.ErrorStatus != 429, NativeBody: s.ErrorBody}
	}
	if s.Block != "" {
		if native {
			return nil
		}
		return &Error{Code: "content_policy_block", Message: "Gemini content policy blocked the response (" + s.Block + ")", Status: 400, Stop: true}
	}
	if !s.Answer && s.Finish == "MAX_TOKENS" {
		if native {
			return nil
		}
		return &Error{Code: "upstream_output_limit", Message: "Gemini exhausted the output budget before a final answer; increase the output limit or reduce the thinking budget", Status: 502, Stop: true}
	}
	if !s.Answer || (requireImage && !s.Image) {
		return &Error{Code: "upstream_empty_response", Message: "Gemini upstream returned no usable generated content", Status: 502}
	}
	return nil
}

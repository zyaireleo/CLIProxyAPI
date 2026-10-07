package helps

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func geminiInputError(message string) error {
	return &geminiresponse.Error{Code: "invalid_request_error", Message: message, Status: http.StatusBadRequest, Stop: true}
}

// NormalizeGeminiGenerationInput preserves all history and signatures. It only
// normalizes valid JSON-string function arguments; unsupported input is explicit.
func NormalizeGeminiGenerationInput(payload []byte, path string) ([]byte, error) {
	contents := gjson.GetBytes(payload, path)
	if !contents.IsArray() || len(contents.Array()) == 0 {
		return nil, geminiInputError("request contains no usable Gemini contents")
	}
	usable := false
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() || len(parts.Array()) == 0 {
			return nil, geminiInputError("Gemini content parts must be an array")
		}
		turnUsable := false
		for pi, part := range parts.Array() {
			prefix := path + "." + strconv.Itoa(ci) + ".parts." + strconv.Itoa(pi)
			if strings.TrimSpace(part.Get("text").String()) != "" || part.Get("functionResponse").Exists() || part.Get("fileData").Exists() || part.Get("toolCall").Exists() || part.Get("toolResponse").Exists() || part.Get("executableCode").Exists() || part.Get("codeExecutionResult").Exists() {
				usable = true
				turnUsable = true
			}
			if call := part.Get("functionCall"); call.Exists() {
				usable = true
				turnUsable = true
				if strings.TrimSpace(call.Get("name").String()) == "" {
					return nil, geminiInputError("functionCall.name is required")
				}
				args := call.Get("args")
				if args.Type == gjson.String {
					if !gjson.Valid(args.String()) || !gjson.Parse(args.String()).IsObject() {
						return nil, geminiInputError("functionCall.args must contain a JSON object")
					}
					var err error
					payload, err = sjson.SetRawBytes(payload, prefix+".functionCall.args", []byte(args.String()))
					if err != nil {
						return nil, err
					}
				} else if args.Exists() && !args.IsObject() {
					return nil, geminiInputError("functionCall.args must be an object")
				}
			}
			if inline := part.Get("inlineData"); inline.Exists() {
				usable = true
				turnUsable = true
				encoded := inline.Get("data").String()
				if base64.StdEncoding.DecodedLen(len(encoded)) > 20<<20 {
					return nil, geminiInputError("inline image exceeds 20 MiB")
				}
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(data) == 0 {
					return nil, geminiInputError("inlineData.data must contain valid base64 bytes")
				}
			}
		}
		if !turnUsable {
			return nil, geminiInputError("Gemini history contains an empty content turn")
		}
	}
	if !usable {
		return nil, geminiInputError("request contains no usable Gemini contents")
	}
	last := contents.Array()[len(contents.Array())-1]
	if role := last.Get("role").String(); role == "model" || role == "assistant" {
		response := false
		for _, part := range last.Get("parts").Array() {
			response = response || part.Get("functionResponse").Exists()
		}
		if !response {
			return nil, geminiInputError("Gemini generation requires a final user message or function response")
		}
	}
	root := strings.TrimSuffix(path, "contents")
	functions, builtin := false, false
	for _, tool := range gjson.GetBytes(payload, root+"tools").Array() {
		functions = functions || len(tool.Get("functionDeclarations").Array()) > 0 || len(tool.Get("function_declarations").Array()) > 0
		for _, kind := range []string{"googleSearch", "google_search", "codeExecution", "code_execution", "urlContext", "url_context"} {
			builtin = builtin || tool.Get(kind).Exists()
		}
	}
	if functions && builtin {
		return sjson.SetBytes(payload, root+"toolConfig.includeServerSideToolInvocations", true)
	}
	return payload, nil
}

// ValidateGeminiClientFunctionArguments runs before translation can replace
// malformed compatibility arguments with an empty object.
func ValidateGeminiClientFunctionArguments(payload []byte) error {
	check := func(value gjson.Result) error {
		if !value.Exists() {
			return nil
		}
		if value.Type == gjson.String {
			if !gjson.Valid(value.String()) || !gjson.Parse(value.String()).IsObject() {
				return geminiInputError("Function arguments must contain a valid JSON object")
			}
			return nil
		}
		if !value.IsObject() {
			return geminiInputError("Function arguments must be a JSON object")
		}
		return nil
	}
	for _, message := range gjson.GetBytes(payload, "messages").Array() {
		for _, call := range message.Get("tool_calls").Array() {
			if err := check(call.Get("function.arguments")); err != nil {
				return err
			}
		}
		for _, part := range message.Get("content").Array() {
			if part.Get("type").String() == "tool_use" {
				if err := check(part.Get("input")); err != nil {
					return err
				}
			}
		}
	}
	for _, item := range gjson.GetBytes(payload, "input").Array() {
		if item.Get("type").String() == "function_call" {
			if err := check(item.Get("arguments")); err != nil {
				return err
			}
		}
	}
	return nil
}

// CheckGeminiContext uses a byte upper bound only to decide whether token
// counting is necessary; a countTokens result is required to reject history.
func CheckGeminiContext(payload []byte, limit int, count func() (int64, error)) error {
	if limit <= 0 || len(payload) < limit*3/4 {
		return nil
	}
	n, err := count()
	if err != nil {
		return nil
	}
	if n > int64(limit) {
		return &geminiresponse.Error{Code: "context_length_exceeded", Message: fmt.Sprintf("Gemini input has %d tokens; model limit is %d", n, limit), Status: 400, Stop: true}
	}
	return nil
}

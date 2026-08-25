package claude

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeAssistantPrefillSupport int

const (
	claudeAssistantPrefillUnknown claudeAssistantPrefillSupport = iota
	claudeAssistantPrefillSupported
	claudeAssistantPrefillUnsupported
)

func claudeAssistantPrefillSupportForModel(modelName string) claudeAssistantPrefillSupport {
	normalized, ok := normalizeKnownClaudeModelName(modelName)
	if !ok {
		return claudeAssistantPrefillUnknown
	}

	versions := claudeModelVersionNumbers(normalized)
	if len(versions) == 0 {
		return claudeAssistantPrefillUnknown
	}

	major := versions[0]
	if major > 4 {
		return claudeAssistantPrefillUnsupported
	}
	if major < 4 {
		return claudeAssistantPrefillSupported
	}
	minor := -1
	for _, version := range versions[1:] {
		if version < 100 {
			minor = version
			break
		}
	}
	if minor >= 6 {
		return claudeAssistantPrefillUnsupported
	}
	return claudeAssistantPrefillSupported
}

func normalizeKnownClaudeModelName(modelName string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if strings.HasPrefix(normalized, "claude-") {
		return normalized, true
	}

	for _, prefix := range []string{"anthropic/", "bedrock/", "vertex_ai/"} {
		if strings.HasPrefix(normalized, prefix) {
			normalized = strings.TrimPrefix(normalized, prefix)
			break
		}
	}
	if strings.HasPrefix(normalized, "claude-") {
		return normalized, true
	}

	for _, prefix := range []string{
		"anthropic.",
		"us.anthropic.",
		"eu.anthropic.",
		"apac.anthropic.",
		"jp.anthropic.",
		"global.anthropic.",
	} {
		if strings.HasPrefix(normalized, prefix) {
			normalized = strings.TrimPrefix(normalized, prefix)
			return normalized, strings.HasPrefix(normalized, "claude-")
		}
	}

	const vertexPublisherPath = "publishers/anthropic/models/"
	if index := strings.Index(normalized, vertexPublisherPath); index >= 0 {
		unwrapped := normalized[index+len(vertexPublisherPath):]
		return unwrapped, strings.HasPrefix(unwrapped, "claude-")
	}

	return "", false
}

func claudeModelVersionNumbers(modelName string) []int {
	versions := make([]int, 0, 2)
	start := -1
	for index, r := range modelName {
		if unicode.IsDigit(r) {
			if start == -1 {
				start = index
			}
			continue
		}
		if start != -1 {
			if value, errValue := strconv.Atoi(modelName[start:index]); errValue == nil {
				versions = append(versions, value)
			}
			start = -1
		}
	}
	if start != -1 {
		if value, errValue := strconv.Atoi(modelName[start:]); errValue == nil {
			versions = append(versions, value)
		}
	}
	return versions
}

func applyUnsupportedClaudeAssistantPrefillFallback(modelName string, rawJSON []byte) []byte {
	if claudeAssistantPrefillSupportForModel(modelName) != claudeAssistantPrefillUnsupported {
		return rawJSON
	}

	messagesResult := gjson.GetBytes(rawJSON, "messages")
	if !messagesResult.IsArray() {
		return rawJSON
	}
	messages := messagesResult.Array()
	if len(messages) == 0 {
		return rawJSON
	}

	tailMessage := messages[len(messages)-1]
	if tailMessage.Get("role").String() != "assistant" {
		return rawJSON
	}

	contentResult := tailMessage.Get("content")
	if isBlankClaudeAssistantPrefillContent(contentResult) {
		if hasEarlierClaudeUserMessage(messages[:len(messages)-1]) {
			out, errDelete := sjson.DeleteBytes(rawJSON, "messages."+strconv.Itoa(len(messages)-1))
			if errDelete == nil {
				return out
			}
		}
		return rawJSON
	}

	if !isPureTextClaudeAssistantPrefillContent(contentResult) {
		return rawJSON
	}

	out, errAppend := sjson.SetRawBytes(rawJSON, "messages.-1", []byte(`{"role":"user","content":"Continue."}`))
	if errAppend != nil {
		return rawJSON
	}
	return out
}

func hasEarlierClaudeUserMessage(messages []gjson.Result) bool {
	for _, message := range messages {
		if message.Get("role").String() != "user" {
			continue
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			if strings.TrimSpace(content.String()) != "" {
				return true
			}
			continue
		}
		if !content.IsArray() {
			continue
		}
		for _, block := range content.Array() {
			blockType := block.Get("type")
			if blockType.Type != gjson.String {
				continue
			}
			if blockType.String() != "text" {
				return true
			}
			text := block.Get("text")
			if text.Type == gjson.String && strings.TrimSpace(text.String()) != "" {
				return true
			}
		}
	}
	return false
}

func isBlankClaudeAssistantPrefillContent(content gjson.Result) bool {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if !content.Exists() {
		return true
	}
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if part.Get("type").String() != "text" {
			return false
		}
		textResult := part.Get("text")
		if textResult.Type != gjson.String {
			return false
		}
		if strings.TrimSpace(textResult.String()) != "" {
			return false
		}
	}
	return true
}

func isPureTextClaudeAssistantPrefillContent(content gjson.Result) bool {
	if content.Type == gjson.String {
		return true
	}
	if !content.IsArray() {
		return false
	}
	parts := content.Array()
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part.Get("type").String() != "text" {
			return false
		}
		if part.Get("text").Type != gjson.String {
			return false
		}
	}
	return true
}

package helps

import (
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/tidwall/gjson"
)

func TestGeminiInputPreservesHistoryAndSignature(t *testing.T) {
	input := []byte("{\"request\":{\"contents\":[{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"read\",\"args\":\"{\\\"id\\\":1}\"},\"thoughtSignature\":\"original-signature\"}]},{\"role\":\"user\",\"parts\":[{\"functionResponse\":{\"name\":\"read\",\"response\":{\"ok\":true}}}]}],\"tools\":[{\"googleSearch\":{}},{\"functionDeclarations\":[{\"name\":\"read\"}]}]}}")
	out, err := NormalizeGeminiGenerationInput(input, "request.contents")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.contents.#").Int(); got != 2 {
		t.Fatalf("history size=%d", got)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != "original-signature" {
		t.Fatalf("lost signature: %s", got)
	}
	if !gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.args").IsObject() {
		t.Fatal("arguments were not normalized")
	}
	if got := gjson.GetBytes(out, "request.tools.#").Int(); got != 2 {
		t.Fatal("tool combination lost")
	}
}

func TestGeminiInvalidInputStopsRecovery(t *testing.T) {
	for _, input := range []string{
		"{\"request\":{\"contents\":[]}}",
		"{\"request\":{\"contents\":[{\"role\":\"model\",\"parts\":[{\"text\":\"prefill\"}]}]}}",
		"{\"request\":{\"contents\":[{\"role\":\"user\",\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":\"invalid\"}}]}]}}",
	} {
		_, err := NormalizeGeminiGenerationInput([]byte(input), "request.contents")
		var typed *geminiresponse.Error
		if !errors.As(err, &typed) || typed.Status != 400 || !typed.Stop {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	if err := ValidateGeminiClientFunctionArguments([]byte("{\"messages\":[{\"role\":\"assistant\",\"tool_calls\":[{\"function\":{\"arguments\":\"[1]\"}}]}]}")); err == nil {
		t.Fatal("invalid compatibility arguments accepted")
	}
}

func TestGeminiContextCountDoesNotTruncate(t *testing.T) {
	input := make([]byte, 1000)
	calls := 0
	err := CheckGeminiContext(input, 100, func() (int64, error) { calls++; return 101, nil })
	if err == nil || calls != 1 {
		t.Fatalf("count validation missing: %v calls=%d", err, calls)
	}
	if err := CheckGeminiContext(input, 100, func() (int64, error) { return 99, nil }); err != nil {
		t.Fatal(err)
	}
	if err := CheckGeminiContext([]byte("small"), 100, func() (int64, error) { t.Fatal("small request counted"); return 0, nil }); err != nil {
		t.Fatal(err)
	}
}

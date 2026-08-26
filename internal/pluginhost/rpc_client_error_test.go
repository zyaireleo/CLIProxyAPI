package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type staticEnvelopePluginClient struct {
	raw []byte
}

func (c staticEnvelopePluginClient) Call(context.Context, string, []byte) ([]byte, error) {
	return c.raw, nil
}

func (c staticEnvelopePluginClient) Shutdown() {}

func TestDecodeEnvelopeResultPreservesPluginHTTPStatus(t *testing.T) {
	body := []byte(`{"error":{"code":"model_cooldown"}}`)
	_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:            "plugin_error",
			Message:         "license required",
			HTTPStatus:      http.StatusForbidden,
			ResponseHeaders: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Retry-After": {"17"}},
			ResponseBody:    body,
		},
	})
	if errDecode == nil {
		t.Fatal("decodeEnvelopeResult returned nil error")
	}
	if got := errDecode.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errDecode.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errDecode)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
	responseProvider, ok := errDecode.(interface {
		ResponseHeaders() http.Header
		ResponseBody() []byte
	})
	if !ok {
		t.Fatalf("error %T does not expose direct response", errDecode)
	}
	if got := responseProvider.ResponseHeaders().Get("Retry-After"); got != "17" {
		t.Fatalf("Retry-After = %q, want 17", got)
	}
	if got := string(responseProvider.ResponseBody()); got != string(body) {
		t.Fatalf("response body = %q, want %q", got, body)
	}
}

func TestDecodeEnvelopeResultSanitizesPluginDirectResponse(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), maxPluginErrorResponseBody+1)
	_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Message:         "bad plugin response",
			HTTPStatus:      http.StatusOK,
			ResponseHeaders: http.Header{"Location": {"https://example.test"}, "Retry-After": {"not-a-number"}, "Content-Type": {"text/html"}},
			ResponseBody:    oversized,
		},
	})
	pluginErr, ok := errDecode.(rpcError)
	if !ok {
		t.Fatalf("error type = %T", errDecode)
	}
	if pluginErr.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", pluginErr.StatusCode())
	}
	if len(pluginErr.ResponseHeaders()) != 0 || len(pluginErr.ResponseBody()) != 0 {
		t.Fatalf("unsafe direct response survived: headers=%v body=%d", pluginErr.ResponseHeaders(), len(pluginErr.ResponseBody()))
	}
}

func TestCallPluginReturnsPluginErrorWithoutMethodWrapper(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       "plugin_error",
			Message:    "license required",
			HTTPStatus: http.StatusForbidden,
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}
	_, errCall := callPlugin[rpcEmptyResponse](context.Background(), staticEnvelopePluginClient{raw: raw}, pluginabi.MethodExecutorExecuteStream, rpcEmptyResponse{})
	if errCall == nil {
		t.Fatal("callPlugin returned nil error")
	}
	if got := errCall.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errCall.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errCall)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestIsPluginErrorEnvelopeAcceptsNonzeroReturnEnvelope(t *testing.T) {
	raw := marshalRPCError("plugin_error", "upstream failed")
	if !isPluginErrorEnvelope(raw) {
		t.Fatalf("isPluginErrorEnvelope(%s) = false, want true", raw)
	}
	if isPluginErrorEnvelope([]byte(`not json`)) {
		t.Fatal("isPluginErrorEnvelope accepted invalid JSON")
	}
}

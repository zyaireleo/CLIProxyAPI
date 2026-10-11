package executor

import (
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestAntigravityGeminiQuotaAndCapacityClassification(t *testing.T) {
	body := []byte("{\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.RetryInfo\",\"retryDelay\":\"3600s\"}]}}")
	err := newAntigravityStatusErr(429, body, http.Header{"Retry-After": {"30"}})
	if err.retryAfter == nil || *err.retryAfter != time.Hour || err.credentialScoped {
		t.Fatalf("quota classification: %+v", err)
	}
	global := []byte("{\"error\":{\"details\":[{\"metadata\":{\"quota_scope\":\"credential\"}}]}}")
	if !newAntigravityStatusErr(429, global).credentialScoped {
		t.Fatal("explicit credential scope was lost")
	}
	capacity := []byte("{\"error\":{\"code\":429,\"message\":\"capacity unavailable\",\"details\":[{\"reason\":\"MODEL_CAPACITY_EXHAUSTED\"}]}}")
	err = newAntigravityStatusErr(429, capacity)
	if err.code != 503 || gjson.Get(err.msg, "error.code").String() != "upstream_capacity_exhausted" {
		t.Fatalf("capacity classification: %+v", err)
	}
}

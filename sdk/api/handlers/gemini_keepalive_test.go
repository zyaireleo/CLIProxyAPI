package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestGeminiNonStreamingFailureCanSetHTTPStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	h := &BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{NonStreamKeepAliveInterval: 1}}
	stop := h.StartNonStreamingKeepAlive(c, context.Background(), "gemini-pro-agent")
	defer stop()
	time.Sleep(1100 * time.Millisecond)
	if c.Writer.Written() {
		t.Fatal("Gemini non-streaming keepalive committed a success response")
	}
	c.JSON(400, gin.H{"error": gin.H{"code": "content_policy_block"}})
	if rec.Code != 400 {
		t.Fatalf("policy failure status=%d", rec.Code)
	}
}

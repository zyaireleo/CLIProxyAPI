package management

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed native_quota_assets/index.html
var nativeQuotaHTML string

var nativeQuotaCSP = "default-src 'none'; connect-src 'self'; img-src 'none'; " +
	"script-src " + nativeQuotaInlineHash("script") + "; style-src " + nativeQuotaInlineHash("style") +
	"; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

func nativeQuotaInlineHash(tag string) string {
	_, body, found := strings.Cut(nativeQuotaHTML, "<"+tag+">")
	if !found {
		return "'none'"
	}
	body, _, found = strings.Cut(body, "</"+tag+">")
	if !found {
		return "'none'"
	}
	sum := sha256.Sum256([]byte(body))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// GetNativeQuotaUI serves an immutable, secret-free shell. It does not inspect
// request auth headers, expand templates, read credentials, or download assets.
func (h *Handler) GetNativeQuotaUI(c *gin.Context) {
	nativeQuotaHeaders(c)
	h.mu.Lock()
	disabled := h.cfg == nil || h.cfg.Home.Enabled || h.cfg.RemoteManagement.DisableControlPanel
	h.mu.Unlock()
	if disabled {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Security-Policy", nativeQuotaCSP)
	c.Header("X-Frame-Options", "DENY")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(nativeQuotaHTML))
}

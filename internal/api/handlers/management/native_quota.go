package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const nativeQuotaBodyLimit = 64 << 10

// nativeQuotaAccountView adds explicit unknown age to the credential-free engine view.
type nativeQuotaAccountView struct {
	nativequota.View
	AgeSeconds *float64 `json:"age_seconds"`
}

func nativeQuotaHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
}

// GetNativeQuotaStatus returns only engine snapshots, never auth records or metadata.
func (h *Handler) GetNativeQuotaStatus(c *gin.Context) {
	nativeQuotaHeaders(c)
	page, ok := nativeQuotaPageNumber(c, "page", 1, 1000000)
	if !ok {
		return
	}
	size, ok := nativeQuotaPageNumber(c, "page_size", 50, 200)
	if !ok {
		return
	}
	for key, values := range c.Request.URL.Query() {
		switch key {
		case "page", "page_size", "provider", "state", "auth_id":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown_filter"})
			return
		}
		if len(values) != 1 || len(values[0]) > 512 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_filter"})
			return
		}
	}
	provider, state, id := c.Query("provider"), c.Query("state"), c.Query("auth_id")
	if provider != "" && provider != "antigravity" && provider != "codex" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_provider"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil || h.authManager == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	cfg, errConfig := nativeQuotaEffectiveConfig(h.cfg.NativeQuota)
	engine := h.authManager.NativeQuota()
	h.mu.Unlock()
	if engine == nil || errConfig != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	now := time.Now().UTC()
	views := engine.Views(now)
	filtered := make([]nativeQuotaAccountView, 0, len(views))
	for _, view := range views {
		if provider != "" && view.Provider != provider || state != "" && view.State != state || id != "" && view.AuthID != id {
			continue
		}
		var age *float64
		if !view.ObservedAt.IsZero() {
			seconds := max(0, now.Sub(view.ObservedAt).Seconds())
			age = &seconds
		}
		filtered = append(filtered, nativeQuotaAccountView{View: view, AgeSeconds: age})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].AuthID < filtered[j].AuthID })
	total := len(filtered)
	start := min((page-1)*size, total)
	end := min(start+size, total)
	// Return events for displayed IDs only; the UI cannot accidentally disclose other pages.
	visible := make(map[string]struct{}, end-start)
	for _, view := range filtered[start:end] {
		visible[view.AuthID] = struct{}{}
	}
	events := make([]nativequota.Event, 0)
	for _, event := range engine.Events() {
		if _, exists := visible[event.AuthID]; exists {
			events = append(events, event)
		}
	}
	if len(events) > 100 {
		events = events[len(events)-100:]
	}
	c.JSON(http.StatusOK, gin.H{
		"config": cfg, "accounts": filtered[start:end], "events": events,
		"page": page, "page_size": size, "total": total, "has_more": end < total, "server_time": now,
		"runtime_mode": engine.Config().Mode,
	})
}

// PostNativeQuotaRefresh queues a known exact ID; user input is never interpreted as a URL.
func (h *Handler) PostNativeQuotaRefresh(c *gin.Context) {
	nativeQuotaHeaders(c)
	var body struct {
		AuthID string `json:"auth_id"`
	}
	if !nativeQuotaDecode(c, &body) {
		return
	}
	if body.AuthID == "" || len(body.AuthID) > 512 || strings.TrimSpace(body.AuthID) != body.AuthID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_auth_id"})
		return
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	auth, exists := manager.GetByID(body.AuthID)
	if !exists || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_id_not_found"})
		return
	}
	engine := manager.NativeQuota()
	if engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	cfg := engine.Config()
	if cfg.Mode == nativequota.ModeOff {
		c.JSON(http.StatusConflict, gin.H{"error": "native_quota_off"})
		return
	}
	if auth.Disabled || auth.Status == "disabled" {
		c.JSON(http.StatusConflict, gin.H{"error": "auth_disabled"})
		return
	}
	enabled := false
	for _, provider := range cfg.Providers {
		if auth.Provider == provider {
			enabled = true
			break
		}
	}
	if !enabled || auth.AuthKind() == coreauth.AuthKindAPIKey || auth.Provider != "antigravity" && auth.Provider != "codex" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_provider"})
		return
	}
	queued := engine.RequestRefresh(body.AuthID, "management")
	c.JSON(http.StatusAccepted, gin.H{"auth_id": body.AuthID, "queued": queued, "status": "accepted_or_coalesced"})
}

func nativeQuotaPageNumber(c *gin.Context, key string, fallback, upper int) (int, bool) {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return fallback, true
	}
	if len(values) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_pagination", "field": key})
		return 0, false
	}
	n, err := strconv.Atoi(values[0])
	if err != nil || n < 1 || n > upper {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_pagination", "field": key})
		return 0, false
	}
	return n, true
}

// nativeQuotaDecode rejects oversized, duplicate-field, unknown-field and trailing JSON.
// Error details intentionally do not echo request bodies or credentials.
func nativeQuotaDecode(c *gin.Context, target any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, nativeQuotaBodyLimit))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return false
	}
	check := json.NewDecoder(bytes.NewReader(data))
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' || nativeQuotaCheckJSON(check, 0) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return false
	}
	return true
}

func nativeQuotaCheckJSON(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("invalid JSON depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, errKey := decoder.Token()
			key, ok := keyToken.(string)
			if errKey != nil || !ok || seen[key] {
				return errors.New("duplicate or invalid JSON key")
			}
			seen[key] = true
			if errValue := nativeQuotaCheckJSON(decoder, depth+1); errValue != nil {
				return errValue
			}
		}
		end, errEnd := decoder.Token()
		if errEnd != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case json.Delim('['):
		for decoder.More() {
			if errValue := nativeQuotaCheckJSON(decoder, depth+1); errValue != nil {
				return errValue
			}
		}
		end, errEnd := decoder.Token()
		if errEnd != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	}
	return nil
}

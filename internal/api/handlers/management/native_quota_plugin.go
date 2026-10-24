package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// PostNativeQuotaPluginStop is an explicit lifecycle handoff, not a migration.
// It cannot alter plugin configuration or remove credentials, binaries, or caches.
func (h *Handler) PostNativeQuotaPluginStop(c *gin.Context) {
	nativeQuotaHeaders(c)
	var body struct {
		ID string `json:"id"`
	}
	if !nativeQuotaDecode(c, &body) {
		return
	}
	if _, allowed := nativeQuotaMigrationSources[body.ID]; !allowed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_plugin_id"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil || h.pluginHost == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin_host_unavailable"})
		return
	}
	plugin, exists := h.cfg.Plugins.Configs[body.ID]
	host := h.pluginHost
	h.mu.Unlock()
	if !exists || plugin.Enabled == nil || *plugin.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "explicit_plugin_disable_required"})
		return
	}
	if errStop := host.StopPlugin(c.Request.Context(), body.ID); errStop != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": "plugin_stop_incomplete", "stopped": false,
			"message": "Shutdown was not confirmed. Keep native quota out of enforce and verify lifecycle completion before retrying.",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": body.ID, "stopped": true, "config_changed": false})
}

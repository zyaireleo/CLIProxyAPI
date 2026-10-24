package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// This is a versioned, user-supplied preview format, not a legacy state importer.
// Only reviewed identities and explicitly allowlisted configuration are accepted.
var nativeQuotaMigrationSources = map[string]struct{ Version, Provider string }{
	"antigravity-priority":  {"1.2.20", "antigravity"},
	"codex-quota-scheduler": {"0.2.1", "codex"},
}

type nativeQuotaMigrationRequest struct {
	SchemaVersion int    `json:"schema_version"`
	PluginID      string `json:"plugin_id"`
	PluginVersion string `json:"plugin_version"`
	UserConfig    struct {
		RefreshInterval string `json:"refresh_interval"`
	} `json:"user_config"`
	CredentialMappings []struct {
		CredentialID string `json:"credential_id"`
		AuthID       string `json:"auth_id"`
	} `json:"credential_mappings"`
}

type nativeQuotaMigrationAccount struct {
	CredentialID    string `json:"credential_id"`
	AuthID          string `json:"auth_id"`
	Provider        string `json:"provider"`
	CurrentDisabled bool   `json:"current_disabled"`
	CurrentPriority *int   `json:"current_priority"`
	Proposal        string `json:"proposal"`
}

// PostNativeQuotaMigrationPreview never writes config, auth files, or plugin state.
func (h *Handler) PostNativeQuotaMigrationPreview(c *gin.Context) {
	nativeQuotaHeaders(c)
	var body nativeQuotaMigrationRequest
	if !nativeQuotaDecode(c, &body) {
		return
	}
	source, supported := nativeQuotaMigrationSources[body.PluginID]
	if !supported || body.PluginVersion != source.Version || body.SchemaVersion != 1 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "unsupported_migration_source"})
		return
	}
	if len(body.CredentialMappings) == 0 || len(body.CredentialMappings) > 200 || body.UserConfig.RefreshInterval == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "explicit_mapping_and_user_config_required"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil || h.authManager == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	proposed, errConfig := nativeQuotaEffectiveConfig(h.cfg.NativeQuota)
	manager := h.authManager
	h.mu.Unlock()
	if errConfig != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_current_config"})
		return
	}
	proposed.Mode = "shadow"
	if source.Provider == "antigravity" {
		proposed.AntigravityInterval = body.UserConfig.RefreshInterval
	} else {
		proposed.CodexInterval = body.UserConfig.RefreshInterval
	}
	if errValidate := proposed.Validate(); errValidate != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_user_config"})
		return
	}
	seenCredentials, seenAuth := make(map[string]bool), make(map[string]bool)
	accounts := make([]nativeQuotaMigrationAccount, 0, len(body.CredentialMappings))
	for _, mapping := range body.CredentialMappings {
		if mapping.CredentialID == "" || mapping.AuthID == "" || len(mapping.CredentialID) > 512 || len(mapping.AuthID) > 512 ||
			strings.TrimSpace(mapping.AuthID) != mapping.AuthID || seenCredentials[mapping.CredentialID] || seenAuth[mapping.AuthID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_or_duplicate_mapping"})
			return
		}
		seenCredentials[mapping.CredentialID], seenAuth[mapping.AuthID] = true, true
		auth, exists := manager.GetByID(mapping.AuthID)
		if !exists || auth == nil || auth.Provider != source.Provider {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mapping_auth_not_found_or_provider_mismatch"})
			return
		}
		priority := 0
		priorityValue := &priority
		if raw := strings.TrimSpace(auth.Attributes["priority"]); raw != "" {
			var errPriority error
			priority, errPriority = strconv.Atoi(raw)
			if errPriority != nil {
				priorityValue = nil
			}
		}
		accounts = append(accounts, nativeQuotaMigrationAccount{
			CredentialID: mapping.CredentialID, AuthID: mapping.AuthID, Provider: auth.Provider,
			CurrentDisabled: auth.Disabled || auth.Status == "disabled", CurrentPriority: priorityValue,
			Proposal: "preserve_disabled_and_priority; manual_provenance_review_required",
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"dry_run": true, "execution_allowed": false, "source_verification": "user_supplied_version_not_live_verified",
		"plugin_id": body.PluginID, "plugin_version": body.PluginVersion, "accounts": accounts,
		"proposed_config": proposed, "allowlisted_user_config": []string{"refresh_interval"},
		"warnings": []string{
			"No plugin, credential, priority, disabled flag, or native quota config was changed.",
			"Priorities 96 through 100 do not prove plugin ownership. Missing plugin state never justifies enabling an account.",
			"Verify actual plugin versions, stop legacy schedulers, back up state, and complete lifecycle handoff tests before enforce.",
		},
	})
}

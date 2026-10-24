package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

// Every field is required on PUT. No omission silently resets a safety limit.
var nativeQuotaConfigFields = []string{
	"mode", "providers", "refresh-concurrency", "antigravity-interval", "codex-interval",
	"refresh-min-interval", "max-wait", "max-in-flight", "unknown-max-in-flight", "queue-limit",
}

var (
	errNativeQuotaSave     = errors.New("config_save_failed")
	errNativeQuotaApply    = errors.New("config_apply_failed")
	errNativeQuotaChanged  = errors.New("config_changed_retry")
	errNativeQuotaRollback = errors.New("config_rollback_failed")
)

// PutNativeQuotaConfig validates a complete replacement and publishes only a committed config.
func (h *Handler) PutNativeQuotaConfig(c *gin.Context) {
	nativeQuotaHeaders(c)
	var fields map[string]json.RawMessage
	if !nativeQuotaDecode(c, &fields) {
		return
	}
	if len(fields) != len(nativeQuotaConfigFields) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "complete_config_required", "fields": nativeQuotaConfigFields})
		return
	}
	for _, key := range nativeQuotaConfigFields {
		value, exists := fields[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "complete_config_required", "fields": nativeQuotaConfigFields})
			return
		}
	}
	data, _ := json.Marshal(fields)
	var next config.NativeQuotaConfig
	if json.Unmarshal(data, &next) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_config"})
		return
	}
	if errValidate := nativeQuotaValidateComplete(next); errValidate != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config"})
		return
	}

	// Match the existing reload lock order so an older management reload cannot
	// overwrite a freshly committed native quota configuration.
	h.reloadMu.Lock()
	h.mu.Lock()
	if h.cfg == nil || h.authManager == nil {
		h.mu.Unlock()
		h.reloadMu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native_quota_unavailable"})
		return
	}
	candidate := h.cfg.CloneForRuntime()
	candidate.NativeQuota = next
	if errValidate := candidate.ValidateNativeQuota(); errValidate != nil {
		h.mu.Unlock()
		h.reloadMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "native_quota_conflict", "message": "Disable legacy quota schedulers and Home mode before enforce; no plugins were changed."})
		return
	}
	manager := h.authManager
	if errValidate := manager.ValidateNativeQuotaConfig(next); errValidate != nil {
		h.mu.Unlock()
		h.reloadMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "native_quota_conflict", "message": "Runtime handoff is not ready; no plugins were changed."})
		return
	}
	ctx := context.WithoutCancel(c.Request.Context())
	errSave := nativeQuotaSaveTransaction(h.configFilePath, candidate, func() error {
		return manager.ApplyNativeQuotaConfig(ctx, next)
	})
	if errSave != nil {
		h.mu.Unlock()
		h.reloadMu.Unlock()
		status := http.StatusInternalServerError
		if errors.Is(errSave, errNativeQuotaChanged) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": errSave.Error(), "previous_state_preserved": !errors.Is(errSave, errNativeQuotaRollback)})
		return
	}
	h.cfg = candidate
	snapshot := h.reloadSnapshotConfigLocked()
	h.appliedReloadGeneration = snapshot.generation
	h.mu.Unlock()
	h.reloadMu.Unlock()
	h.reloadConfigAfterManagementSave(ctx, snapshot)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config": next})
}

// nativeQuotaEffectiveConfig makes implicit file defaults explicit for safe full PUTs.
// It is a response-only conversion and does not write or activate configuration.
func nativeQuotaEffectiveConfig(c config.NativeQuotaConfig) (config.NativeQuotaConfig, error) {
	r, err := c.RuntimeConfig()
	if err != nil {
		return config.NativeQuotaConfig{}, err
	}
	return config.NativeQuotaConfig{
		Mode: string(r.Mode), Providers: append([]string{}, r.Providers...), RefreshConcurrency: r.RefreshConcurrency,
		AntigravityInterval: r.AntigravityInterval.String(), CodexInterval: r.CodexInterval.String(),
		RefreshMinInterval: r.RefreshMinInterval.String(), MaxWait: r.MaxWait.String(),
		MaxInFlight: r.MaxInFlight, UnknownMaxInFlight: r.UnknownMaxInFlight, QueueLimit: r.QueueLimit,
	}, nil
}

func nativeQuotaValidateComplete(c config.NativeQuotaConfig) error {
	if c.Mode == "" || c.Providers == nil || c.AntigravityInterval == "" || c.CodexInterval == "" ||
		c.RefreshMinInterval == "" || c.MaxWait == "" || c.RefreshConcurrency < 1 || c.MaxInFlight < 1 ||
		c.UnknownMaxInFlight < 1 || c.QueueLimit < 1 {
		return errors.New("complete_config_required")
	}
	return c.Validate()
}

// nativeQuotaSaveTransaction reuses the existing comment-preserving serializer,
// but writes only a private staged file until the entire document is ready.
// Apply must preserve runtime state on error, as required by the manager contract.
func nativeQuotaSaveTransaction(path string, next *config.Config, apply func() error) error {
	info, errStat := os.Lstat(path)
	if errStat != nil || !info.Mode().IsRegular() {
		return errNativeQuotaSave
	}
	original, errRead := os.ReadFile(path)
	if errRead != nil {
		return errNativeQuotaSave
	}
	staged, errStage := nativeQuotaStageFile(path, original, info.Mode().Perm())
	if errStage != nil {
		return errNativeQuotaSave
	}
	defer func() { _ = os.Remove(staged) }()
	if errSave := config.SaveConfigPreserveComments(staged, next); errSave != nil {
		return errNativeQuotaSave
	}
	f, errOpen := os.OpenFile(staged, os.O_RDWR, info.Mode().Perm())
	if errOpen != nil {
		return errNativeQuotaSave
	}
	errSync := f.Sync()
	errClose := f.Close()
	if errSync != nil || errClose != nil {
		return errNativeQuotaSave
	}
	written, errReadStage := os.ReadFile(staged)
	if errReadStage != nil {
		return errNativeQuotaSave
	}
	// Refuse serialization that changes policy semantics (notably [] versus null
	// provider coverage). A later watcher must see the same policy we apply here.
	var persisted config.Config
	if errDecode := yaml.Unmarshal(written, &persisted); errDecode != nil {
		return errNativeQuotaSave
	}
	wantRuntime, errWant := next.NativeQuota.RuntimeConfig()
	gotRuntime, errGot := persisted.NativeQuota.RuntimeConfig()
	if errWant != nil || errGot != nil || !reflect.DeepEqual(wantRuntime, gotRuntime) {
		return errNativeQuotaSave
	}
	// Prepare restoration before committing, so a failed apply does not need a
	// fresh allocation or write of the previous config on a potentially full disk.
	backup, errBackup := nativeQuotaStageFile(path, original, info.Mode().Perm())
	if errBackup != nil {
		return errNativeQuotaSave
	}
	defer func() { _ = os.Remove(backup) }()
	current, errCurrent := os.ReadFile(path)
	if errCurrent != nil || !bytes.Equal(current, original) {
		return errNativeQuotaChanged
	}
	if errRename := os.Rename(staged, path); errRename != nil {
		return errNativeQuotaSave
	}
	if errApply := apply(); errApply != nil {
		current, errCurrent = os.ReadFile(path)
		if errCurrent != nil || !bytes.Equal(current, written) {
			return errNativeQuotaRollback
		}
		if errRestore := os.Rename(backup, path); errRestore != nil {
			return errNativeQuotaRollback
		}
		return errNativeQuotaApply
	}
	return nil
}

func nativeQuotaStageFile(path string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".native-quota-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(name) }
	if _, errWrite := f.Write(data); errWrite != nil {
		cleanup()
		return "", errWrite
	}
	if errChmod := f.Chmod(mode); errChmod != nil {
		cleanup()
		return "", errChmod
	}
	if errSync := f.Sync(); errSync != nil {
		cleanup()
		return "", errSync
	}
	if errClose := f.Close(); errClose != nil {
		_ = os.Remove(name)
		return "", errClose
	}
	return name, nil
}

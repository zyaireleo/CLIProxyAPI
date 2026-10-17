package cliproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

var antigravityCatalogSlots = make(chan struct{}, 4)

type antigravityCatalogSnapshot struct {
	Route   string       `json:"route"`
	Models  []*ModelInfo `json:"models"`
	Success time.Time    `json:"success"`
}

type antigravityCatalogState struct {
	antigravityCatalogSnapshot
	epoch    uint64
	next     time.Time
	pending  bool
	failures int
	settings config.AntigravityModelsConfig
}

func catalogRoute(a *coreauth.Auth, cfg *config.Config) string {
	proxy := a.ProxyURL
	if proxy == "" {
		proxy = cfg.ProxyURL
	}
	// Stable across access-token refreshes; changing the credential or route invalidates snapshots.
	key, _ := json.Marshal([]any{a.ID, a.Metadata["refresh_token"], a.Metadata["project_id"], antigravityModelBaseURLs(a), proxy})
	return fmt.Sprintf("%x", sha256.Sum256(key))
}

func catalogSnapshotPath(cfg *config.Config, id string) string {
	if cfg.AuthDir == "" {
		return ""
	}
	return filepath.Join(cfg.AuthDir, ".antigravity-models", fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

// The caller holds cfgMu before agCatalogMu throughout publication.
func (s *Service) catalogStateLocked(a *coreauth.Auth, cfg *config.Config) *antigravityCatalogState {
	if s.agCatalogs == nil {
		s.agCatalogs = make(map[string]*antigravityCatalogState)
	}
	route := catalogRoute(a, cfg)
	state := s.agCatalogs[a.ID]
	if state != nil && state.Route == route && state.epoch == a.RegistrationEpoch {
		return state
	}
	state = &antigravityCatalogState{antigravityCatalogSnapshot: antigravityCatalogSnapshot{Route: route}, epoch: a.RegistrationEpoch}
	if data, err := os.ReadFile(catalogSnapshotPath(cfg, a.ID)); err == nil {
		var saved antigravityCatalogSnapshot
		if json.Unmarshal(data, &saved) == nil && saved.Route == route && !saved.Success.IsZero() && saved.Models != nil {
			state.antigravityCatalogSnapshot = saved
			known := make(map[string]bool)
			for _, m := range registry.GetAntigravityModels() {
				known[m.ID] = true
			}
			for _, m := range state.Models {
				if m != nil && !known[m.ID] {
					m.UserDefined = true
				}
			}
		}
	}
	s.agCatalogs[a.ID] = state
	return state
}

func catalogModels(a *coreauth.Auth, cfg *config.Config, state *antigravityCatalogState) []*ModelInfo {
	models := registry.GetAntigravityModels()
	if cfg.AntigravityModels.DiscoveryEnabled && !state.Success.IsZero() {
		models = state.Models
	}
	byID := make(map[string]*ModelInfo, len(models))
	for _, m := range models {
		if m != nil {
			clone := *m
			byID[m.ID] = &clone
		}
	}
	for _, extra := range cfg.AntigravityModels.AdditionalModels {
		allowed := len(extra.AuthIDs) == 0
		for _, id := range extra.AuthIDs {
			if id == a.ID {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		m := byID[extra.ID]
		if m == nil {
			m = &ModelInfo{ID: extra.ID, Object: "model", Type: "antigravity", OwnedBy: "google", UserDefined: true}
			for _, known := range registry.GetAntigravityModels() {
				if known.ID == extra.ID {
					clone := *known
					m = &clone
					break
				}
			}
			byID[extra.ID] = m
		}
		if extra.DisplayName != "" {
			m.DisplayName = extra.DisplayName
		}
	}
	models = make([]*ModelInfo, 0, len(byID))
	for _, m := range byID {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	excluded := cfg.OAuthExcludedModels[coreauth.OAuthModelAliasChannel(a.Provider, a.AuthKind())]
	if v := strings.TrimSpace(a.Attributes["excluded_models"]); v != "" {
		excluded = strings.Split(v, ",")
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAliasForAuth(cfg, "antigravity", a.AuthKind(), a.Attributes, models)
	return models
}

func (s *Service) publishCatalogLocked(a *coreauth.Auth, cfg *config.Config, state *antigravityCatalogState) bool {
	publish := func(current *coreauth.Auth) {
		models := catalogModels(current, cfg, state)
		models = s.appendPluginModels("antigravity", models)
		models = applyModelPrefixes(models, current.Prefix, cfg.ForceModelPrefix)
		registry.GetGlobalRegistry().RegisterClientPreservingState(current.ID, "antigravity", models)
	}
	if s.coreManager == nil {
		publish(a)
		return true
	}
	published := false
	s.coreManager.WithCatalogRegistration(a.ID, a.RegistrationEpoch, func(current *coreauth.Auth) {
		if catalogRoute(current, cfg) == state.Route {
			publish(current)
			published = true
		}
	})
	return published
}

func (s *Service) registerAntigravityCatalog(ctx context.Context, a *coreauth.Auth) {
	if s == nil || a == nil || a.Disabled {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	s.agCatalogMu.Lock()
	state := s.catalogStateLocked(a, cfg)
	if !reflect.DeepEqual(state.settings, cfg.AntigravityModels) {
		state.next = time.Time{}
		state.settings = cfg.CloneForRuntime().AntigravityModels
	}
	s.publishCatalogLocked(a, cfg, state)
	enabled := cfg.AntigravityModels.DiscoveryEnabled
	s.agCatalogMu.Unlock()
	s.cfgMu.RUnlock()
	if enabled {
		s.queueAntigravityCatalog(ctx, a)
	} else {
		s.asyncProbeAntigravityCapabilities(ctx, a, "antigravity")
	}
}

func (s *Service) queueAntigravityCatalog(ctx context.Context, a *coreauth.Auth) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.cfgMu.RLock()
	if s.cfg == nil || !s.cfg.AntigravityModels.DiscoveryEnabled || a.Disabled {
		s.cfgMu.RUnlock()
		return
	}
	cfg := s.cfg.CloneForRuntime()
	s.agCatalogMu.Lock()
	state := s.catalogStateLocked(a, cfg)
	if state.pending || time.Now().Before(state.next) {
		s.agCatalogMu.Unlock()
		s.cfgMu.RUnlock()
		return
	}
	state.pending = true
	s.agCatalogMu.Unlock()
	s.cfgMu.RUnlock()
	a = a.Clone()
	s.antigravityProbeWg.Add(1)
	go func() {
		defer s.antigravityProbeWg.Done()
		select {
		case antigravityCatalogSlots <- struct{}{}:
		case <-ctx.Done():
			s.agCatalogMu.Lock()
			state.pending = false
			s.agCatalogMu.Unlock()
			return
		}
		defer func() { <-antigravityCatalogSlots }()
		models, err := fetchAntigravityCatalog(ctx, a, cfg)
		s.cfgMu.RLock()
		s.agCatalogMu.Lock()
		state.pending = false
		active := s.cfg
		valid := active != nil && active.AntigravityModels.DiscoveryEnabled && reflect.DeepEqual(active.AntigravityModels, cfg.AntigravityModels) && active.ProxyURL == cfg.ProxyURL && active.AuthDir == cfg.AuthDir && s.agCatalogs[a.ID] == state && ctx.Err() == nil
		if valid && s.coreManager != nil {
			current, ok := s.coreManager.GetByID(a.ID)
			valid = ok && !current.Disabled && current.RegistrationEpoch == a.RegistrationEpoch && catalogRoute(current, active) == state.Route
		}
		if valid {
			if err != nil {
				state.failures++
				delay := time.Minute * time.Duration(1<<min(state.failures-1, 5))
				state.next = time.Now().Add(min(delay, 30*time.Minute))
				log.WithFields(log.Fields{"auth_id": a.ID, "using_snapshot": !state.Success.IsZero(), "last_success": state.Success, "retry_at": state.next, "reason": err.Error()}).Warn("antigravity model discovery failed; retaining catalog")
			} else {
				previous := state.Models
				state.Models = models
				state.Success = time.Now().UTC()
				state.failures = 0
				// Deterministic early jitter keeps every account within the configured cadence.
				hash := sha256.Sum256([]byte(a.ID))
				state.next = time.Now().Add(cfg.AntigravityModels.Interval() * (89 + time.Duration(hash[0]%11)) / 100)
				if s.publishCatalogLocked(a, active, state) {
					if errSave := saveAntigravityCatalog(catalogSnapshotPath(cfg, a.ID), state.antigravityCatalogSnapshot); errSave != nil {
						log.Warn("antigravity model snapshot could not be saved")
					}
					added, removed := catalogDelta(previous, models)
					log.WithFields(log.Fields{"auth_id": a.ID, "models": len(models), "added": added, "removed": removed, "last_success": state.Success}).Info("antigravity model discovery succeeded")
				}
			}
		}
		s.agCatalogMu.Unlock()
		s.cfgMu.RUnlock()
		if valid && err == nil && s.coreManager != nil {
			s.coreManager.ReconcileRegistryModelStates(ctx, a.ID)
			s.coreManager.RefreshSchedulerEntry(a.ID)
		}
	}()
}

func catalogDelta(old, next []*ModelInfo) (int, int) {
	ids := make(map[string]bool)
	for _, m := range old {
		if m != nil {
			ids[m.ID] = true
		}
	}
	added := 0
	for _, m := range next {
		if !ids[m.ID] {
			added++
		}
		delete(ids, m.ID)
	}
	return added, len(ids)
}

func fetchAntigravityCatalog(ctx context.Context, a *coreauth.Auth, cfg *config.Config) ([]*ModelInfo, error) {
	token, _ := a.Metadata["access_token"].(string)
	if token == "" {
		return nil, fmt.Errorf("missing access token")
	}
	proxy := a.ProxyURL
	if proxy == "" {
		proxy = cfg.ProxyURL
	}
	transport, _, err := proxyutil.BuildHTTPTransport(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy")
	}
	client := &http.Client{}
	if transport != nil {
		client.Transport = transport
	}
	defer client.CloseIdleConnections()
	// Discovery is bounded background metadata work, never a generation request.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body := map[string]string{}
	if project, ok := a.Metadata["project_id"].(string); ok && project != "" {
		body["project"] = project
	}
	encoded, _ := json.Marshal(body)
	lastError := fmt.Errorf("model discovery connection failed")
	for _, base := range antigravityModelBaseURLs(a) {
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+antigravityModelsPath, bytes.NewReader(encoded))
		if e != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())
		resp, e := client.Do(req)
		if e != nil {
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
		closeErr := resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastError = fmt.Errorf("model discovery HTTP %d", resp.StatusCode)
			continue
		}
		if readErr != nil || closeErr != nil || len(data) > 8*1024*1024 {
			lastError = fmt.Errorf("model discovery response incomplete or too large")
			continue
		}
		models, e := parseAntigravityCatalog(data)
		if e == nil {
			return models, nil
		}
		lastError = e
	}
	return nil, lastError
}

func parseAntigravityCatalog(data []byte) ([]*ModelInfo, error) {
	var response struct {
		Models    map[string]json.RawMessage `json:"models"`
		WebSearch []string                   `json:"webSearchModelIds"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Models == nil {
		return nil, fmt.Errorf("missing or invalid model catalog")
	}
	known := make(map[string]*ModelInfo)
	for _, m := range registry.GetAntigravityModels() {
		known[m.ID] = m
	}
	web := make(map[string]bool)
	for _, id := range response.WebSearch {
		web[id] = true
	}
	models := make([]*ModelInfo, 0, len(response.Models))
	for id, raw := range response.Models {
		if strings.TrimSpace(id) == "" || strings.ContainsAny(id, " \t\r\n") {
			return nil, fmt.Errorf("invalid model ID")
		}
		var metadata struct {
			DisplayName      string `json:"displayName"`
			InputTokenLimit  int    `json:"inputTokenLimit"`
			OutputTokenLimit int    `json:"outputTokenLimit"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("invalid model metadata")
		}
		m := &ModelInfo{ID: id, Object: "model", Type: "antigravity", OwnedBy: "google", UserDefined: true}
		if existing := known[id]; existing != nil {
			clone := *existing
			m = &clone
		} else {
			m.DisplayName = metadata.DisplayName
			m.InputTokenLimit = max(0, metadata.InputTokenLimit)
			m.OutputTokenLimit = max(0, metadata.OutputTokenLimit)
		}
		m.SupportsWebSearch = web[id]
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func saveAntigravityCatalog(path string, snapshot antigravityCatalogSnapshot) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s *Service) startAntigravityCatalogRefresh(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.agCatalogMu.Lock()
	s.agCatalogCancel = cancel
	s.agCatalogMu.Unlock()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if s.coreManager != nil {
				for _, a := range s.coreManager.List() {
					if a.Provider == "antigravity" && !a.Disabled {
						s.queueAntigravityCatalog(ctx, a)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) forgetAntigravityCatalog(id string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	s.agCatalogMu.Lock()
	defer s.agCatalogMu.Unlock()
	delete(s.agCatalogs, id)
	if s.cfg != nil {
		if path := catalogSnapshotPath(s.cfg, id); path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Warn("antigravity model snapshot could not be removed")
			}
		}
	}
}

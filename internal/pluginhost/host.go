package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type loadedPlugin struct {
	id         string
	path       string
	version    string
	name       string
	configYAML []byte
	plugin     pluginapi.Plugin
	registered bool
	client     pluginClient
	callbacks  *pluginCallbackGeneration
}

type modelExecutor interface {
	ExecuteModel(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage)
	ExecuteModelStream(context.Context, handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage)
}

type pluginUnloadTarget struct {
	id        string
	name      string
	path      string
	version   string
	client    pluginClient
	callbacks *pluginCallbackGeneration
}

type pluginShutdown struct {
	targets []pluginUnloadTarget
	done    chan struct{}
	once    sync.Once
}

type pluginLoadRequest struct {
	result         chan pluginLoadResult
	done           chan struct{}
	callbacks      *pluginCallbackGeneration
	cleanupStarted bool
}

type pluginLoadResult struct {
	loaded      *loadedPlugin
	plugin      pluginapi.Plugin
	initialized bool
	err         error
}

type Host struct {
	applyMu                chan struct{}
	mu                     sync.Mutex
	loader                 pluginLoader
	loaded                 map[string]*loadedPlugin
	retired                map[string][]*loadedPlugin
	loading                map[string]*pluginLoadRequest
	shuttingDown           map[string]*pluginShutdown
	nativeQuotaOwners      map[string]int
	fused                  map[string]string
	pluginFileVersions     map[string]string
	activePluginVersions   map[string]string
	activePluginPaths      map[string]string
	cleanupFilesPending    bool
	runtimeConfig          *config.Config
	authManager            *coreauth.Manager
	modelExecutor          modelExecutor
	modelClientIDs         map[string]struct{}
	executorModelClientIDs map[string]struct{}
	modelProviders         map[string]string
	modelRegistrations     map[string]pluginModelRegistration
	providerModels         map[string][]*registryModelInfo
	executorProviders      map[string]struct{}
	accessProviderKeys     map[string]struct{}
	commandLineFlags       map[string]commandLineFlagRecord
	commandLineHits        map[string]struct{}
	managementRoutes       map[string]managementRouteRecord
	resourceRoutes         map[string]resourceRouteRecord
	streams                *streamBridge
	httpStreams            *hostHTTPStreamBridge
	modelStreams           *modelStreamBridge
	callbackContexts       *callbackContextRegistry
	snapshot               atomic.Value
}

func New() *Host {
	h := &Host{
		applyMu:                make(chan struct{}, 1),
		loader:                 defaultPluginLoader(),
		loaded:                 make(map[string]*loadedPlugin),
		retired:                make(map[string][]*loadedPlugin),
		loading:                make(map[string]*pluginLoadRequest),
		shuttingDown:           make(map[string]*pluginShutdown),
		nativeQuotaOwners:      make(map[string]int),
		fused:                  make(map[string]string),
		pluginFileVersions:     make(map[string]string),
		activePluginVersions:   make(map[string]string),
		activePluginPaths:      make(map[string]string),
		cleanupFilesPending:    true,
		modelClientIDs:         make(map[string]struct{}),
		executorModelClientIDs: make(map[string]struct{}),
		modelProviders:         make(map[string]string),
		modelRegistrations:     make(map[string]pluginModelRegistration),
		providerModels:         make(map[string][]*registryModelInfo),
		executorProviders:      make(map[string]struct{}),
		accessProviderKeys:     make(map[string]struct{}),
		commandLineFlags:       make(map[string]commandLineFlagRecord),
		commandLineHits:        make(map[string]struct{}),
		managementRoutes:       make(map[string]managementRouteRecord),
		resourceRoutes:         make(map[string]resourceRouteRecord),
		streams:                newStreamBridge(),
		httpStreams:            newHostHTTPStreamBridge(),
		modelStreams:           newModelStreamBridge(),
		callbackContexts:       newCallbackContextRegistry(),
	}
	h.snapshot.Store(emptySnapshot())
	return h
}

func NewForTest(loader pluginLoader) *Host {
	h := New()
	h.loader = loader
	return h
}

func (h *Host) SetModelExecutor(executor modelExecutor) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.modelExecutor = executor
	h.mu.Unlock()
}

func (h *Host) currentModelExecutor() modelExecutor {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	executor := h.modelExecutor
	h.mu.Unlock()
	return executor
}

func (h *Host) Snapshot() *Snapshot {
	if h == nil {
		return emptySnapshot()
	}
	raw := h.snapshot.Load()
	if snap, ok := raw.(*Snapshot); ok && snap != nil {
		return snap
	}
	return emptySnapshot()
}

// PluginLoaded reports whether a plugin dynamic library is still loaded by the host.
func (h *Host) PluginLoaded(id string) bool {
	if h == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.loaded[id]
	if ok {
		return true
	}
	return len(h.retired[id]) > 0
}

// PluginBusy reports whether a plugin is loaded, loading, or still shutting down.
func (h *Host) PluginBusy(id string) bool {
	if h == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.loaded[id]; ok {
		return true
	}
	if len(h.retired[id]) > 0 {
		return true
	}
	_, ok := h.loading[id]
	return ok || h.shuttingDown[id] != nil
}

// NativeQuotaConflicts reports live legacy quota owners, independently of their
// enabled flag or snapshot registration. A detached client remains a conflict
// until Shutdown returns and its admitted auth-save callbacks have drained.
func (h *Host) NativeQuotaConflicts(providers []string) []string {
	if h == nil {
		return nil
	}
	covered := legacyQuotaPlugins(providers)
	h.mu.Lock()
	defer h.mu.Unlock()
	var conflicts []string
	for _, id := range covered {
		if h.loaded[id] != nil || len(h.retired[id]) > 0 || h.loading[id] != nil || h.shuttingDown[id] != nil {
			conflicts = append(conflicts, id)
		}
	}
	return conflicts
}

// AcquireNativeQuotaOwnership atomically reserves providers for native enforce.
// The caller holds the lease for the entire enforced interval. Loading tokens and
// reservations share mu, so even an ApplyConfig carrying stale configuration
// cannot start a legacy owner after a successful acquisition. Overlapping leases
// support provider changes; releasing one never drops another caller's lease.
func (h *Host) AcquireNativeQuotaOwnership(providers []string) (func(), error) {
	if h == nil {
		return nil, fmt.Errorf("plugin host is unavailable")
	}
	ids := legacyQuotaPlugins(providers)
	h.mu.Lock()
	for _, id := range ids {
		if h.loaded[id] != nil || len(h.retired[id]) > 0 || h.loading[id] != nil || h.shuttingDown[id] != nil {
			h.mu.Unlock()
			return nil, fmt.Errorf("native quota ownership conflicts with plugin %s; completed stop is required", id)
		}
	}
	for _, id := range ids {
		h.nativeQuotaOwners[id]++
	}
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			for _, id := range ids {
				h.nativeQuotaOwners[id]--
				if h.nativeQuotaOwners[id] == 0 {
					delete(h.nativeQuotaOwners, id)
				}
			}
			h.mu.Unlock()
		})
	}, nil
}

func (h *Host) ApplyConfig(ctx context.Context, cfg *config.Config) {
	if h == nil || !h.lockApply(ctx) {
		return
	}
	defer h.unlockApply()
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return
	}

	rc, errRuntimeConfig := runtimeConfigFromConfig(cfg)
	if errRuntimeConfig != nil {
		log.WithError(errRuntimeConfig).Error("failed to apply plugin runtime config")
		return
	}
	h.mu.Lock()
	h.runtimeConfig = cfg
	h.mu.Unlock()

	if !rc.Enabled {
		h.mu.Lock()
		h.managementRoutes = make(map[string]managementRouteRecord)
		h.resourceRoutes = make(map[string]resourceRouteRecord)
		h.rebuildActivePluginMapsLocked(nil)
		h.snapshot.Store(emptySnapshot())
		h.mu.Unlock()
		h.refreshThinkingProviders(nil)
		return
	}

	desiredVersions := desiredPluginVersions(rc.Items)
	files, errSelect := selectPluginFiles(rc.Dir, desiredVersions)
	if errSelect != nil {
		log.Warnf("pluginhost: failed to select plugin files: %v", errSelect)
		h.mu.Lock()
		h.managementRoutes = make(map[string]managementRouteRecord)
		h.resourceRoutes = make(map[string]resourceRouteRecord)
		h.rebuildActivePluginMapsLocked(nil)
		h.snapshot.Store(emptySnapshot())
		h.mu.Unlock()
		h.refreshThinkingProviders(nil)
		return
	}
	files = h.withLoadedPluginFallbacks(files, rc.Items, desiredVersions)

	records := make([]capabilityRecord, 0, len(files))
	loadedFiles := make([]pluginFile, 0, len(files))
	hotReloadLogs := make([]log.Fields, 0)
	for _, file := range files {
		item, ok := rc.Items[file.ID]
		if !ok {
			item = defaultRuntimeItemConfig(file.ID)
		}
		if !item.Enabled {
			continue
		}
		if nativeQuotaBlocksPlugin(cfg, file.ID) {
			log.WithField("plugin_id", file.ID).Warn("pluginhost: native quota enforce prevents legacy scheduler loading")
			continue
		}
		h.mu.Lock()
		if h.shuttingDown[file.ID] != nil || h.nativeQuotaOwners[file.ID] > 0 {
			h.mu.Unlock()
			continue
		}
		lp := h.loaded[file.ID]
		var replaced *loadedPlugin
		if lp != nil && cleanPluginPath(lp.path) != cleanPluginPath(file.Path) {
			replaced = lp
			lp = nil
		}
		_, disabled := h.fused[file.ID]
		h.mu.Unlock()
		if disabled && replaced == nil {
			continue
		}

		loadedNow := false
		var hotReloadFields log.Fields
		var plugin pluginapi.Plugin
		registeredNow := false
		if lp == nil {
			request := &pluginLoadRequest{
				result:    make(chan pluginLoadResult, 1),
				done:      make(chan struct{}),
				callbacks: newPluginCallbackGeneration(file.ID),
			}
			h.mu.Lock()
			if h.loading[file.ID] != nil || h.shuttingDown[file.ID] != nil || h.nativeQuotaOwners[file.ID] > 0 {
				h.mu.Unlock()
				continue
			}
			h.loading[file.ID] = request
			h.mu.Unlock()

			if replaced != nil {
				h.callQuiesce(ctx, replaced)
				if errContext := ctx.Err(); errContext != nil {
					h.clearLoadingRequest(file.ID, request)
					_, _, _ = h.rollbackReplacement(replaced, item)
					return
				}
			}
			h.startPluginLoad(ctx, file, item, request)

			loadResult, completed := h.waitForPluginLoad(ctx, request)
			if !completed {
				if replaced == nil {
					h.cleanupCanceledPluginLoad(file.ID, request)
				} else {
					h.cleanupCanceledPluginLoadAndWait(file.ID, request)
					_, _, _ = h.rollbackReplacement(replaced, item)
				}
				return
			}
			if loadResult.err != nil || (replaced != nil && !loadResult.initialized) {
				if replaced == nil {
					h.cleanupPluginLoad(file.ID, request, loadResult.loaded)
				} else {
					h.cleanupPluginLoadAndWait(file.ID, request, loadResult.loaded)
					if rollbackRecord, rollbackFile, okRollback := h.rollbackReplacement(replaced, item); okRollback {
						records = append(records, rollbackRecord)
						loadedFiles = append(loadedFiles, rollbackFile)
					}
				}
				if loadResult.err != nil {
					log.Warnf("pluginhost: failed to load plugin %s from %s: %v", file.ID, file.Path, loadResult.err)
				}
				continue
			}

			h.mu.Lock()
			if h.loading[file.ID] != request {
				h.mu.Unlock()
				h.discardLoadedPlugin(loadResult.loaded)
				if replaced != nil {
					_, _, _ = h.rollbackReplacement(replaced, item)
				}
				return
			}
			if errContext := ctx.Err(); errContext != nil {
				h.mu.Unlock()
				if replaced == nil {
					h.cleanupPluginLoad(file.ID, request, loadResult.loaded)
				} else {
					h.cleanupPluginLoadAndWait(file.ID, request, loadResult.loaded)
					_, _, _ = h.rollbackReplacement(replaced, item)
				}
				return
			}
			delete(h.loading, file.ID)
			close(request.done)
			lp = loadResult.loaded
			if replaced != nil {
				hotReloadFields = pluginHotReloadLogFields(file.ID, file.Version, file.Path, replaced.version, replaced.path)
				h.retireLoadedPluginLocked(replaced)
				delete(h.fused, file.ID)
				h.removePluginRuntimeStateLocked(file.ID)
			}
			h.loaded[file.ID] = lp
			loadedNow = true
			plugin = loadResult.plugin
			registeredNow = loadResult.initialized
			h.mu.Unlock()
			log.WithFields(pluginLogFields(file.ID, "", file.Version, file.Path)).Info("pluginhost: plugin loaded")
		}

		if !registeredNow {
			if loadedNow {
				continue
			}
			var okCall bool
			plugin, okCall = h.callRegister(ctx, lp, item)
			if !okCall {
				continue
			}
		}
		plugin.Metadata = clonePluginMetadata(plugin.Metadata)
		h.mu.Lock()
		if lp != nil {
			lp.name = strings.TrimSpace(plugin.Metadata.Name)
			if strings.TrimSpace(lp.version) == "" {
				lp.version = strings.TrimSpace(plugin.Metadata.Version)
			}
		}
		h.mu.Unlock()
		if loadedNow {
			log.WithFields(pluginLogFieldsFromMetadata(file.ID, plugin.Metadata, file.Path)).Info("pluginhost: plugin registered")
		}
		if hotReloadFields != nil {
			hotReloadLogs = append(hotReloadLogs, hotReloadFields)
		}
		records = append(records, capabilityRecord{
			id:       file.ID,
			path:     file.Path,
			version:  file.Version,
			priority: item.Priority,
			meta:     plugin.Metadata,
			plugin:   plugin,
		})
		loadedFiles = append(loadedFiles, file)
	}

	sortRecords(records)
	h.mu.Lock()
	cleanupFiles := h.cleanupFilesPending
	if len(loadedFiles) > 0 {
		h.cleanupFilesPending = false
	}
	h.rebuildActivePluginMapsLocked(records)
	h.snapshot.Store(&Snapshot{enabled: true, records: records, quotaSupportedProviders: make(map[string][]string)})
	h.mu.Unlock()
	h.refreshThinkingProviders(records)
	for _, fields := range hotReloadLogs {
		log.WithFields(fields).Info("pluginhost: plugin hot reloaded")
	}
	if cleanupFiles && len(loadedFiles) > 0 {
		if errCleanup := cleanupUnselectedPluginFiles(rc.Dir, loadedFiles); errCleanup != nil {
			log.Warnf("pluginhost: failed to clean old plugin files: %v", errCleanup)
		}
	}
}

func (h *Host) startPluginLoad(ctx context.Context, file pluginFile, item runtimeItemConfig, request *pluginLoadRequest) {
	if h == nil || request == nil || request.result == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	file.callbacks = request.callbacks
	go func() {
		client, errOpen := h.loader.Open(file, h)
		if errOpen != nil {
			request.result <- pluginLoadResult{err: errOpen}
			return
		}
		if client == nil {
			request.result <- pluginLoadResult{err: fmt.Errorf("plugin loader returned nil client")}
			return
		}
		loaded := &loadedPlugin{
			id:        file.ID,
			path:      file.Path,
			version:   file.Version,
			client:    newGuardedPluginClient(client),
			callbacks: request.callbacks,
		}
		plugin, okCall := h.callRegister(ctx, loaded, item)
		request.result <- pluginLoadResult{loaded: loaded, plugin: plugin, initialized: okCall}
	}()
}

func (h *Host) waitForPluginLoad(ctx context.Context, request *pluginLoadRequest) (pluginLoadResult, bool) {
	if h == nil || request == nil || request.result == nil {
		return pluginLoadResult{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result := <-request.result:
		return result, true
	case <-ctx.Done():
		return pluginLoadResult{}, false
	}
}

func (h *Host) cleanupCanceledPluginLoad(id string, request *pluginLoadRequest) {
	if h == nil || request == nil || request.result == nil {
		return
	}
	h.mu.Lock()
	if h.loading[id] != request || request.cleanupStarted {
		h.mu.Unlock()
		return
	}
	request.cleanupStarted = true
	request.callbacks.detach()
	h.mu.Unlock()

	go func() {
		result := <-request.result
		h.finishPluginLoadCleanup(id, request, result.loaded)
	}()
}

func (h *Host) cleanupCanceledPluginLoadAndWait(id string, request *pluginLoadRequest) {
	if h == nil || request == nil || request.result == nil {
		return
	}
	h.mu.Lock()
	if h.loading[id] != request || request.cleanupStarted {
		h.mu.Unlock()
		return
	}
	request.cleanupStarted = true
	request.callbacks.detach()
	h.mu.Unlock()

	result := <-request.result
	h.discardLoadedPlugin(result.loaded)
	h.clearLoadingRequest(id, request)
}

// cleanupPluginLoad retains the matching load token until the client has physically
// shut down, preventing a replacement ApplyConfig from opening a second client.
func (h *Host) cleanupPluginLoad(id string, request *pluginLoadRequest, loaded *loadedPlugin) {
	if h == nil || request == nil {
		return
	}
	h.mu.Lock()
	if h.loading[id] != request || request.cleanupStarted {
		h.mu.Unlock()
		return
	}
	request.cleanupStarted = true
	request.callbacks.detach()
	h.mu.Unlock()

	h.finishPluginLoadCleanup(id, request, loaded)
}

func (h *Host) cleanupPluginLoadAndWait(id string, request *pluginLoadRequest, loaded *loadedPlugin) {
	if h == nil || request == nil {
		return
	}
	h.mu.Lock()
	if h.loading[id] != request || request.cleanupStarted {
		h.mu.Unlock()
		return
	}
	request.cleanupStarted = true
	request.callbacks.detach()
	h.mu.Unlock()

	h.discardLoadedPlugin(loaded)
	h.clearLoadingRequest(id, request)
}

func (h *Host) finishPluginLoadCleanup(id string, request *pluginLoadRequest, loaded *loadedPlugin) {
	go func() {
		h.discardLoadedPlugin(loaded)
		h.clearLoadingRequest(id, request)
	}()
}

func (h *Host) clearLoadingRequest(id string, request *pluginLoadRequest) {
	if h == nil || request == nil {
		return
	}
	request.callbacks.detach()
	request.callbacks.wait()
	h.mu.Lock()
	if h.loading[id] == request {
		delete(h.loading, id)
		if request.done != nil {
			close(request.done)
		}
	}
	h.mu.Unlock()
}

func (h *Host) discardLoadedPlugin(loaded *loadedPlugin) {
	if loaded == nil {
		return
	}
	loaded.callbacks.detach()
	shutdownPluginClient(context.Background(), loaded.client)
	loaded.callbacks.wait()
}

func (h *Host) withLoadedPluginFallbacks(files []pluginFile, items map[string]runtimeItemConfig, desired map[string]string) []pluginFile {
	if h == nil || len(desired) == 0 {
		return files
	}
	selected := make(map[string]struct{}, len(files))
	for _, file := range files {
		id := strings.TrimSpace(file.ID)
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if _, ok := selected[id]; ok {
			continue
		}
		if item, ok := items[id]; ok && !item.Enabled {
			continue
		}
		lp := h.loaded[id]
		if lp == nil || strings.TrimSpace(lp.path) == "" {
			continue
		}
		files = append(files, pluginFile{
			ID:      id,
			Path:    lp.path,
			Version: strings.TrimSpace(lp.version),
		})
		selected[id] = struct{}{}
	}
	return files
}

// UnloadPlugin removes one plugin from the active runtime and closes its dynamic library.
func (h *Host) UnloadPlugin(id string) bool {
	return h.UnloadPluginContext(context.Background(), id)
}

// UnloadPluginContext detaches a plugin from the runtime before waiting for its
// active calls. Physical client cleanup continues after cancellation if needed.
func (h *Host) UnloadPluginContext(ctx context.Context, id string) bool {
	if h == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" || !h.lockApply(ctx) {
		return false
	}
	defer h.unlockApply()

	h.mu.Lock()
	shutdown, records := h.detachPluginLocked(id)
	h.mu.Unlock()
	if shutdown == nil {
		return false
	}
	h.refreshThinkingProviders(records)
	h.RegisterFrontendAuthProviders()
	h.startPluginShutdown(id, shutdown)
	_ = waitPluginCompletion(ctx, shutdown.done)
	return true
}

// StopPlugin explicitly stops a disabled plugin without deleting its binaries or
// persistent cache. Unlike UnloadPluginContext's detach result, nil acknowledges
// completed shutdown of every generation. Cancellation stops waiting, not cleanup;
// the plugin remains busy/conflicting until cleanup actually finishes.
// Missing configuration, enabled:nil, and a global plugins disable are not an
// explicit per-plugin disable and do not authorize this operation.
func (h *Host) StopPlugin(ctx context.Context, id string) error {
	if h == nil {
		return fmt.Errorf("plugin host is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	id = strings.TrimSpace(id)
	if !validPluginID(id) {
		return fmt.Errorf("invalid plugin id")
	}
	if !h.lockApply(ctx) {
		return ctx.Err()
	}
	defer h.unlockApply()
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}

	h.mu.Lock()
	var item config.PluginInstanceConfig
	if h.runtimeConfig != nil {
		item = h.runtimeConfig.Plugins.Configs[id]
	}
	if item.Enabled == nil || *item.Enabled {
		h.mu.Unlock()
		return fmt.Errorf("plugin %s must be explicitly configured enabled: false before stop", id)
	}
	request := h.loading[id]
	shutdown, records := h.detachPluginLocked(id)
	h.mu.Unlock()

	if request != nil {
		h.cleanupCanceledPluginLoad(id, request)
	}
	if shutdown != nil {
		h.refreshThinkingProviders(records)
		h.RegisterFrontendAuthProviders()
		h.startPluginShutdown(id, shutdown)
		if errWait := waitPluginCompletion(ctx, shutdown.done); errWait != nil {
			return errWait
		}
	}
	if request != nil {
		if errWait := waitPluginCompletion(ctx, request.done); errWait != nil {
			return errWait
		}
	}
	return nil
}

func (h *Host) detachPluginLocked(id string) (*pluginShutdown, []capabilityRecord) {
	if shutdown := h.shuttingDown[id]; shutdown != nil {
		return shutdown, h.Snapshot().records
	}
	var targets []pluginUnloadTarget
	if lp := h.loaded[id]; lp != nil {
		targets = append(targets, unloadTarget(lp))
	}
	for _, lp := range h.retired[id] {
		if lp != nil {
			targets = append(targets, unloadTarget(lp))
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	shutdown := h.trackPluginShutdownLocked(id, targets)
	delete(h.loaded, id)
	delete(h.retired, id)
	delete(h.fused, id)
	delete(h.activePluginVersions, id)
	delete(h.activePluginPaths, id)
	for _, target := range targets {
		delete(h.pluginFileVersions, cleanPluginPath(target.path))
	}
	records, enabled := h.snapshotWithoutPluginLocked(id)
	h.removePluginRuntimeStateLocked(id)
	h.snapshot.Store(&Snapshot{enabled: enabled, records: records})
	return shutdown, records
}

func unloadTarget(lp *loadedPlugin) pluginUnloadTarget {
	return pluginUnloadTarget{id: lp.id, name: lp.name, path: lp.path, version: lp.version, client: lp.client, callbacks: lp.callbacks}
}

func (h *Host) trackPluginShutdownLocked(id string, targets []pluginUnloadTarget) *pluginShutdown {
	if shutdown := h.shuttingDown[id]; shutdown != nil {
		return shutdown
	}
	shutdown := &pluginShutdown{targets: targets, done: make(chan struct{})}
	for _, target := range targets {
		target.callbacks.detach()
	}
	h.shuttingDown[id] = shutdown
	return shutdown
}

func (h *Host) startPluginShutdown(id string, shutdown *pluginShutdown) {
	shutdown.once.Do(func() {
		go func() {
			for _, target := range shutdown.targets {
				shutdownPluginClient(context.Background(), target.client)
				target.callbacks.wait()
				log.WithFields(pluginLogFields(target.id, target.name, target.version, target.path)).Info("pluginhost: plugin unloaded")
			}
			h.mu.Lock()
			if h.shuttingDown[id] == shutdown {
				delete(h.shuttingDown, id)
			}
			close(shutdown.done)
			h.mu.Unlock()
		}()
	})
}

func waitPluginCompletion(ctx context.Context, done <-chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ShutdownAll removes active plugin capabilities and closes all loaded dynamic libraries.
func (h *Host) ShutdownAll() {
	h.ShutdownAllContext(context.Background())
}

// ShutdownAllContext detaches all plugin runtime state without waiting beyond ctx
// for active plugin calls to complete.
func (h *Host) ShutdownAllContext(ctx context.Context) {
	if h == nil || !h.lockApply(ctx) {
		return
	}
	defer h.unlockApply()

	targets := make([]pluginUnloadTarget, 0)
	var loading map[string]*pluginLoadRequest
	h.mu.Lock()
	loading = make(map[string]*pluginLoadRequest, len(h.loading))
	for id, request := range h.loading {
		loading[id] = request
	}
	for _, lp := range h.loaded {
		if lp == nil || lp.client == nil {
			continue
		}
		targets = append(targets, unloadTarget(lp))
	}
	for _, retiredPlugins := range h.retired {
		for _, lp := range retiredPlugins {
			if lp == nil || lp.client == nil {
				continue
			}
			targets = append(targets, unloadTarget(lp))
		}
	}
	byID := make(map[string][]pluginUnloadTarget)
	for _, target := range targets {
		byID[target.id] = append(byID[target.id], target)
	}
	for id, group := range byID {
		h.trackPluginShutdownLocked(id, group)
	}
	shutdowns := make(map[string]*pluginShutdown, len(h.shuttingDown))
	for id, shutdown := range h.shuttingDown {
		shutdowns[id] = shutdown
	}
	h.loaded = make(map[string]*loadedPlugin)
	h.retired = make(map[string][]*loadedPlugin)
	h.modelClientIDs = make(map[string]struct{})
	h.executorModelClientIDs = make(map[string]struct{})
	h.modelProviders = make(map[string]string)
	h.modelRegistrations = make(map[string]pluginModelRegistration)
	h.providerModels = make(map[string][]*registryModelInfo)
	h.executorProviders = make(map[string]struct{})
	h.commandLineFlags = make(map[string]commandLineFlagRecord)
	h.commandLineHits = make(map[string]struct{})
	h.managementRoutes = make(map[string]managementRouteRecord)
	h.resourceRoutes = make(map[string]resourceRouteRecord)
	h.pluginFileVersions = make(map[string]string)
	h.activePluginVersions = make(map[string]string)
	h.activePluginPaths = make(map[string]string)
	h.snapshot.Store(emptySnapshot())
	h.mu.Unlock()

	h.refreshThinkingProviders(nil)
	h.RegisterFrontendAuthProviders()
	for id, request := range loading {
		h.cleanupCanceledPluginLoad(id, request)
	}
	for id, shutdown := range shutdowns {
		h.startPluginShutdown(id, shutdown)
	}
	for _, shutdown := range shutdowns {
		_ = waitPluginCompletion(ctx, shutdown.done)
	}
}

func (h *Host) lockApply(ctx context.Context) bool {
	if h == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case h.applyMu <- struct{}{}:
		return true
	default:
	}
	select {
	case h.applyMu <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *Host) unlockApply() {
	<-h.applyMu
}

func shutdownPluginClient(ctx context.Context, client pluginClient) {
	if client == nil {
		return
	}
	if guarded, ok := client.(*guardedPluginClient); ok {
		guarded.ShutdownContext(ctx)
		return
	}
	client.Shutdown()
}

func cleanPluginPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func (h *Host) retireLoadedPluginLocked(lp *loadedPlugin) {
	if h == nil || lp == nil {
		return
	}
	lp.callbacks.detach()
	h.retired[lp.id] = append(h.retired[lp.id], lp)
}

func (h *Host) recordCurrent(record capabilityRecord) bool {
	return h.pluginIdentityCurrent(record.id, record.path, record.version)
}

func (h *Host) pluginIdentityCurrent(id string, path string, version string) bool {
	if h == nil {
		return false
	}
	version = strings.TrimSpace(version)
	h.mu.Lock()
	defer h.mu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	path = cleanPluginPath(path)
	if path == "" || h.activePluginPaths[id] != path {
		return false
	}
	activePathVersion, okVersion := h.pluginFileVersions[path]
	if !okVersion || activePathVersion != version {
		return false
	}
	return h.activePluginVersions[id] == version
}

func (h *Host) snapshotWithoutPluginLocked(id string) ([]capabilityRecord, bool) {
	raw := h.snapshot.Load()
	snap, _ := raw.(*Snapshot)
	if snap == nil || len(snap.records) == 0 {
		return nil, snap != nil && snap.enabled
	}
	records := make([]capabilityRecord, 0, len(snap.records))
	for _, record := range snap.records {
		if record.id == id {
			continue
		}
		records = append(records, record)
	}
	return records, snap.enabled
}

func (h *Host) removePluginRuntimeStateLocked(id string) {
	for key, record := range h.managementRoutes {
		if record.pluginID == id {
			delete(h.managementRoutes, key)
		}
	}
	for key, record := range h.resourceRoutes {
		if record.pluginID == id {
			delete(h.resourceRoutes, key)
		}
	}
	for name, record := range h.commandLineFlags {
		if record.pluginID == id {
			delete(h.commandLineFlags, name)
			delete(h.commandLineHits, name)
		}
	}
	if registration, ok := h.modelRegistrations[id]; ok {
		delete(h.providerModels, registration.provider)
	}
	delete(h.modelProviders, id)
	delete(h.modelRegistrations, id)
}

func (h *Host) rebuildActivePluginMapsLocked(records []capabilityRecord) {
	h.pluginFileVersions = make(map[string]string, len(records))
	h.activePluginVersions = make(map[string]string, len(records))
	h.activePluginPaths = make(map[string]string, len(records))
	for _, record := range records {
		id := strings.TrimSpace(record.id)
		path := cleanPluginPath(record.path)
		if id == "" || path == "" {
			continue
		}
		h.pluginFileVersions[path] = strings.TrimSpace(record.version)
		h.activePluginVersions[id] = strings.TrimSpace(record.version)
		h.activePluginPaths[id] = path
	}
}

func (h *Host) callQuiesce(ctx context.Context, lp *loadedPlugin) bool {
	if h == nil || lp == nil || lp.client == nil {
		return false
	}
	errQuiesce, okCall := h.safePluginAction(ctx, lp.id, pluginabi.MethodPluginQuiesce, func() error {
		_, errCall := callPlugin[rpcEmptyResponse](ctx, lp.client, pluginabi.MethodPluginQuiesce, rpcEmptyResponse{})
		return errCall
	})
	if errQuiesce != nil {
		logQuiesceError(lp.id, errQuiesce)
	}
	return okCall && errQuiesce == nil
}

func logQuiesceError(id string, errQuiesce error) {
	entry := log.WithError(errQuiesce).WithField("plugin_id", id)
	switch {
	case quiesceUnsupported(errQuiesce):
		entry.Debug("pluginhost: plugin quiesce unsupported")
	case errors.Is(errQuiesce, context.Canceled), errors.Is(errQuiesce, context.DeadlineExceeded):
		entry.Debug("pluginhost: plugin quiesce canceled")
	default:
		entry.Warn("pluginhost: plugin quiesce failed")
	}
}

func quiesceUnsupported(errQuiesce error) bool {
	if errQuiesce == nil {
		return false
	}
	var errRPC rpcError
	if errors.As(errQuiesce, &errRPC) {
		switch strings.ToLower(strings.TrimSpace(errRPC.Code)) {
		case "unknown_method", "method_not_found", "unsupported_method":
			return true
		}
	}
	if errors.Is(errQuiesce, errors.ErrUnsupported) {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(errQuiesce.Error()))
	for _, indication := range []string{
		"unknown method",
		"method not found",
		"unsupported method",
		"method unsupported",
		"method is not supported",
		"method not supported",
	} {
		if strings.Contains(message, indication) {
			return true
		}
	}
	return false
}

func (h *Host) rollbackReplacement(lp *loadedPlugin, item runtimeItemConfig) (capabilityRecord, pluginFile, bool) {
	if h == nil || lp == nil {
		return capabilityRecord{}, pluginFile{}, false
	}
	h.mu.Lock()
	configYAML := bytes.Clone(lp.configYAML)
	previousPlugin := lp.plugin
	h.mu.Unlock()
	if len(configYAML) > 0 {
		item.ConfigYAML = configYAML
	}

	plugin, okCall := h.callRegister(context.Background(), lp, item)
	if okCall {
		h.mu.Lock()
		delete(h.fused, lp.id)
		h.mu.Unlock()
	} else {
		plugin = previousPlugin
	}
	if !validPlugin(plugin) {
		return capabilityRecord{}, pluginFile{}, false
	}
	return capabilityRecord{
		id:       lp.id,
		path:     lp.path,
		version:  lp.version,
		priority: item.Priority,
		meta:     plugin.Metadata,
		plugin:   plugin,
	}, pluginFile{
		ID:      lp.id,
		Path:    lp.path,
		Version: lp.version,
	}, true
}

func (h *Host) callRegister(ctx context.Context, lp *loadedPlugin, item runtimeItemConfig) (pluginapi.Plugin, bool) {
	if lp == nil {
		return pluginapi.Plugin{}, false
	}

	method := pluginabi.MethodPluginRegister
	h.mu.Lock()
	registered := lp.registered
	h.mu.Unlock()
	if registered {
		method = pluginabi.MethodPluginReconfigure
	}

	plugin, okCall := h.safePluginCall(ctx, lp.id, method, func() pluginapi.Plugin {
		plugin, errRegister := registerRPCPlugin(ctx, h, lp.id, lp.client, method, item.ConfigYAML)
		if errRegister != nil {
			log.Warnf("pluginhost: plugin %s %s failed: %v", lp.id, method, errRegister)
			return pluginapi.Plugin{}
		}
		return plugin
	})
	if !okCall {
		return pluginapi.Plugin{}, false
	}
	h.mu.Lock()
	lp.registered = true
	h.mu.Unlock()
	if !validPlugin(plugin) {
		log.Warnf("pluginhost: plugin %s returned invalid metadata or no capabilities", lp.id)
		return pluginapi.Plugin{}, false
	}
	plugin.Metadata = clonePluginMetadata(plugin.Metadata)
	h.mu.Lock()
	lp.name = strings.TrimSpace(plugin.Metadata.Name)
	if strings.TrimSpace(lp.version) == "" {
		lp.version = strings.TrimSpace(plugin.Metadata.Version)
	}
	lp.configYAML = bytes.Clone(item.ConfigYAML)
	lp.plugin = plugin
	h.mu.Unlock()
	return plugin, true
}

func (h *Host) safePluginAction(ctx context.Context, id, method string, fn func() error) (err error, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(id, method, recovered)
			err = nil
			ok = false
		}
	}()

	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err(), false
		default:
		}
	}
	return fn(), true
}

func (h *Host) safePluginCall(ctx context.Context, id, method string, fn func() pluginapi.Plugin) (out pluginapi.Plugin, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(id, method, recovered)
			out = pluginapi.Plugin{}
			ok = false
		}
	}()

	if ctx != nil {
		select {
		case <-ctx.Done():
			return pluginapi.Plugin{}, false
		default:
		}
	}
	return fn(), true
}

func validPlugin(plugin pluginapi.Plugin) bool {
	if strings.TrimSpace(plugin.Metadata.Name) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.Version) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.Author) == "" {
		return false
	}
	if strings.TrimSpace(plugin.Metadata.GitHubRepository) == "" {
		return false
	}
	caps := plugin.Capabilities
	return caps.ModelRegistrar != nil ||
		caps.ModelProvider != nil ||
		caps.AuthProvider != nil ||
		caps.FrontendAuthProvider != nil ||
		caps.Scheduler != nil ||
		caps.ModelRouter != nil ||
		caps.Executor != nil ||
		caps.RequestTranslator != nil ||
		caps.RequestNormalizer != nil ||
		caps.RequestInterceptor != nil ||
		caps.RequestLifecyclePlugin != nil ||
		caps.ResponseTranslator != nil ||
		caps.ResponseBeforeTranslator != nil ||
		caps.ResponseAfterTranslator != nil ||
		caps.ResponseInterceptor != nil ||
		caps.StreamChunkInterceptor != nil ||
		caps.WebSocketResponseObserver != nil ||
		caps.ThinkingApplier != nil ||
		caps.UsagePlugin != nil ||
		caps.CommandLinePlugin != nil ||
		caps.ManagementAPI != nil ||
		caps.QuotaProvider != nil
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}

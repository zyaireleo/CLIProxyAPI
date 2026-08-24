package pluginhost

import (
	"context"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (h *Host) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	record := h.schedulerRecord()
	if requiredID, required, _ := h.RequiredScheduler(req.Provider, req.Providers); required {
		record = h.schedulerRecordByID(requiredID)
	}
	if record == nil {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}

	resp, handled, errPick := h.callScheduler(ctx, *record, req)
	if errPick != nil || !handled {
		return resp, handled, errPick
	}
	if !resp.Handled {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}

	resp, valid, reason := normalizeSchedulerResponse(resp, req)
	if !valid {
		log.WithField("plugin_id", record.id).Warnf("pluginhost: scheduler returned invalid response: %s", reason)
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	return resp, true, nil
}

func (h *Host) HasScheduler() bool {
	return h.schedulerRecord() != nil
}

// RequiredScheduler reports whether the configured route requires one exact
// scheduler plugin and whether that plugin is active. Other schedulers may
// remain active for unrelated providers; PickAuth routes required requests
// directly to the configured plugin. The requirement is retained from config
// even when the plugin failed to load or became fused, allowing the auth
// manager to fail closed instead of silently selecting through built-ins.
func (h *Host) RequiredScheduler(provider string, providers []string) (pluginID string, required bool, ready bool) {
	if h == nil {
		return "", false, false
	}
	routeProviders := schedulerRouteProviderSet(provider, providers)
	if len(routeProviders) == 0 {
		return "", false, false
	}

	h.mu.Lock()
	requiredIDs := make([]string, 0)
	if h.runtimeConfig != nil {
		for id, item := range h.runtimeConfig.Plugins.Configs {
			// The host-owned requirement is independent from plugin enablement.
			// Disabling or unloading a still-required scheduler must fail closed;
			// operators remove required-scheduler-for explicitly to retire it.
			if !providerListsIntersect(item.RequiredSchedulerFor, routeProviders) {
				continue
			}
			requiredIDs = append(requiredIDs, strings.TrimSpace(id))
		}
	}
	h.mu.Unlock()
	if len(requiredIDs) == 0 {
		return "", false, false
	}
	sort.Strings(requiredIDs)
	if len(requiredIDs) != 1 || requiredIDs[0] == "" {
		return strings.Join(requiredIDs, ","), true, false
	}

	for _, record := range h.activeRecords() {
		if record.id == requiredIDs[0] && record.plugin.Capabilities.Scheduler != nil && !h.isPluginFused(record.id) {
			return requiredIDs[0], true, true
		}
	}
	return requiredIDs[0], true, false
}

func schedulerRouteProviderSet(provider string, providers []string) map[string]struct{} {
	out := make(map[string]struct{}, len(providers)+1)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && value != "mixed" {
			out[value] = struct{}{}
		}
	}
	add(provider)
	for _, value := range providers {
		add(value)
	}
	return out
}

func providerListsIntersect(configured []string, route map[string]struct{}) bool {
	for _, provider := range configured {
		if _, ok := route[strings.ToLower(strings.TrimSpace(provider))]; ok {
			return true
		}
	}
	return false
}

func (h *Host) schedulerRecord() *capabilityRecord {
	if h == nil {
		return nil
	}
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) || record.plugin.Capabilities.Scheduler == nil {
			continue
		}
		copyRecord := record
		return &copyRecord
	}
	return nil
}

func (h *Host) schedulerRecordByID(id string) *capabilityRecord {
	if h == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	for _, record := range h.activeRecords() {
		if record.id != id || h.isPluginFused(record.id) || record.plugin.Capabilities.Scheduler == nil {
			continue
		}
		copyRecord := record
		return &copyRecord
	}
	return nil
}

func (h *Host) callScheduler(ctx context.Context, record capabilityRecord, req pluginapi.SchedulerPickRequest) (resp pluginapi.SchedulerPickResponse, handled bool, err error) {
	scheduler := record.plugin.Capabilities.Scheduler
	if h == nil || scheduler == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "Scheduler.Pick", recovered)
			resp = pluginapi.SchedulerPickResponse{}
			handled = false
			err = nil
		}
	}()

	req.Plugin = record.meta
	resp, errPick := scheduler.Pick(ctx, req)
	if errPick != nil {
		log.WithField("plugin_id", record.id).WithError(errPick).Warn("pluginhost: scheduler rejected auth pick")
		return pluginapi.SchedulerPickResponse{}, true, errPick
	}
	return resp, true, nil
}

func normalizeSchedulerResponse(resp pluginapi.SchedulerPickResponse, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, string) {
	resp.AuthID = strings.TrimSpace(resp.AuthID)
	resp.DelegateBuiltin = strings.TrimSpace(resp.DelegateBuiltin)

	hasAuthID := resp.AuthID != ""
	hasDelegate := resp.DelegateBuiltin != ""
	if !hasAuthID && !hasDelegate {
		return pluginapi.SchedulerPickResponse{}, false, "missing auth id or delegate"
	}
	if hasAuthID {
		if !schedulerCandidateExists(req.Candidates, resp.AuthID) {
			return pluginapi.SchedulerPickResponse{}, false, "unknown auth id"
		}
		return resp, true, ""
	}
	if !validSchedulerBuiltin(resp.DelegateBuiltin) {
		return pluginapi.SchedulerPickResponse{}, false, "unknown delegate"
	}
	return resp, true, ""
}

func schedulerCandidateExists(candidates []pluginapi.SchedulerAuthCandidate, authID string) bool {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == authID {
			return true
		}
	}
	return false
}

func validSchedulerBuiltin(delegate string) bool {
	switch delegate {
	case pluginapi.SchedulerBuiltinConfigured, pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst:
		return true
	default:
		return false
	}
}

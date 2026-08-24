package pluginhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostPickAuthUsesHighestPrioritySchedulerOnly(t *testing.T) {
	var highCalls int
	var lowCalls int
	host := newHostWithRecords(
		capabilityRecord{
			id:       "low",
			priority: 1,
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
				lowCalls++
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"}, nil
			})}},
		},
		capabilityRecord{
			id:       "high",
			priority: 10,
			meta:     pluginapi.Metadata{Name: "high", Version: "1.0.0"},
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
				highCalls++
				if req.Plugin.Name != "high" {
					t.Fatalf("req.Plugin.Name = %q, want high", req.Plugin.Name)
				}
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-high"}, nil
			})}},
		},
	)

	resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-high", "auth-low"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if resp.AuthID != "auth-high" {
		t.Fatalf("PickAuth() AuthID = %q, want auth-high", resp.AuthID)
	}
	if highCalls != 1 {
		t.Fatalf("high calls = %d, want 1", highCalls)
	}
	if lowCalls != 0 {
		t.Fatalf("low calls = %d, want 0", lowCalls)
	}
}

func TestHostPickAuthReturnsSchedulerError(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{
		id: "scheduler",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
			return pluginapi.SchedulerPickResponse{}, errors.New("tenant quota exhausted")
		})}},
	})

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if errPick == nil || !strings.Contains(errPick.Error(), "tenant quota exhausted") {
		t.Fatalf("PickAuth() error = %v, want tenant quota exhausted", errPick)
	}
}

func TestHostPickAuthPanicFusesAndFallsBack(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{
		id: "scheduler",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
			panic("boom")
		})}},
	})

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
	if handled {
		t.Fatal("PickAuth() handled = true, want false")
	}
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !host.isPluginFused("scheduler") {
		t.Fatal("scheduler plugin was not fused after panic")
	}
}

func TestHostPickAuthUnhandledDoesNotCallLowerPriorityScheduler(t *testing.T) {
	var lowCalls int
	host := newHostWithRecords(
		capabilityRecord{
			id:       "low",
			priority: 1,
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
				lowCalls++
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"}, nil
			})}},
		},
		capabilityRecord{
			id:       "high",
			priority: 10,
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
				return pluginapi.SchedulerPickResponse{Handled: false}, nil
			})}},
		},
	)

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-low"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if handled {
		t.Fatal("PickAuth() handled = true, want false")
	}
	if lowCalls != 0 {
		t.Fatalf("low calls = %d, want 0", lowCalls)
	}
}

func TestHostPickAuthInvalidResponseFallsBack(t *testing.T) {
	tests := []struct {
		name string
		resp pluginapi.SchedulerPickResponse
	}{
		{
			name: "unknown auth id",
			resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "missing"},
		},
		{
			name: "unknown delegate",
			resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: "unknown"},
		},
		{
			name: "handled without decision",
			resp: pluginapi.SchedulerPickResponse{Handled: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{
				id: "scheduler",
				plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
					return tt.resp, nil
				})}},
			})

			_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
			if errPick != nil {
				t.Fatalf("PickAuth() error = %v, want nil", errPick)
			}
			if handled {
				t.Fatal("PickAuth() handled = true, want false")
			}
		})
	}
}

func TestHostPickAuthPrefersValidAuthIDOverInvalidDelegate(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{
		id: "scheduler",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a", DelegateBuiltin: "unknown"}, nil
		})}},
	})

	resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-a"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if resp.AuthID != "auth-a" {
		t.Fatalf("PickAuth() AuthID = %q, want auth-a", resp.AuthID)
	}
}

func TestHostPickAuthAllowsKnownBuiltinDelegates(t *testing.T) {
	for _, delegate := range []string{pluginapi.SchedulerBuiltinConfigured, pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		t.Run(delegate, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{
				id: "scheduler",
				plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
					return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: delegate}, nil
				})}},
			})

			resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
			if errPick != nil {
				t.Fatalf("PickAuth() error = %v, want nil", errPick)
			}
			if !handled {
				t.Fatal("PickAuth() handled = false, want true")
			}
			if resp.DelegateBuiltin != delegate {
				t.Fatalf("PickAuth() DelegateBuiltin = %q, want %q", resp.DelegateBuiltin, delegate)
			}
		})
	}
}

func TestHostRequiredSchedulerReadiness(t *testing.T) {
	enabled := true
	disabled := false
	scheduler := func(id string, priority int) capabilityRecord {
		return capabilityRecord{
			id:       id,
			priority: priority,
			plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-a"}, nil
			})}},
		}
	}
	configure := func(host *Host) {
		host.runtimeConfig = &config.Config{Plugins: config.PluginsConfig{
			Enabled: true,
			Configs: map[string]config.PluginInstanceConfig{
				"quota-guard": {
					Enabled:              &enabled,
					RequiredSchedulerFor: []string{"antigravity"},
				},
			},
		}}
	}

	tests := []struct {
		name         string
		records      []capabilityRecord
		fuseGuard    bool
		provider     string
		providers    []string
		wantRequired bool
		wantReady    bool
		wantPluginID string
		disabled     bool
		globalOff    bool
	}{
		{
			name:         "configured but missing",
			provider:     "antigravity",
			wantRequired: true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "disabled but still required",
			disabled:     true,
			provider:     "antigravity",
			wantRequired: true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "global plugins disabled but marker still required",
			globalOff:    true,
			provider:     "antigravity",
			wantRequired: true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "required scheduler uniquely active",
			records:      []capabilityRecord{scheduler("quota-guard", 10)},
			provider:     "antigravity",
			wantRequired: true,
			wantReady:    true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "second scheduler makes ownership ambiguous",
			records:      []capabilityRecord{scheduler("quota-guard", 10), scheduler("other", 20)},
			provider:     "antigravity",
			wantRequired: true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "required scheduler fused",
			records:      []capabilityRecord{scheduler("quota-guard", 10)},
			fuseGuard:    true,
			provider:     "antigravity",
			wantRequired: true,
			wantPluginID: "quota-guard",
		},
		{
			name:         "mixed route containing required provider",
			records:      []capabilityRecord{scheduler("quota-guard", 10)},
			provider:     "mixed",
			providers:    []string{"gemini", "antigravity"},
			wantRequired: true,
			wantReady:    true,
			wantPluginID: "quota-guard",
		},
		{
			name:      "unrelated provider",
			records:   []capabilityRecord{scheduler("quota-guard", 10)},
			provider:  "gemini",
			wantReady: false,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			host := newHostWithRecords(testCase.records...)
			configure(host)
			if testCase.disabled {
				item := host.runtimeConfig.Plugins.Configs["quota-guard"]
				item.Enabled = &disabled
				host.runtimeConfig.Plugins.Configs["quota-guard"] = item
			}
			if testCase.globalOff {
				host.runtimeConfig.Plugins.Enabled = false
			}
			if testCase.fuseGuard {
				host.mu.Lock()
				host.fused["quota-guard"] = "test fuse"
				host.mu.Unlock()
			}

			pluginID, required, ready := host.RequiredScheduler(testCase.provider, testCase.providers)
			if pluginID != testCase.wantPluginID || required != testCase.wantRequired || ready != testCase.wantReady {
				t.Fatalf("RequiredScheduler() = (%q, %t, %t), want (%q, %t, %t)", pluginID, required, ready, testCase.wantPluginID, testCase.wantRequired, testCase.wantReady)
			}
		})
	}
}

func schedulerRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{
		Provider: "test",
		Model:    "test-model",
	}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id})
	}
	return req
}

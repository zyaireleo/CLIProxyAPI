//go:build cgo && linux

package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDynamicLibraryLoaderUnixOpenAndCall(t *testing.T) {
	if got := SupportPluginHeaderValue(); got != "1" {
		t.Fatalf("SupportPluginHeaderValue() = %q, want 1", got)
	}
	if loader := defaultPluginLoader(); loader != (dynamicLibraryLoader{}) {
		t.Fatalf("defaultPluginLoader() = %T, want dynamicLibraryLoader", loader)
	}
	client, _ := openUnixLoaderFixture(t)

	for _, test := range []struct {
		name    string
		request []byte
		want    string
	}{
		{name: "empty_request", want: `{"empty":true}`},
		{
			name:    "json_request",
			request: []byte(`{"message":"native fixture","values":[1,true]}`),
			want:    `{"message":"native fixture","values":[1,true]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, errCall := client.Call(context.Background(), "fixture.echo", test.request)
			if errCall != nil {
				t.Fatalf("Call() error = %v", errCall)
			}
			if !json.Valid(got) || string(got) != test.want {
				t.Fatalf("Call() response = %q, want JSON %q", got, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, errCall := client.Call(ctx, "fixture.echo", []byte(`{}`)); !errors.Is(errCall, context.Canceled) || got != nil {
		t.Fatalf("Call(canceled) = (%q, %v), want (nil, context.Canceled)", got, errCall)
	}

	// The native counters are sampled before allocating the stats response itself.
	stats, errStats := callPlugin[struct {
		Allocations int `json:"allocations"`
		Frees       int `json:"frees"`
	}](context.Background(), client, "fixture.stats", struct{}{})
	if errStats != nil {
		t.Fatalf("fixture.stats error = %v", errStats)
	}
	if stats.Allocations != 2 || stats.Frees != 2 {
		t.Fatalf("native buffer stats = %+v, want 2 allocations and 2 frees", stats)
	}
}

func TestDynamicLibraryLoaderUnixRegisterRPCPlugin(t *testing.T) {
	client, host := openUnixLoaderFixture(t)
	ctx := context.Background()
	configYAML := []byte("route: local\n")
	plugin, errRegister := registerRPCPlugin(ctx, host, "unix-native-fixture", client, pluginabi.MethodPluginRegister, configYAML)
	if errRegister != nil {
		t.Fatalf("registerRPCPlugin() error = %v", errRegister)
	}
	if plugin.SchemaVersion != 1 || plugin.Metadata.Name != "unix-native-fixture" || plugin.Metadata.Version != "1.0.0" {
		t.Fatalf("unexpected registration: schema = %d, metadata = %+v", plugin.SchemaVersion, plugin.Metadata)
	}
	if plugin.Capabilities.ModelRouter == nil {
		t.Fatal("registration did not expose the native model_router capability")
	}

	rawRequest, errCall := client.Call(ctx, "fixture.registration_request", nil)
	if errCall != nil {
		t.Fatalf("read native registration request: %v", errCall)
	}
	var request rpcLifecycleRequest
	if errDecode := json.Unmarshal(rawRequest, &request); errDecode != nil {
		t.Fatalf("decode native registration request %q: %v", rawRequest, errDecode)
	}
	if request.SchemaVersion != pluginabi.SchemaVersion || !bytes.Equal(request.ConfigYAML, configYAML) {
		t.Fatalf("native registration request = %+v, want schema %d and config %q", request, pluginabi.SchemaVersion, configYAML)
	}

	route, errRoute := plugin.Capabilities.ModelRouter.RouteModel(ctx, pluginapi.ModelRouteRequest{RequestedModel: "input-model"})
	if errRoute != nil {
		t.Fatalf("native model_router.RouteModel() error = %v", errRoute)
	}
	if !route.Handled || route.TargetKind != pluginapi.ModelRouteTargetProvider || route.Target != "fixture-provider" || route.TargetModel != "fixture-model" {
		t.Fatalf("native model route = %+v, want handled fixture-provider/fixture-model route", route)
	}
}

func TestDynamicLibraryLoaderUnixCallErrors(t *testing.T) {
	client, _ := openUnixLoaderFixture(t)
	ctx := context.Background()

	// A nonzero native return code must not hide a structured plugin error.
	raw, errCall := client.Call(ctx, "fixture.error_envelope", []byte(`{}`))
	if errCall != nil {
		t.Fatalf("Call(error envelope) error = %v", errCall)
	}
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
		t.Fatalf("decode native error envelope %q: %v", raw, errDecode)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "fixture_error" || envelope.Error.HTTPStatus != 422 {
		t.Fatalf("unexpected native error envelope: %+v", envelope)
	}
	_, errRPC := callPlugin[struct{}](ctx, client, "fixture.error_envelope", struct{}{})
	var pluginErr rpcError
	if !errors.As(errRPC, &pluginErr) || pluginErr.Code != "fixture_error" || pluginErr.StatusCode() != 422 || pluginErr.Error() != "native failure" {
		t.Fatalf("callPlugin(error envelope) error = %v, want fixture_error with status 422", errRPC)
	}

	got, errNative := client.Call(ctx, "fixture.native_error", nil)
	if errNative == nil || !strings.Contains(errNative.Error(), "plugin call fixture.native_error returned 9: native failure") || got != nil {
		t.Fatalf("Call(native error) = (%q, %v), want nil response and native return code 9", got, errNative)
	}
}

func TestDynamicLibraryLoaderUnixRejectsInvalidPlugins(t *testing.T) {
	for _, test := range []struct {
		name      string
		define    string
		wantError string
	}{
		{
			name:      "missing_init_symbol",
			define:    "-Dcliproxy_plugin_init=fixture_not_plugin_init",
			wantError: "missing cliproxy_plugin_init",
		},
		{
			name:      "unsupported_abi",
			define:    "-DFIXTURE_ABI_VERSION=2",
			wantError: "plugin ABI version 2 is not supported",
		},
		{
			name:      "init_failure",
			define:    "-DFIXTURE_INIT_RESULT=17",
			wantError: "cliproxy_plugin_init returned 17",
		},
		{
			name:      "missing_call",
			define:    "-DFIXTURE_MISSING_CALL=1",
			wantError: "plugin function table is incomplete",
		},
		{
			name:      "missing_free_buffer",
			define:    "-DFIXTURE_MISSING_FREE=1",
			wantError: "plugin function table is incomplete",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := buildUnixLoaderFixture(t, test.define)
			client, errOpen := (dynamicLibraryLoader{}).Open(pluginFile{ID: "unix-native-fixture", Path: path}, New())
			if client != nil {
				t.Cleanup(client.Shutdown)
				t.Fatalf("Open() returned client %T for an invalid plugin", client)
			}
			if errOpen == nil || !strings.Contains(errOpen.Error(), test.wantError) {
				t.Fatalf("Open() error = %v, want %q", errOpen, test.wantError)
			}
		})
	}
}

func openUnixLoaderFixture(t *testing.T) (*dynamicLibraryClient, *Host) {
	t.Helper()
	path := buildUnixLoaderFixture(t)
	host := New()
	client, errOpen := (dynamicLibraryLoader{}).Open(pluginFile{ID: "unix-native-fixture", Path: path}, host)
	if errOpen != nil {
		t.Fatalf("dynamicLibraryLoader.Open() error = %v", errOpen)
	}
	if client == nil {
		t.Fatal("dynamicLibraryLoader.Open() returned a nil client")
	}
	t.Cleanup(client.Shutdown)
	native, ok := client.(*dynamicLibraryClient)
	if !ok {
		t.Fatalf("Open() client = %T, want *dynamicLibraryClient", client)
	}
	return native, host
}

func buildUnixLoaderFixture(t *testing.T, defines ...string) string {
	t.Helper()
	// Use the compiler selected by Go, including a CC configured with go env -w.
	ccOutput, errCC := exec.Command("go", "env", "CC").CombinedOutput()
	if errCC != nil {
		t.Fatalf("go env CC: %v\n%s", errCC, ccOutput)
	}
	cc := strings.Fields(strings.TrimSpace(string(ccOutput)))
	if len(cc) == 0 {
		t.Fatal("go env CC returned an empty compiler command")
	}
	compiler, errCompiler := exec.LookPath(cc[0])
	if errCompiler != nil {
		// CI must run the native fixture tests, never silently skip a missing CC.
		ci := strings.TrimSpace(os.Getenv("CI"))
		if ci != "" && ci != "0" && !strings.EqualFold(ci, "false") {
			t.Fatalf("native loader tests require CC in CI: %q: %v", cc[0], errCompiler)
		}
		t.Skipf("native loader fixture needs local CC %q: %v; CI=true makes this fatal", cc[0], errCompiler)
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "fixture.c")
	library := filepath.Join(dir, "fixture.so")
	if errWrite := os.WriteFile(source, []byte(unixLoaderFixtureSource), 0o600); errWrite != nil {
		t.Fatalf("write native fixture: %v", errWrite)
	}
	args := append([]string(nil), cc[1:]...)
	args = append(args, "-std=c99", "-shared", "-fPIC", "-Wall", "-Wextra", "-Werror")
	args = append(args, defines...)
	args = append(args, "-o", library, source)
	if output, errBuild := exec.Command(compiler, args...).CombinedOutput(); errBuild != nil {
		t.Fatalf("compile native fixture with %q %q: %v\n%s", compiler, args, errBuild, output)
	}
	return library
}

// This ABI-1 fixture is entirely synchronous: no threads, callbacks, or calls after shutdown.
const unixLoaderFixtureSource = `
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef FIXTURE_ABI_VERSION
#define FIXTURE_ABI_VERSION 1
#endif
#ifndef FIXTURE_INIT_RESULT
#define FIXTURE_INIT_RESULT 0
#endif

typedef struct {
    void *ptr;
    size_t len;
} cliproxy_buffer;

typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    int (*call)(void *, const char *, const uint8_t *, size_t, cliproxy_buffer *);
    void (*free_buffer)(void *, size_t);
} cliproxy_host_api;

typedef struct {
    uint32_t abi_version;
    int (*call)(const char *, const uint8_t *, size_t, cliproxy_buffer *);
    void (*free_buffer)(void *, size_t);
    void (*shutdown)(void);
} cliproxy_plugin_api;

static unsigned long allocations;
static unsigned long frees;
static uint8_t *registration_request;
static size_t registration_request_len;

static int write_response(cliproxy_buffer *response, const void *data, size_t len) {
    if (response == NULL || data == NULL || len == 0) {
        return 20;
    }
    response->ptr = malloc(len);
    if (response->ptr == NULL) {
        return 21;
    }
    memcpy(response->ptr, data, len);
    response->len = len;
    allocations++;
    return 0;
}

static int write_text(cliproxy_buffer *response, const char *text) {
    return write_response(response, text, strlen(text));
}

static int plugin_call(const char *method, const uint8_t *request, size_t request_len, cliproxy_buffer *response) {
    if (method == NULL || response == NULL) {
        return 22;
    }
    response->ptr = NULL;
    response->len = 0;
    if (strcmp(method, "fixture.echo") == 0) {
        if (request_len == 0) {
            return write_text(response, "{\"empty\":true}");
        }
        return write_response(response, request, request_len);
    }
    if (strcmp(method, "fixture.stats") == 0) {
        char stats[160];
        int len = snprintf(stats, sizeof(stats),
            "{\"ok\":true,\"result\":{\"allocations\":%lu,\"frees\":%lu}}", allocations, frees);
        if (len < 0 || (size_t)len >= sizeof(stats)) {
            return 23;
        }
        return write_response(response, stats, (size_t)len);
    }
    if (strcmp(method, "plugin.register") == 0) {
        if (request == NULL || request_len == 0) {
            return 24;
        }
        free(registration_request);
        registration_request = malloc(request_len);
        if (registration_request == NULL) {
            return 25;
        }
        memcpy(registration_request, request, request_len);
        registration_request_len = request_len;
        return write_text(response,
            "{\"ok\":true,\"result\":{\"schema_version\":1,\"metadata\":{"
            "\"Name\":\"unix-native-fixture\",\"Version\":\"1.0.0\",\"Author\":\"test\","
            "\"GitHubRepository\":\"https://github.com/router-for-me/CLIProxyAPI\"},"
            "\"capabilities\":{\"model_router\":true}}}");
    }
    if (strcmp(method, "fixture.registration_request") == 0) {
        return write_response(response, registration_request, registration_request_len);
    }
    if (strcmp(method, "model.route") == 0) {
        return write_text(response,
            "{\"ok\":true,\"result\":{\"Handled\":true,\"TargetKind\":\"provider\","
            "\"Target\":\"fixture-provider\",\"TargetModel\":\"fixture-model\"}}");
    }
    if (strcmp(method, "fixture.error_envelope") == 0) {
        int rc = write_text(response,
            "{\"ok\":false,\"error\":{\"code\":\"fixture_error\","
            "\"message\":\"native failure\",\"http_status\":422}}");
        return rc == 0 ? 7 : rc;
    }
    int rc = write_text(response, "native failure");
    return rc == 0 ? 9 : rc;
}

static void plugin_free(void *ptr, size_t len) {
    (void)len;
    if (ptr != NULL) {
        frees++;
        free(ptr);
    }
}

static void plugin_shutdown(void) {
    free(registration_request);
    registration_request = NULL;
    registration_request_len = 0;
}

__attribute__((visibility("default")))
int cliproxy_plugin_init(const cliproxy_host_api *host, cliproxy_plugin_api *plugin) {
    if (host == NULL || host->abi_version != 1 || host->host_ctx == NULL ||
        host->call == NULL || host->free_buffer == NULL || plugin == NULL) {
        return 26;
    }
    plugin->abi_version = FIXTURE_ABI_VERSION;
    plugin->call = plugin_call;
    plugin->free_buffer = plugin_free;
    plugin->shutdown = plugin_shutdown;
#ifdef FIXTURE_MISSING_CALL
    plugin->call = NULL;
#endif
#ifdef FIXTURE_MISSING_FREE
    plugin->free_buffer = NULL;
#endif
    return FIXTURE_INIT_RESULT;
}
`

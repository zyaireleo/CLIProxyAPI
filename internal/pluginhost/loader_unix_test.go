//go:build cgo && (linux || darwin || freebsd)

package pluginhost

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Build a real native plugin: its worker deliberately survives shutdown and
// retains both the original host API pointer and an unfreed host response.
// File gates make the callback happen after Shutdown returns, not a timing guess.
const unixLateCallbackFixture = `
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <pthread.h>
#include <unistd.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    int (*call)(void*, const char*, const uint8_t*, size_t, buffer*);
    void (*free_buffer)(void*, size_t);
} host_api;
typedef struct {
    uint32_t abi_version;
    int (*call)(const char*, const uint8_t*, size_t, buffer*);
    void (*free_buffer)(void*, size_t);
    void (*shutdown)(void);
} plugin_api;
static const host_api *host;
static void *original_ctx;
static buffer held;
static int shutdowns, shutdown_rc, freed;
static const char save_request[] = "{\"name\":\"late.json\",\"json\":{\"type\":\"demo\",\"priority\":999}}";
static void mark(const char *name) {
    char path[4096]; snprintf(path, sizeof(path), "%s/%s", FIXTURE_DIR, name);
    FILE *f = fopen(path, "w"); if (f) fclose(f);
}
static void await(const char *name) {
    char path[4096]; snprintf(path, sizeof(path), "%s/%s", FIXTURE_DIR, name);
    while (access(path, F_OK)) usleep(1000);
}
static void release(void *ptr, size_t len) {
    if (ptr) { host->free_buffer(ptr, len); freed++; }
}
static void *worker(void *unused) {
    (void)unused;
    await("callback");
    buffer response = {0};
    int rc = host->call(host->host_ctx, "host.auth.save", (const uint8_t*)save_request, sizeof(save_request)-1, &response);
    int empty = response.ptr == NULL && response.len == 0;
    release(response.ptr, response.len);
    release(held.ptr, held.len);
    char path[4096]; snprintf(path, sizeof(path), "%s/result", FIXTURE_DIR);
    FILE *f = fopen(path, "w");
    if (f) {
        fprintf(f, "{\"rc\":%d,\"empty\":%d,\"freed\":%d,\"shutdowns\":%d,\"shutdown_rc\":%d,\"same_ctx\":%d}",
            rc, empty, freed, shutdowns, shutdown_rc, host->host_ctx == original_ctx);
        fclose(f);
    }
    mark("done");
    return NULL;
}
static int call(const char *method, const uint8_t *request, size_t len, buffer *out) {
    if (!strcmp(method, "hold")) {
        return host->call(host->host_ctx, "host.auth.list", (const uint8_t*)"{}", 2, &held);
    }
    return host->call(host->host_ctx, method, request, len, out);
}
static void shutdown_plugin(void) {
    shutdowns++;
    buffer response = {0};
    shutdown_rc = host->call(host->host_ctx, "host.auth.save", (const uint8_t*)save_request, sizeof(save_request)-1, &response);
    release(response.ptr, response.len);
    mark("shutdown");
}
int cliproxy_plugin_init(const host_api *h, plugin_api *p) {
    host = h; original_ctx = h->host_ctx;
    p->abi_version = h->abi_version;
    p->call = call; p->free_buffer = release; p->shutdown = shutdown_plugin;
    pthread_t thread;
    if (pthread_create(&thread, NULL, worker, NULL)) return 99;
    pthread_detach(thread);
    return INIT_RC;
}
`

func buildUnixNativeFixture(t *testing.T, initRC int) (pluginFile, string) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "late.c")
	if err := os.WriteFile(source, []byte(unixLateCallbackFixture), 0600); err != nil {
		t.Fatal(err)
	}
	cc := os.Getenv("CC")
	if cc == "" {
		out, err := exec.Command("go", "env", "CC").CombinedOutput()
		if err != nil {
			t.Fatalf("go env CC: %v: %s", err, out)
		}
		cc = strings.TrimSpace(string(out))
	}
	args := strings.Fields(cc)
	if len(args) == 0 {
		t.Fatal("CGO test requires a C compiler")
	}
	path := filepath.Join(dir, "late.so")
	linkMode := "-shared"
	if runtime.GOOS == "darwin" {
		linkMode = "-dynamiclib"
	}
	args = append(args, linkMode, "-fPIC", "-pthread", "-DFIXTURE_DIR="+strconv.Quote(dir), "-DINIT_RC="+strconv.Itoa(initRC), "-o", path, source)
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("build native fixture: %v: %s", err, out)
	}
	return pluginFile{ID: "native-late-callback", Path: path}, dir
}

func unixNativeGate(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func waitUnixNativeFile(t *testing.T, dir, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("native fixture did not signal %s", name)
}

func assertUnixNativeLateResult(t *testing.T, dir string, freed int) {
	t.Helper()
	waitUnixNativeFile(t, dir, "done")
	raw, err := os.ReadFile(filepath.Join(dir, "result"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		RC         int `json:"rc"`
		Empty      int `json:"empty"`
		Freed      int `json:"freed"`
		Shutdowns  int `json:"shutdowns"`
		ShutdownRC int `json:"shutdown_rc"`
		SameCtx    int `json:"same_ctx"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.RC == 0 || result.Empty != 1 || result.Freed != freed || result.Shutdowns != 1 || result.ShutdownRC == 0 || result.SameCtx != 1 {
		t.Fatalf("unsafe late callback result: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "late.json")); !os.IsNotExist(err) {
		t.Fatalf("stale callback wrote auth state: %v", err)
	}
}

func TestUnixNativeLateCallbackAfterShutdown(t *testing.T) {
	file, dir := buildUnixNativeFixture(t, 0)
	h := New()
	h.runtimeConfig = &config.Config{AuthDir: dir}
	client, err := (dynamicLibraryLoader{}).Open(file, h)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown()
	if _, err := client.Call(context.Background(), "hold", nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(client.Shutdown)
	}
	wg.Wait()
	if _, err := client.Call(context.Background(), pluginabi.MethodHostAuthList, nil); err == nil {
		t.Fatal("closed client admitted a call")
	}
	if _, err := (dynamicLibraryLoader{}).Open(file, h); err == nil {
		t.Fatal("same mapping reinitialized and could reauthorize old workers")
	}
	runtime.GC()
	unixNativeGate(t, dir, "callback")
	assertUnixNativeLateResult(t, dir, 1)
}

func TestUnixNativeLateCallbackAfterInitFailure(t *testing.T) {
	file, dir := buildUnixNativeFixture(t, 7)
	h := New()
	h.runtimeConfig = &config.Config{AuthDir: dir}
	if _, err := (dynamicLibraryLoader{}).Open(file, h); err == nil {
		t.Fatal("fixture init failure was accepted")
	}
	runtime.GC()
	unixNativeGate(t, dir, "callback")
	assertUnixNativeLateResult(t, dir, 0)
}

func TestUnixNativeShutdownDrainsAdmittedCallback(t *testing.T) {
	file, dir := buildUnixNativeFixture(t, 0)
	h := New()
	h.runtimeConfig = &config.Config{AuthDir: dir}
	hook := &quotaBlockingAuthHook{started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(hook.release) })
	h.SetAuthManager(coreauth.NewManager(nil, nil, hook))
	client, err := (dynamicLibraryLoader{}).Open(file, h)
	if err != nil {
		t.Fatal(err)
	}
	unixNativeGate(t, dir, "callback")
	waitForHostTestSignal(t, hook.started, "native admitted auth save")
	done := make(chan struct{})
	go func() { client.Shutdown(); close(done) }()
	waitUnixNativeFile(t, dir, "shutdown")
	select {
	case <-done:
		t.Fatal("shutdown acknowledged an admitted callback before drain")
	default:
	}
	// Plugin shutdown has already run: draining first could deadlock workers
	// which need shutdown to cancel their outstanding callback.
	release.Do(func() { close(hook.release) })
	waitForHostTestSignal(t, done, "native callback drain")
	waitUnixNativeFile(t, dir, "done")
}

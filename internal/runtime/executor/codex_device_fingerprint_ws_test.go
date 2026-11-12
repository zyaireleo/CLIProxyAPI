package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type devFPFileStore struct {
	dir   string
	store *fileauth.FileTokenStore
}

func newDevFPFileStore(t *testing.T) *devFPFileStore {
	t.Helper()
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	return &devFPFileStore{dir: dir, store: store}
}

func (s *devFPFileStore) save(t *testing.T, auth *cliproxyauth.Auth) map[string]any {
	t.Helper()
	if auth.Attributes == nil {
		auth.Attributes = map[string]string{}
	}
	auth.Attributes[cliproxyauth.AttributePath] = filepath.Join(s.dir, auth.ID+".json")
	path, err := s.store.Save(context.Background(), auth)
	if err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode saved auth: %v", err)
	}
	return out
}

// devFPWSUpstream is a websocket upstream that records every handshake and
// frame and answers each response.create with response.completed.
type devFPWSUpstream struct {
	server      *httptest.Server
	connections atomic.Int32
	mu          sync.Mutex
	handshakes  []http.Header
	frames      chan []byte
}

func newDevFPWSUpstream(t *testing.T) *devFPWSUpstream {
	t.Helper()
	upstream := &devFPWSUpstream{frames: make(chan []byte, 32)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		upstream.connections.Add(1)
		upstream.mu.Lock()
		upstream.handshakes = append(upstream.handshakes, r.Header.Clone())
		upstream.mu.Unlock()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			upstream.frames <- bytes.Clone(payload)
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-devfp","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *devFPWSUpstream) lastHandshake() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.handshakes) == 0 {
		return nil
	}
	return u.handshakes[len(u.handshakes)-1]
}

func (u *devFPWSUpstream) nextFrame(t *testing.T) []byte {
	t.Helper()
	select {
	case frame := <-u.frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket frame")
		return nil
	}
}

func devFPWSAuth(id string, baseURL string) *cliproxyauth.Auth {
	auth := devFPAuth(id, "acc-ws")
	auth.Attributes["base_url"] = baseURL
	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	return auth
}

func devFPCloneAuth(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	clone := *auth
	clone.Attributes = make(map[string]string, len(auth.Attributes))
	for k, v := range auth.Attributes {
		clone.Attributes[k] = v
	}
	clone.Metadata = make(map[string]any, len(auth.Metadata))
	for k, v := range auth.Metadata {
		clone.Metadata[k] = v
	}
	return &clone
}

const devFPWSPayload = `{"model":"gpt-5.3-codex","input":[],"client_metadata":{"x-codex-installation-id":"client-1","x-codex-turn-metadata":"{\"installation_id\":\"client-1\",\"turn_id\":\"t1\"}"}}`

func devFPWSClientHeaders() http.Header {
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-1","turn_id":"t1"}`)
	headers.Set("X-Codex-Installation-Id", "client-1")
	headers.Set("Session_id", "sess-devfp")
	return headers
}

func devFPAssertWSConverged(t *testing.T, name string, frame []byte, handshake http.Header, want string) {
	t.Helper()
	if got := gjson.GetBytes(frame, "client_metadata.x-codex-installation-id").String(); got != want {
		t.Fatalf("%s: frame installation = %q, want %q; frame=%s", name, got, want, frame)
	}
	inner := gjson.GetBytes(frame, "client_metadata.x-codex-turn-metadata").String()
	if got := gjson.Get(inner, "installation_id").String(); got != want {
		t.Fatalf("%s: frame turn metadata installation = %q, want %q", name, got, want)
	}
	if got := gjson.Get(headerValueCaseInsensitive(handshake, "X-Codex-Turn-Metadata"), "installation_id").String(); got != want {
		t.Fatalf("%s: handshake turn metadata installation = %q, want %q", name, got, want)
	}
	if want != "client-1" && headerValueCaseInsensitive(handshake, "X-Codex-Installation-Id") != "" {
		t.Fatalf("%s: handshake kept X-Codex-Installation-Id", name)
	}
}

// 28
func TestCodexDeviceFP_WebsocketEndToEnd(t *testing.T) {
	devFPResetState(t)
	upstream := newDevFPWSUpstream(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Codex:     config.CodexConfig{DeviceFingerprintMode: "device"},
	})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := devFPWSAuth("codex-ws-e2e.json", upstream.server.URL)
	want := devFPAccountID("acc-ws")
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(devFPWSPayload)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Headers: devFPWSClientHeaders()}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	devFPAssertWSConverged(t, "execute", upstream.nextFrame(t), upstream.lastHandshake(), want)

	streamOpts := opts
	streamOpts.Stream = true
	result, err := exec.ExecuteStream(context.Background(), auth, req, streamOpts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk: %v", chunk.Err)
		}
	}
	devFPAssertWSConverged(t, "stream", upstream.nextFrame(t), upstream.lastHandshake(), want)
}

// 29
func TestCodexDeviceFP_WebsocketRedialsOnDeviceChange(t *testing.T) {
	devFPResetState(t)
	upstream := newDevFPWSUpstream(t)
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Codex:     config.CodexConfig{DeviceFingerprintMode: "device"},
	})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	const sessionID = "devfp-redial"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	device := devFPWSAuth("codex-ws-redial.json", upstream.server.URL)
	off := devFPCloneAuth(device)
	off.Attributes["codex_device_fingerprint"] = "off"
	want := devFPAccountID("acc-ws")
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(devFPWSPayload)}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Headers:      devFPWSClientHeaders(),
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	execute := func(ctx context.Context, auth *cliproxyauth.Auth) error {
		_, err := exec.Execute(ctx, auth, req, opts)
		return err
	}

	steps := []struct {
		name        string
		auth        *cliproxyauth.Auth
		want        string
		connections int32
	}{
		{"device dial", device, want, 1},
		{"device reuse", device, want, 1},
		{"device to off", off, "client-1", 2},
		{"off reuse", off, "client-1", 2},
		{"off to device", device, want, 3},
	}
	for _, step := range steps {
		if err := execute(context.Background(), step.auth); err != nil {
			t.Fatalf("%s: Execute: %v", step.name, err)
		}
		frame := upstream.nextFrame(t)
		if got := upstream.connections.Load(); got != step.connections {
			t.Fatalf("%s: connections = %d, want %d", step.name, got, step.connections)
		}
		devFPAssertWSConverged(t, step.name, frame, upstream.lastHandshake(), step.want)
	}

	required := cliproxyexecutor.WithRequiredUpstreamWebsocket(context.Background())
	err := execute(required, off)
	if !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		t.Fatalf("required upstream with changed device: err = %v, want replay required", err)
	}
	if got := upstream.connections.Load(); got != 3 {
		t.Fatalf("required upstream dialed a new connection: %d", got)
	}
	if err := execute(required, device); err != nil {
		t.Fatalf("required upstream with matching device: %v", err)
	}
	devFPAssertWSConverged(t, "required matching", upstream.nextFrame(t), upstream.lastHandshake(), want)
}

func devFPDuplexAuth(id string, baseURL string) *cliproxyauth.Auth {
	auth := devFPAuth(id, "acc-duplex")
	auth.Attributes["base_url"] = baseURL
	auth.Attributes["websockets"] = "true"
	return auth
}

// 30
func TestCodexDeviceFP_DuplexPinsDevice(t *testing.T) {
	devFPResetState(t)
	frames := make(chan []byte, 4)
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for _, id := range []string{"r1", "r2"} {
			_, frame, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read create: %v", errRead)
				return
			}
			frames <- frame
			for _, event := range []string{
				`{"type":"response.created","response":{"id":"` + id + `","output":[]}}`,
				`{"type":"response.completed","response":{"id":"` + id + `","output":[]}}`,
			} {
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(event)); errWrite != nil {
					t.Errorf("write: %v", errWrite)
					return
				}
			}
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	input := make(chan cliproxyexecutor.WebsocketInput, 2)
	ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Codex:     config.CodexConfig{ResponseSteering: true, DeviceFingerprintMode: "device"},
	})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := devFPDuplexAuth("codex-duplex-pin.json", server.URL)
	want := devFPAccountID("acc-duplex")
	req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(`{"model":"gpt-5.3-codex","input":[],"instructions":"x","client_metadata":{"x-codex-installation-id":"client-A"}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "devfp-duplex-pin"}}
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for completed < 2 {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatal("stream ended early")
			}
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			if gjson.GetBytes(chunk.Payload, "type").String() == "response.completed" {
				completed++
				if completed == 1 {
					input <- cliproxyexecutor.WebsocketInput{Payload: []byte(`{"type":"response.create","previous_response_id":"r1","input":[],"client_metadata":{"x-codex-installation-id":"client-C"}}`)}
				}
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	cancel()
	for range result.Chunks {
	}
	<-serverDone
	for i := 0; i < 2; i++ {
		frame := <-frames
		if got := gjson.GetBytes(frame, "client_metadata.x-codex-installation-id").String(); got != want {
			t.Fatalf("duplex frame %d installation = %q, want pinned %q", i, got, want)
		}
	}
}

// 31
func TestCodexDeviceFP_DuplexSteerFrameRewritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steer string
		check func(t *testing.T, got []byte, want string)
	}{
		{
			name:  "existing key rewritten",
			steer: `{"type":"response.steer","previous_response_id":"r1","input":"go","client_metadata":{"x-codex-installation-id":"client-B"}}`,
			check: func(t *testing.T, got []byte, want string) {
				if id := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); id != want {
					t.Fatalf("steer installation = %q, want %q; steer=%s", id, want, got)
				}
			},
		},
		{
			name:  "missing key not injected",
			steer: `{"type":"response.steer","previous_response_id":"r1","input":"go"}`,
			check: func(t *testing.T, got []byte, _ string) {
				if gjson.GetBytes(got, "client_metadata").Exists() {
					t.Fatalf("steer gained client_metadata: %s", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devFPResetState(t)
			captured := make(chan []byte, 1)
			serverDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverDone)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					t.Errorf("read create: %v", errRead)
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
				_, steer, errSteer := conn.ReadMessage()
				if errSteer != nil {
					t.Errorf("read steer: %v", errSteer)
					return
				}
				captured <- steer
				for _, event := range []string{
					`{"type":"response.steer.accepted","steer":{"id":"s1","previous_response_id":"r1"}}`,
					`{"type":"response.completed","response":{"id":"r1","output":[]}}`,
				} {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			input := make(chan cliproxyexecutor.WebsocketInput, 1)
			ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
			exec := NewCodexWebsocketsExecutor(&config.Config{
				SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
				Codex:     config.CodexConfig{ResponseSteering: true, DeviceFingerprintMode: "device"},
			})
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			auth := devFPDuplexAuth("codex-duplex-steer.json", server.URL)
			req := cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: []byte(`{"model":"gpt-5.3-codex","input":[],"instructions":"x"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "devfp-duplex-steer"}}
			result, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			done := false
			for !done {
				select {
				case chunk, ok := <-result.Chunks:
					if !ok {
						t.Fatal("stream ended early")
					}
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					switch gjson.GetBytes(chunk.Payload, "type").String() {
					case "response.created":
						input <- cliproxyexecutor.WebsocketInput{Payload: []byte(tc.steer)}
					case "response.completed":
						done = true
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			cancel()
			for range result.Chunks {
			}
			<-serverDone
			tc.check(t, <-captured, devFPAccountID("acc-duplex"))
		})
	}
}

// 32
func TestCodexDeviceFP_InterruptFrameRewritten(t *testing.T) {
	received := make(chan []byte, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			received <- payload
		}
	}))
	defer upstream.Close()
	client, _, errDial := websocket.DefaultDialer.Dial("ws"+upstream.URL[len("http"):], nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()

	const sessionID = "devfp-interrupt"
	executor := NewCodexWebsocketsExecutor(nil)
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	sess := executor.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = client
	sess.authID = "auth"
	sess.wsURL = upstream.URL
	sess.connMu.Unlock()
	readCh := sess.activate(client)
	defer sess.clearActive(client, readCh)

	send := func(deviceKey string, payload string) []byte {
		t.Helper()
		sess.connMu.Lock()
		sess.deviceKey = deviceKey
		sess.connMu.Unlock()
		if err := executor.InterruptExecutionSession(context.Background(), sessionID, []byte(payload)); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		select {
		case got := <-received:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("interrupt not forwarded")
			return nil
		}
	}

	withKey := `{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","x-ext":{"a":1},"client_metadata":{"x-codex-installation-id":"client-1"}}`
	got := send(devFPGoldenAccount1, withKey)
	if want := strings.Replace(withKey, "client-1", devFPGoldenAccount1, 1); string(got) != want {
		t.Fatalf("interrupt = %s, want %s", got, want)
	}
	withoutKey := `{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`
	if got := send(devFPGoldenAccount1, withoutKey); string(got) != withoutKey {
		t.Fatalf("interrupt without key changed: %s", got)
	}
	if got := send("", withKey); string(got) != withKey {
		t.Fatalf("interrupt changed with empty device key: %s", got)
	}
}

// 33
func TestCodexDeviceFP_OffIsByteIdentical(t *testing.T) {
	devFPResetState(t)
	auth := devFPAuth("codex-off-identical.json", "acc-test-1")
	auth.Attributes["header:X-Codex-Installation-Id"] = "$X-Codex-Installation-Id"
	payload := []byte(devFPWSPayload)
	clientHeaders := devFPWSClientHeaders()
	url := "https://example.invalid/backend-api/codex/responses"

	baselineBody := devFPCacheHelperBody(t, NewCodexExecutor(&config.Config{}), auth, url, payload, clientHeaders)
	offBody := devFPCacheHelperBody(t, NewCodexExecutor(devFPConfig("off")), auth, url, payload, clientHeaders)
	if !bytes.Equal(baselineBody, offBody) {
		t.Fatalf("HTTP off body differs:\n%s\n%s", baselineBody, offBody)
	}
	if !bytes.Contains(offBody, []byte("client-1")) {
		t.Fatalf("HTTP off body lost client installation: %s", offBody)
	}
	headersFor := func(cfg *config.Config) http.Header {
		req, _ := http.NewRequest(http.MethodPost, url, nil)
		applyCodexHeadersWithSIWC(req, auth, false, "token", true, cfg, clientHeaders)
		return req.Header
	}
	baseHeaders, offHeaders := headersFor(&config.Config{}), headersFor(devFPConfig("off"))
	for _, key := range []string{"X-Codex-Turn-Metadata", "X-Codex-Installation-Id"} {
		if baseHeaders.Get(key) != offHeaders.Get(key) || offHeaders.Get(key) == "" {
			t.Fatalf("HTTP off header %s differs: %q vs %q", key, baseHeaders.Get(key), offHeaders.Get(key))
		}
	}

	prepare := func(cfg *config.Config) *codexWebsocketPrepared {
		exec := NewCodexWebsocketsExecutor(cfg)
		prepared, err := exec.prepareCodexWebsocketStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.3-codex", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Headers: clientHeaders})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return prepared
	}
	base := prepare(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	offCfg := devFPConfig("off")
	offCfg.DisableImageGeneration = config.DisableImageGenerationAll
	off := prepare(offCfg)
	if !bytes.Equal(base.clientBody, off.clientBody) || off.deviceID != "" {
		t.Fatalf("WS off body differs:\n%s\n%s", base.clientBody, off.clientBody)
	}
	for _, key := range []string{"X-Codex-Turn-Metadata", "X-Codex-Installation-Id"} {
		if headerValueCaseInsensitive(base.wsHeaders, key) != headerValueCaseInsensitive(off.wsHeaders, key) || headerValueCaseInsensitive(off.wsHeaders, key) == "" {
			t.Fatalf("WS off header %s differs", key)
		}
	}
}

// 34
func TestCodexDeviceFP_MalformedShapes(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":                  ``,
		"array":                  `[{"client_metadata":{"x-codex-installation-id":"c"}}]`,
		"string":                 `"text"`,
		"invalid":                `{"client_metadata":`,
		"client_metadata string": `{"client_metadata":"x"}`,
		"client_metadata array":  `{"client_metadata":[{"x-codex-installation-id":"c"}]}`,
		"client_metadata null":   `{"client_metadata":null}`,
		"no installation":        `{"client_metadata":{"session_id":"s"}}`,
	} {
		body := applyCodexDeviceFingerprintBody([]byte(payload), devFPGoldenAccount1, true)
		if name == "no installation" {
			if gjson.Get(string(body), "client_metadata.x-codex-installation-id").String() != devFPGoldenAccount1 || gjson.Get(string(body), "client_metadata.session_id").String() != "s" {
				t.Fatalf("%s: inject into existing object failed: %s", name, body)
			}
			continue
		}
		if name == "invalid" {
			// Not valid JSON: gjson cannot see client_metadata, so injection
			// is attempted only when sjson accepts the document.
			continue
		}
		if string(body) != payload {
			t.Fatalf("%s: malformed body changed: %s", name, body)
		}
	}
	if body := applyCodexDeviceFingerprintBody([]byte(` {"a":1}`), "", true); string(body) != ` {"a":1}` {
		t.Fatalf("empty id changed body: %s", body)
	}
	nonString := []byte(`{"client_metadata":{"x-codex-installation-id":42}}`)
	if got := gjson.GetBytes(applyCodexDeviceFingerprintBody(nonString, devFPGoldenAccount1, false), "client_metadata.x-codex-installation-id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("non-string installation not rewritten: %q", got)
	}
	var nilHeaders http.Header
	applyCodexDeviceFingerprintHeaders(nilHeaders, devFPGoldenAccount1)
	if _, ok := resolveCodexDeviceIdentity(devFPConfig("device"), nil, false); ok {
		t.Fatal("nil auth resolved")
	}
	if key := codexDeviceKey(devFPConfig("off"), devFPAuth("x", "acc"), false); key != "" {
		t.Fatalf("off device key = %q", key)
	}
	if !websocketSessionDeviceKeyMatches(nil, nil, "") || websocketSessionDeviceKeyMatches(nil, nil, "x") {
		t.Fatal("nil session device key match semantics")
	}
}

// 35
func TestCodexDeviceFP_CloakingDisabledStillConverges(t *testing.T) {
	devFPResetState(t)
	hook := devFPCaptureLogs(t)
	cfg := devFPConfig("device")
	cfg.Codex.DisableCodexCloaking = true
	auth := devFPAuth("codex-cloak-off.json", "acc-test-1")
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
	applyCodexHeadersWithSIWC(req, auth, false, "token", true, cfg, http.Header{
		"User-Agent":            {"my-client/1.0"},
		"X-Codex-Turn-Metadata": {`{"installation_id":"client-1"}`},
	})
	if got := req.Header.Get("User-Agent"); got != "my-client/1.0" {
		t.Fatalf("User-Agent = %q, want client value with cloaking disabled", got)
	}
	if got := gjson.Get(req.Header.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got != devFPGoldenAccount1 {
		t.Fatalf("installation = %q, want converged", got)
	}
	if !devFPHasWarn(hook, "cloaking is disabled") {
		t.Fatal("cloaking-disabled warning missing")
	}
}

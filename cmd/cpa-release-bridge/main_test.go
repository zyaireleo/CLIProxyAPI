package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "test-only-control-key-0123456789abcdef"

func testBridge(t *testing.T, handler http.HandlerFunc, queue int, wait time.Duration) (*bridge, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	b, err := newBridge(config{listen: "127.0.0.1:28317", control: "127.0.0.1:28318", upstream: up.URL, keyFile: "unused", maxQueue: queue, maxWait: wait}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)
	return b, front
}

func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for state")
		}
		time.Sleep(time.Millisecond)
	}
}

func command(t *testing.T, b *bridge, method, path string) status {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	b.control(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("control %s: %d %s", path, w.Code, w.Body.String())
	}
	var s status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHoldSSEAndExactOnceQueuedBody(t *testing.T) {
	finish := make(chan struct{})
	defer close(finish)
	var calls atomic.Int32
	observed := make(chan string, 1)
	b, front := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-finish:
			case <-r.Context().Done():
			}
			fmt.Fprint(w, "data: last\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		observed <- r.RequestURI + " " + string(body)
		fmt.Fprint(w, "ok")
	}, 4, time.Second)
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(front.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event: %q %v", line, err)
	}
	s := command(t, b, "POST", "/hold")
	if s.Active != 1 || s.Drained || s.Phase != "held" {
		t.Fatalf("hold status: %+v", s)
	}
	result := make(chan error, 1)
	go func() {
		r, err := client.Post(front.URL+"/a%2Fb?x=%2f&bad=%zz", "application/octet-stream", strings.NewReader("raw\x00body\n"))
		if err == nil {
			_, err = io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != 200 {
				err = fmt.Errorf("status %d", r.StatusCode)
			}
		}
		result <- err
	}()
	await(t, func() bool { return b.snapshot().Queued == 1 })
	if calls.Load() != 1 {
		t.Fatal("held request reached upstream")
	}
	// The existing stream is still active and can deliver more data during hold.
	finish <- struct{}{}
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), "data: last") {
		t.Fatalf("stream was interrupted: %q %v", rest, err)
	}
	await(t, func() bool { return b.snapshot().Drained })
	command(t, b, "POST", "/resume")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := <-observed; got != "/a%2Fb?x=%2f&bad=%zz raw\x00body\n" {
		t.Fatalf("rewritten body/path: %q", got)
	}
	await(t, func() bool { return b.snapshot().Served == 2 })
	if calls.Load() != 2 || b.snapshot().Errors != 0 {
		t.Fatalf("unexpected attempts/status: %d %+v", calls.Load(), b.snapshot())
	}
}

func TestQueueCancellationExpiryAndOverflow(t *testing.T) {
	var calls atomic.Int32
	b, _ := testBridge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, 1, 80*time.Millisecond)
	command(t, b, "POST", "/hold")
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/", strings.NewReader("not sent")).WithContext(ctx)
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { b.ServeHTTP(first, r); close(done) }()
	await(t, func() bool { return b.snapshot().Queued == 1 })
	overflow := httptest.NewRecorder()
	b.ServeHTTP(overflow, httptest.NewRequest("GET", "/", nil))
	if overflow.Code != 503 || overflow.Header().Get("Retry-After") == "" {
		t.Fatal("overflow must be retryable 503")
	}
	cancel()
	<-done
	if first.Code != 503 || b.snapshot().Queued != 0 {
		t.Fatal("cancelled request did not leave queue")
	}
	expired := httptest.NewRecorder()
	start := time.Now()
	b.ServeHTTP(expired, httptest.NewRequest("GET", "/", nil))
	if expired.Code != 503 || expired.Header().Get("Retry-After") == "" || time.Since(start) < 60*time.Millisecond {
		t.Fatal("hold timeout not enforced")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	deadline := httptest.NewRecorder()
	b.ServeHTTP(deadline, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if deadline.Code != 503 || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("client deadline not honored")
	}
	command(t, b, "POST", "/resume")
	if calls.Load() != 0 || b.snapshot().Served != 0 || b.snapshot().Errors != 4 {
		t.Fatalf("rejected requests forwarded: %+v", b.snapshot())
	}
}

func TestFailedBackendNoReplay(t *testing.T) {
	var calls atomic.Int32
	b, front := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}, 1, time.Second)
	for _, method := range []string{"GET", "POST"} {
		r, _ := http.NewRequest(method, front.URL+"/", strings.NewReader("unknown-outcome"))
		r.Header.Set("Idempotency-Key", "does-not-authorize-replay")
		res, err := front.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 502 || res.Header.Get("Retry-After") != "" {
			t.Fatalf("failure status: %d", res.StatusCode)
		}
	}
	await(t, func() bool { return b.snapshot().Served == 2 })
	if calls.Load() != 2 || b.snapshot().Errors != 2 {
		t.Fatalf("unexpected replay/status: %d %+v", calls.Load(), b.snapshot())
	}
	if !b.transport.DisableKeepAlives || b.transport.Proxy != nil {
		t.Fatal("unsafe transport configuration")
	}
}

func TestControlAuthenticationAndParameters(t *testing.T) {
	b, _ := testBridge(t, func(http.ResponseWriter, *http.Request) {}, 1, time.Second)
	for _, tc := range []struct {
		method, path, token, body string
		code                      int
	}{
		{"GET", "/status", "", "", 401},
		{"POST", "/hold", "wrong", "", 401},
		{"POST", "/resume", "", "", 401},
		{"GET", "/unknown", "", "", 401},
		{"GET", "/status", testKey, "", 200},
		{"POST", "/hold?upstream=http://example.com", testKey, "", 400},
		{"POST", "/resume", testKey, `{"upstream":"http://example.com"}`, 400},
		{"POST", "/resume?", testKey, "", 400},
		{"POST", "/%68old", testKey, "", 400},
		{"GET", "/hold", testKey, "", 405},
		{"POST", "/status", testKey, "", 405},
		{"POST", "/upstream", testKey, "", 404},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		b.control(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.code)
		}
	}
	if s := b.snapshot(); s.Phase != "open" || !s.Ready || s.Drained {
		t.Fatalf("invalid initial status: %+v", s)
	}
}

func TestLoopbackValidation(t *testing.T) {
	for _, address := range []string{"0.0.0.0:28317", ":28317", "localhost:28317", "127.0.0.1.evil:28317", "192.168.1.1:8317", "[::]:8317", "127.0.0.1:0", "[::1%lo]:8317"} {
		if _, err := loopbackAddress(address); err == nil {
			t.Errorf("accepted %q", address)
		}
	}
	for _, raw := range []string{"https://127.0.0.1:8317", "http://127.0.0.1:8317/path", "http://127.0.0.1:8317?target=evil", "http://127.0.0.1:8317?", "http://user@127.0.0.1:8317", "http://127.0.0.1:8317#", "http://example.com:8317"} {
		if _, err := upstreamURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8317", "http://[::1]:8317/"} {
		if _, err := upstreamURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	c, err := parseConfig([]string{"--control-key-file", "unused"})
	if err != nil || c.maxQueue != 64 || c.maxWait != 30*time.Second || c.listen != "127.0.0.1:28317" || c.control != "127.0.0.1:28318" || c.upstream != "http://127.0.0.1:8317" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}

func TestWebSocketHoldAndShutdownDrain(t *testing.T) {
	b, front := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		// An upgrade byte tunnel is sufficient: the proxy must not inspect frames.
		_, _ = io.Copy(conn, rw)
	}, 1, time.Second)
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: original\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatalf("upgrade failed: %d", res.StatusCode)
	}
	if s := command(t, b, "POST", "/hold"); s.Active != 1 || s.Drained {
		t.Fatalf("untracked WebSocket: %+v", s)
	}
	payload := []byte{0x81, 0x02, 'o', 'k'}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, got); err != nil || string(got) != string(payload) {
		t.Fatalf("tunnel interrupted: %q %v", got, err)
	}
	queued := httptest.NewRecorder()
	queueDone := make(chan struct{})
	go func() { b.ServeHTTP(queued, httptest.NewRequest("GET", "/queued", nil)); close(queueDone) }()
	await(t, func() bool { return b.snapshot().Queued == 1 })
	b.transition("stopping")
	<-queueDone
	if queued.Code != 503 {
		t.Fatal("shutdown forwarded queued request")
	}
	drained := make(chan struct{})
	go func() { b.activeWG.Wait(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("shutdown did not wait for WebSocket")
	case <-time.After(20 * time.Millisecond):
	}
	if _, ok := b.transition("open"); ok {
		t.Fatal("shutdown can be resumed")
	}
	conn.Close()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("WebSocket did not drain")
	}
	if s := b.snapshot(); s.Active != 0 || s.Ready || s.Served != 1 {
		t.Fatalf("shutdown status: %+v", s)
	}
}

func TestBrokenSSECountsError(t *testing.T) {
	b, front := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 1000\r\n\r\ndata: truncated\n\n")
		rw.Flush()
	}, 1, time.Second)
	res, err := front.Client().Get(front.URL)
	if err == nil {
		_, _ = io.ReadAll(res.Body)
		res.Body.Close()
	}
	await(t, func() bool { return b.snapshot().Served == 1 })
	if b.snapshot().Errors != 1 {
		t.Fatalf("stream error not counted: %+v", b.snapshot())
	}
}

func TestHoldAgeAndForwardedHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	b, _ := testBridge(t, func(w http.ResponseWriter, r *http.Request) { seen <- r.Header.Clone() }, 64, 30*time.Second)
	command(t, b, "POST", "/hold")
	await(t, func() bool { return b.snapshot().HoldDurationMS >= 5 })
	before := b.snapshot()
	repeated := command(t, b, "POST", "/hold")
	if repeated.HoldDurationMS < before.HoldDurationMS || repeated.MaxQueue != 64 || repeated.MaxWaitMS != 30000 || !repeated.Drained {
		t.Fatalf("hold timing: %+v", repeated)
	}
	if s := command(t, b, "POST", "/resume"); s.HoldDurationMS != 0 || s.Drained {
		t.Fatalf("resume timing: %+v", s)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:1234"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("X-Real-IP", "127.0.0.1")
	r.Header.Set("Forwarded", "for=127.0.0.1")
	r.Header.Set("Authorization", "Bearer client-api-key")
	b.ServeHTTP(httptest.NewRecorder(), r)
	h := <-seen
	if h.Get("X-Forwarded-For") != "192.0.2.7" || h.Get("X-Real-IP") != "" || h.Get("Forwarded") != "" || h.Get("Authorization") != "Bearer client-api-key" {
		t.Fatalf("unexpected upstream forwarding headers: %+v", h)
	}
}

func TestFixedTargetDoesNotFollowRedirect(t *testing.T) {
	var escaped atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped.Add(1) }))
	defer other.Close()
	b, _ := testBridge(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 307) }, 1, time.Second)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, httptest.NewRequest("GET", other.URL+"/absolute-target", nil))
	if w.Code != 307 || w.Header().Get("Location") != other.URL || escaped.Load() != 0 {
		t.Fatal("proxy followed redirect or client target")
	}
}

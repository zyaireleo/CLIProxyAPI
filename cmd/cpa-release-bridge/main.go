// cpa-release-bridge is a deployment-only HTTP gate, not a CPA instance.
// It never loads CPA configuration, OAuth credentials, or refresh machinery.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	listen, control, upstream, keyFile string
	maxQueue                           int
	maxWait                            time.Duration
}

func parseConfig(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("cpa-release-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&c.listen, "listen", "127.0.0.1:28317", "loopback data listener")
	fs.StringVar(&c.control, "control", "127.0.0.1:28318", "loopback authenticated control listener")
	fs.StringVar(&c.upstream, "upstream", "http://127.0.0.1:8317", "fixed loopback HTTP upstream origin")
	fs.StringVar(&c.keyFile, "control-key-file", "", "root-owned private file containing the control bearer token")
	fs.IntVar(&c.maxQueue, "max-queue", 64, "maximum requests waiting during a deployment hold")
	fs.DurationVar(&c.maxWait, "max-wait", 30*time.Second, "deployment hold wait limit; not a normal admission timeout")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

// Only literal loopback IPs are accepted: no wildcard binds, DNS, zones, or
// hostname suffix checks. The transport never uses environment HTTP proxies.
func loopbackAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("expected loopback IP:port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return "", errors.New("host must be a literal loopback IP")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return "", errors.New("invalid port")
	}
	return net.JoinHostPort(ip.Unmap().String(), port), nil
}

func upstreamURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid upstream origin")
	}
	if u.Scheme != "http" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("upstream must be a plain HTTP loopback origin without credentials, path, query, or fragment")
	}
	host, err := loopbackAddress(u.Host)
	if err != nil {
		return nil, err
	}
	u.Host, u.Path = host, ""
	return u, nil
}

func (c config) validate() error {
	listen, err := loopbackAddress(c.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	control, err := loopbackAddress(c.control)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	u, err := upstreamURL(c.upstream)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if listen == control || listen == u.Host || control == u.Host {
		return errors.New("listeners and upstream must use distinct addresses")
	}
	if c.keyFile == "" {
		return errors.New("control-key-file is required")
	}
	if c.maxQueue < 0 || c.maxWait <= 0 {
		return errors.New("max-queue must be nonnegative and max-wait must be positive")
	}
	return nil
}

func loadControlKey(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("cannot inspect control key file")
	}
	if err := privateKeyFile(info); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open control key file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("control key file changed while opening")
	}
	if err := privateKeyFile(opened); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return "", errors.New("cannot read control key file or key too large")
	}
	key := strings.TrimSpace(string(data))
	if len(key) < 32 || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("control key must be a single token of at least 32 bytes")
	}
	return key, nil
}

type status struct {
	Phase          string `json:"phase"`
	Ready          bool   `json:"ready"`
	Drained        bool   `json:"drained"`
	Active         int    `json:"active"`
	Queued         int    `json:"queued"`
	Served         uint64 `json:"served"`
	Errors         uint64 `json:"errors"`
	HoldDurationMS int64  `json:"hold_duration_ms"`
	MaxQueue       int    `json:"max_queue"`
	MaxWaitMS      int64  `json:"max_wait_ms"`
}

type bridge struct {
	mu             sync.Mutex
	phase          string
	heldSince      time.Time
	active, queued int
	served, errors uint64
	changed        chan struct{}
	activeWG       sync.WaitGroup
	maxQueue       int
	maxWait        time.Duration
	keyHash        [32]byte
	proxy          *httputil.ReverseProxy
	transport      *http.Transport
}

type attemptKey struct{}
type attempt struct{ failed atomic.Bool }

func markFailed(r *http.Request) {
	if a, ok := r.Context().Value(attemptKey{}).(*attempt); ok {
		a.failed.Store(true)
	}
}

type observedBody struct {
	io.ReadCloser
	request *http.Request
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		markFailed(b.request)
	}
	return n, err
}

type observedUpgrade struct {
	*observedBody
	writer io.Writer
}

func (b *observedUpgrade) Write(p []byte) (int, error) {
	n, err := b.writer.Write(p)
	if err != nil {
		markFailed(b.request)
	}
	return n, err
}

func newBridge(c config, key string) (*bridge, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	u, err := upstreamURL(c.upstream)
	if err != nil {
		return nil, err
	}
	b := &bridge{phase: "open", changed: make(chan struct{}), maxQueue: c.maxQueue, maxWait: c.maxWait, keyHash: sha256.Sum256([]byte("Bearer " + key))}
	// Fresh HTTP/1 connections eliminate Transport's reused-connection retries.
	// No redirects, body buffering/replay, HTTP/2 retries, or proxy-from-env.
	b.transport = &http.Transport{DisableKeepAlives: true, DisableCompression: true}
	b.proxy = &httputil.ReverseProxy{
		Transport:     b.transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = u.Scheme, u.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = pr.In.URL.Path, pr.In.URL.RawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL.Opaque = ""
			pr.Out.GetBody = nil
			// Rewrite removes client-supplied Forwarded/X-Forwarded-*.
			// Record only the actual peer; never bless a spoofed chain.
			pr.Out.Header.Del("X-Real-IP")
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			markFailed(r)
			http.Error(w, "upstream request failed; outcome may be unknown", http.StatusBadGateway)
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode >= 400 {
				markFailed(r.Request)
			}
			body := &observedBody{ReadCloser: r.Body, request: r.Request}
			if rw, ok := r.Body.(io.ReadWriteCloser); ok && r.StatusCode == http.StatusSwitchingProtocols {
				r.Body = &observedUpgrade{observedBody: body, writer: rw}
			} else {
				r.Body = body
			}
			return nil
		},
	}
	return b, nil
}

func (b *bridge) snapshotLocked() status {
	// Ready means control-plane preflight is available, NOT upstream readiness.
	// Drained only covers traffic through this bridge, not old direct listeners.
	var holdMS int64
	if b.phase == "held" {
		holdMS = time.Since(b.heldSince).Milliseconds()
	}
	return status{
		Phase: b.phase, Ready: b.phase != "stopping", Drained: b.phase == "held" && b.active == 0,
		Active: b.active, Queued: b.queued, Served: b.served, Errors: b.errors,
		HoldDurationMS: holdMS, MaxQueue: b.maxQueue, MaxWaitMS: b.maxWait.Milliseconds(),
	}
}

func (b *bridge) snapshot() status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

func (b *bridge) transition(phase string) (status, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.phase == "stopping" {
		return b.snapshotLocked(), false
	}
	if b.phase != phase {
		b.phase = phase
		if phase == "held" {
			b.heldSince = time.Now()
		} else {
			b.heldSince = time.Time{}
		}
		close(b.changed)
		b.changed = make(chan struct{})
	}
	return b.snapshotLocked(), true
}

func (b *bridge) admit(ctx context.Context) bool {
	b.mu.Lock()
	if ctx.Err() != nil || b.phase == "stopping" {
		b.mu.Unlock()
		return false
	}
	if b.phase == "open" {
		b.active++
		b.activeWG.Add(1)
		b.mu.Unlock()
		return true
	}
	if b.queued >= b.maxQueue {
		b.mu.Unlock()
		return false
	}
	b.queued++
	deadline := time.Now().Add(b.maxWait)
	timer := time.NewTimer(b.maxWait)
	defer timer.Stop()
	for {
		if ctx.Err() != nil || !time.Now().Before(deadline) || b.phase == "stopping" {
			b.queued--
			b.mu.Unlock()
			return false
		}
		if b.phase == "open" {
			b.queued--
			b.active++
			b.activeWG.Add(1)
			b.mu.Unlock()
			return true
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-changed:
		}
		b.mu.Lock()
	}
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.admit(r.Context()) {
		b.mu.Lock()
		b.errors++
		b.mu.Unlock()
		// No upstream attempt has occurred: this rejection is safe to retry.
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Connection", "close")
		http.Error(w, "deployment gate unavailable; request not forwarded", http.StatusServiceUnavailable)
		return
	}
	a := &attempt{}
	defer func() {
		p := recover()
		if p != nil {
			a.failed.Store(true)
		}
		b.mu.Lock()
		b.active--
		b.served++ // Completed upstream attempts, including unsuccessful ones.
		if a.failed.Load() {
			b.errors++
		}
		b.mu.Unlock()
		b.activeWG.Done()
		// Preserve ReverseProxy's stream-abort semantics; never append a new
		// error response to an already-started stream and never retry it.
		if p != nil {
			panic(p)
		}
	}()
	b.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), attemptKey{}, a)))
}

func (b *bridge) control(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(provided[:], b.keyHash[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// There is deliberately no configurable target, query, or request body.
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawPath != "" {
		w.Header().Set("Connection", "close")
		http.Error(w, "control parameters are not supported", http.StatusBadRequest)
		return
	}
	var s status
	switch r.URL.Path {
	case "/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s = b.snapshot()
	case "/hold", "/resume":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		phase := "held"
		if r.URL.Path == "/resume" {
			phase = "open"
		}
		var ok bool
		s, ok = b.transition(phase)
		if !ok {
			http.Error(w, "bridge is stopping", http.StatusConflict)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

func run(ctx context.Context, c config) error {
	if err := c.validate(); err != nil {
		return err
	}
	key, err := loadControlKey(c.keyFile)
	if err != nil {
		return err
	}
	b, err := newBridge(c, key)
	if err != nil {
		return err
	}
	dataListener, err := net.Listen("tcp", c.listen)
	if err != nil {
		return errors.New("cannot bind data listener")
	}
	defer dataListener.Close()
	controlListener, err := net.Listen("tcp", c.control)
	if err != nil {
		return errors.New("cannot bind control listener")
	}
	defer controlListener.Close()
	// Do not use the signal context as BaseContext: active streams must survive
	// shutdown until the clients/upstream finish. No forced drain deadline.
	data := &http.Server{Handler: b, ErrorLog: log.New(io.Discard, "", 0)}
	control := &http.Server{Handler: http.HandlerFunc(b.control), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	stopped := make(chan error, 2)
	go func() { stopped <- data.Serve(dataListener) }()
	go func() { stopped <- control.Serve(controlListener) }()
	select {
	case <-ctx.Done():
	case err = <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	b.transition("stopping") // Atomically prevents further activeWG additions.
	_ = data.Shutdown(context.Background())
	b.activeWG.Wait() // Server.Shutdown does not wait for hijacked WebSockets.
	_ = control.Shutdown(context.Background())
	b.transport.CloseIdleConnections()
	return err
}

func main() {
	c, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = run(ctx, c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cpa-release-bridge:", err)
		os.Exit(1)
	}
}

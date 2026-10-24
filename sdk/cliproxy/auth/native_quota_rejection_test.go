package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
)

const nativeQuotaTOSFixture = `{"error":{"status":"PERMISSION_DENIED","message":"private-upstream-body","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","domain":"cloudcode-pa.googleapis.com"}]}}`

type nativeQuotaRejectionStore struct {
	mu         sync.Mutex
	saved      []*Auth
	fail       bool
	empty      bool
	beforeSave func(context.Context, *Auth)
}

func (s *nativeQuotaRejectionStore) Save(ctx context.Context, a *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beforeSave != nil {
		s.beforeSave(ctx, a)
	}
	if s.fail {
		return "", errors.New("private-store-error")
	}
	if s.empty {
		return "", nil
	}
	s.saved = append(s.saved, a.Clone())
	return a.ID, nil
}
func (s *nativeQuotaRejectionStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *nativeQuotaRejectionStore) Delete(context.Context, string) error  { return nil }

func nativeQuotaRejectionManager(t *testing.T, mode nativequota.Mode, provider string) (*Manager, *nativeQuotaTestExecutor, *nativeQuotaRejectionStore, *Auth) {
	t.Helper()
	store := &nativeQuotaRejectionStore{}
	m := NewManager(store, nil, nil)
	cfg := nativequota.DefaultConfig()
	cfg.Mode = mode
	m.nativeQuota.Store(nativequota.New(cfg, nil))
	exec := &nativeQuotaTestExecutor{provider: provider}
	m.RegisterExecutor(exec)
	a, err := m.Register(WithSkipPersist(context.Background()), &Auth{
		ID: "tos-fixture", Provider: provider, Status: StatusActive,
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, "priority": "17"},
		Metadata:   map[string]any{"type": provider, "account_id": "fixture-principal", "access_token": "fixture-access", "refresh_token": "fixture-refresh", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "notes": "operator note"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, exec, store, a
}

func TestNativeQuotaTOSDisablesExactlyOnce(t *testing.T) {
	m, exec, store, a := nativeQuotaRejectionManager(t, nativequota.ModeEnforce, "antigravity")
	reads := 0
	exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		reads++
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(nativeQuotaTOSFixture))}, nil
	}
	store.beforeSave = func(ctx context.Context, candidate *Auth) {
		if ctx.Err() != nil || m.auths[a.ID].Disabled {
			t.Error("published or canceled before persistence")
		}
		if candidate.Metadata["type"] != "antigravity" || candidate.Metadata["disabled"] != true {
			t.Error("missing durable disabled metadata")
		}
	}
	_, err := m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
	var rejection *nativequota.RejectionError
	if !errors.As(err, &rejection) || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe/missing typed rejection: %v", err)
	}
	current, _ := m.GetByID(a.ID)
	if !current.Disabled || current.Status != StatusDisabled || current.StatusMessage != nativeQuotaTOSStatusMessage {
		t.Fatal("not disabled with stable reason")
	}
	if current.Metadata["native_quota_rejection_reason"] != nativequota.ReasonTOSViolation || current.Metadata["status_message"] != nativeQuotaTOSStatusMessage {
		t.Fatal("reason not persisted")
	}
	if current.Metadata["refresh_token"] != a.Metadata["refresh_token"] || current.Metadata["notes"] != a.Metadata["notes"] || authPriority(current) != 17 {
		t.Fatal("non-operational fields changed")
	}
	if !m.NativeQuota().Assess(a.ID, a.Provider, "", time.Now()).Disabled {
		t.Fatal("engine membership not disabled")
	}
	_, _ = m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
	if reads != 1 || len(store.saved) != 1 {
		t.Fatalf("fallback/repeated save: reads=%d saves=%d", reads, len(store.saved))
	}
}

func TestNativeQuotaTOSOnlyEnforceAntigravity(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		mode           nativequota.Mode
		status         int
		body           string
		reads          int
	}{
		{"generic403", "antigravity", nativequota.ModeEnforce, 403, `{"error":{"status":"PERMISSION_DENIED","message":"TOS_VIOLATION"}}`, 6},
		{"429", "antigravity", nativequota.ModeEnforce, 429, nativeQuotaTOSFixture, 6},
		{"shadow", "antigravity", nativequota.ModeShadow, 403, nativeQuotaTOSFixture, 1},
		{"codex", "codex", nativequota.ModeEnforce, 403, nativeQuotaTOSFixture, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, exec, store, a := nativeQuotaRejectionManager(t, tc.mode, tc.provider)
			reads := 0
			exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
				reads++
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}
			if _, err := m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a)); err == nil {
				t.Fatal("failure reported as success")
			}
			current, _ := m.GetByID(a.ID)
			if !reflect.DeepEqual(current, a) || len(store.saved) != 0 || reads != tc.reads {
				t.Fatalf("unexpected mutation/fallback: reads=%d saves=%d", reads, len(store.saved))
			}
		})
	}
}

func TestNativeQuotaTOSCurrentCloneAndReplacementRaces(t *testing.T) {
	for _, change := range []string{"token-refresh", "principal", "reregister", "shadow", "shadow-enforce", "manual-disable"} {
		t.Run(change, func(t *testing.T) {
			m, exec, store, a := nativeQuotaRejectionManager(t, nativequota.ModeEnforce, "antigravity")
			var changed *Auth
			reads := 0
			exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
				reads++
				current, _ := m.GetByID(a.ID)
				var err error
				switch change {
				case "token-refresh":
					current.Metadata["access_token"] = "rotated-access"
					current.Metadata["refresh_token"] = "rotated-refresh"
					current.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
					current.Metadata["notes"] = "concurrent operator note"
					current.Attributes["priority"] = "29"
					changed, err = m.Update(WithSkipPersist(context.Background()), current)
				case "principal":
					current.Metadata["account_id"] = "replacement-principal"
					changed, err = m.Update(WithSkipPersist(context.Background()), current)
				case "reregister":
					changed, err = m.Register(WithSkipPersist(context.Background()), current)
				case "manual-disable":
					current.Disabled, current.Status, current.StatusMessage = true, StatusDisabled, "operator disabled"
					changed, err = m.Update(WithSkipPersist(context.Background()), current)
				case "shadow", "shadow-enforce":
					cfg := m.NativeQuota().Config()
					cfg.Mode = nativequota.ModeShadow
					err = m.NativeQuota().Configure(cfg)
					if change == "shadow-enforce" {
						cfg.Mode = nativequota.ModeEnforce
						err = m.NativeQuota().Configure(cfg)
					}
					changed = current
				}
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(nativeQuotaTOSFixture))}, nil
			}
			_, err := m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
			if err == nil || reads != 1 {
				t.Fatal("rejection was not terminal")
			}
			current, _ := m.GetByID(a.ID)
			if change == "token-refresh" {
				if !current.Disabled || current.Metadata["access_token"] != changed.Metadata["access_token"] || current.Metadata["refresh_token"] != changed.Metadata["refresh_token"] || current.Metadata["expired"] != changed.Metadata["expired"] || current.Metadata["notes"] != changed.Metadata["notes"] || authPriority(current) != 29 || len(store.saved) != 1 {
					t.Fatal("stale snapshot clobbered concurrent changes")
				}
			} else if !reflect.DeepEqual(current, changed) || len(store.saved) != 0 {
				t.Fatal("stale rejection changed current auth")
			}
		})
	}
}

func TestNativeQuotaTOSPersistenceFailureAndRetry(t *testing.T) {
	for _, kind := range []string{"error", "empty", "nil-store", "runtime-only", "skip-persist"} {
		t.Run(kind, func(t *testing.T) {
			m, exec, store, a := nativeQuotaRejectionManager(t, nativequota.ModeEnforce, "antigravity")
			exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(nativeQuotaTOSFixture))}, nil
			}
			ctx := context.Background()
			switch kind {
			case "error":
				store.fail = true
			case "empty":
				store.empty = true
			case "nil-store":
				m.store = nil
			case "runtime-only":
				a.Attributes["runtime_only"] = "true"
				a, _ = m.Update(WithSkipPersist(ctx), a)
			case "skip-persist":
				ctx = WithSkipPersist(ctx)
			}
			_, err := m.fetchNativeQuota(ctx, nativeQuotaAccount(a))
			if err == nil || !strings.Contains(err.Error(), "persistence") || strings.Contains(err.Error(), "private") {
				t.Fatalf("missing safe persistence error: %v", err)
			}
			current, _ := m.GetByID(a.ID)
			if !reflect.DeepEqual(a, current) || len(store.saved) != 0 {
				t.Fatal("failed save falsely installed disable")
			}
			if kind == "error" || kind == "empty" {
				store.fail, store.empty = false, false
				_, _ = m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
				current, _ = m.GetByID(a.ID)
				if !current.Disabled || len(store.saved) != 1 {
					t.Fatal("failed save prevented retry")
				}
			}
		})
	}
}

func TestNativeQuotaTOSBusyStopLockAndCanceledRead(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		m, exec, store, a := nativeQuotaRejectionManager(t, nativequota.ModeEnforce, "antigravity")
		ctx, cancel := context.WithCancel(context.Background())
		exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
			if canceled {
				cancel()
			} else {
				m.nativeQuotaMu.Lock()
			}
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(nativeQuotaTOSFixture))}, nil
		}
		done := make(chan error, 1)
		go func() { _, err := m.fetchNativeQuota(ctx, nativeQuotaAccount(a)); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("guard unexpectedly succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("collector blocked behind stop lock")
		}
		if !canceled {
			m.nativeQuotaMu.Unlock()
		}
		cancel()
		current, _ := m.GetByID(a.ID)
		if current.Disabled || len(store.saved) != 0 {
			t.Fatal("canceled/busy collector mutated auth")
		}
	}
}

func TestNativeQuotaTOSConcurrentRefreshFailureKeepsReason(t *testing.T) {
	m, exec, _, a := nativeQuotaRejectionManager(t, nativequota.ModeEnforce, "antigravity")
	started, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	exec.refresh = func(context.Context, *Auth) (*Auth, error) {
		close(started)
		<-release
		return nil, errors.New("fixture refresh failure")
	}
	exec.read = func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(nativeQuotaTOSFixture))}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := m.refreshAuthForRequest(context.Background(), a.ID, authAccessToken(a)); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	_, _ = m.fetchNativeQuota(context.Background(), nativeQuotaAccount(a))
	disabled, _ := m.GetByID(a.ID)
	if !disabled.Disabled {
		t.Fatal("rejection did not disable")
	}
	released.Store(true)
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Error("refresh did not fail")
		}
	case <-time.After(time.Second):
		t.Fatal("refresh blocked")
	}
	current, _ := m.GetByID(a.ID)
	if !reflect.DeepEqual(disabled, current) {
		t.Fatal("late refresh failure changed quarantine")
	}
}

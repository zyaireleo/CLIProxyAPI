package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type nativeQuotaLatencyContextKey struct{}
type nativeQuotaAttemptContextKey struct{}

// nativeQuotaAttemptContext binds feedback to the dispatched principal, never
// to a later credential found by ID. Only safe value fields enter the context.
func (m *Manager) nativeQuotaAttemptContext(ctx context.Context, auth *Auth) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	var account nativequota.Account
	if auth != nil && auth.AuthKind() != AuthKindAPIKey && m.nativeQuotaFor(auth.Provider, false) != nil {
		account = nativeQuotaAccount(auth)
	}
	if account.ID == "" && ctx.Value(nativeQuotaAttemptContextKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, nativeQuotaAttemptContextKey{}, account)
}

func nativeQuotaAttemptFromContext(ctx context.Context) (nativequota.Account, bool) {
	if ctx == nil {
		return nativequota.Account{}, false
	}
	account, ok := ctx.Value(nativeQuotaAttemptContextKey{}).(nativequota.Account)
	return account, ok && account.ID != "" && account.Revision != ""
}

func (m *Manager) nativeQuotaWaitContext(ctx context.Context, providers []string) context.Context {
	if !m.nativeQuotaEnforcesAny(providers) {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return nativequota.WithWaitBudget(ctx)
}

func isNativeQuotaAdmissionError(err error) bool {
	return errors.Is(err, nativequota.ErrAdmissionDenied)
}

func (m *Manager) acquireNativeQuota(ctx context.Context, auth *Auth, model string) (func(), error) {
	if auth == nil || auth.AuthKind() == AuthKindAPIKey {
		return func() {}, nil
	}
	e := m.nativeQuotaFor(auth.Provider, true)
	if e == nil {
		return func() {}, nil
	}
	// Selection is not a reservation. Recheck manual lifecycle state immediately
	// before admission so a concurrent disable cannot launch a new attempt.
	current, ok := m.GetByID(auth.ID)
	if !ok || current.Disabled || current.Status == StatusDisabled {
		return nil, wrapRequestStopError(&nativequota.AdmissionError{Reason: "manual_disabled"})
	}
	account := nativeQuotaAccount(auth)
	if nativeQuotaAccount(current).Revision != account.Revision {
		return nil, wrapRequestStopError(&nativequota.AdmissionError{Reason: "account_changed"})
	}
	release, err := e.AcquireForRevision(ctx, account.ID, account.Provider, canonicalModelKey(model), account.Revision)
	if err != nil {
		return nil, wrapRequestStopError(err)
	}
	var once sync.Once
	finish := func() { once.Do(release) }
	stop := context.AfterFunc(ctx, finish)
	return func() { stop(); finish() }, nil
}

func (m *Manager) executeNativeQuotaAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = m.nativeQuotaAttemptContext(ctx, auth)
	release, err := m.acquireNativeQuota(ctx, auth, req.Model)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer release()
	resp, err := executor.Execute(ctx, auth, req, opts)
	if !isLocalPacingSkipError(err) {
		m.observeNativeQuotaHeaders(ctx, resp.Headers, time.Now())
	}
	return resp, err
}

func (m *Manager) executeNativeQuotaStreamAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx = m.nativeQuotaAttemptContext(ctx, auth)
	// Preserve the exact old stream channel in off/shadow. Shadow only consumes
	// quota response evidence; it never acquires or tracks generation leases.
	if auth == nil || auth.AuthKind() == AuthKindAPIKey || m.nativeQuotaFor(auth.Provider, true) == nil {
		stream, err := executor.ExecuteStream(ctx, auth, req, opts)
		if stream != nil && !isLocalPacingSkipError(err) {
			m.observeNativeQuotaHeaders(ctx, stream.Headers, time.Now())
		}
		return stream, err
	}
	release, err := m.acquireNativeQuota(ctx, auth, req.Model)
	if err != nil {
		return nil, err
	}
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if stream != nil && !isLocalPacingSkipError(err) {
		m.observeNativeQuotaHeaders(ctx, stream.Headers, time.Now())
	}
	if err != nil || stream == nil || stream.Chunks == nil {
		release()
		return stream, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-stream.Chunks:
				if !ok {
					return
				}
				// A terminal provider error ends the upstream attempt even when
				// the caller stops reading before consuming that error frame.
				if chunk.Err != nil {
					release()
				}
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
				if chunk.Err != nil {
					return
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: stream.Headers, Chunks: out}, nil
}

func (m *Manager) observeNativeQuotaHeaders(ctx context.Context, headers http.Header, now time.Time) {
	account, ok := nativeQuotaAttemptFromContext(ctx)
	if !ok || account.Provider != "codex" || len(headers) == 0 {
		return
	}
	e := m.nativeQuotaFor(account.Provider, false)
	if e == nil {
		return
	}
	if ob, err := nativequota.ParseCodexHeaders(headers, account.ID, now); err == nil {
		e.ObserveForRevision(ob, account.Revision)
	}
}

func (m *Manager) recordNativeQuotaResult(ctx context.Context, result Result) {
	if result.SkipQuotaObservation || result.AuthID == "" || IsNativeQuotaRequest(ctx) || (ctx != nil && ctx.Err() != nil) {
		return
	}
	if result.Error != nil && (strings.Contains(strings.ToLower(result.Error.Code), "local") || isNativeQuotaAdmissionError(result.Error)) {
		return
	}
	e := m.nativeQuotaFor(result.Provider, false)
	if e == nil {
		return
	}
	account, ok := nativeQuotaAttemptFromContext(ctx)
	if !ok || account.ID != result.AuthID || !strings.EqualFold(account.Provider, result.Provider) {
		return
	}
	now := time.Now()
	m.observeNativeQuotaHeaders(ctx, internallogging.GetResponseHeaders(ctx), now)
	status := statusCodeFromResult(result.Error)
	if result.Success {
		status = http.StatusOK
	}
	retryAfter := time.Duration(0)
	if result.RetryAfter != nil {
		retryAfter = *result.RetryAfter
	}
	var latency time.Duration
	if ctx != nil {
		latency, _ = ctx.Value(nativeQuotaLatencyContextKey{}).(time.Duration)
	}
	e.RecordResultWithLatencyForRevision(account.ID, account.Provider, canonicalModelKey(result.Model), account.Revision, status, result.Success, retryAfter, latency)
}

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
)

const (
	nativeQuotaAGDailyURL   = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	nativeQuotaAGSandboxURL = "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary"
	nativeQuotaAGMainURL    = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	nativeQuotaCodexURL     = "https://chatgpt.com/backend-api/wham/usage"
	nativeQuotaAGUserAgent  = "antigravity/hub/2.9.1 darwin/arm64"
	nativeQuotaMaxResponse  = 2 << 20
	// This bounds background quota probes only, never generation or streams.
	nativeQuotaCollectionTimeout = 30 * time.Second
)

func (m *Manager) fetchNativeQuota(ctx context.Context, account nativequota.Account) (nativequota.Observation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, nativeQuotaCollectionTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, nativeQuotaRequestContextKey{}, true)
	a, ok := m.GetByID(account.ID)
	if !ok || a == nil || a.Disabled || a.Status == StatusDisabled || a.AuthKind() == AuthKindAPIKey || !strings.EqualFold(a.Provider, account.Provider) {
		return nativequota.Observation{}, fmt.Errorf("native-quota: account unavailable")
	}
	e := m.nativeQuotaFor(a.Provider, false)
	if e == nil {
		return nativequota.Observation{}, fmt.Errorf("native-quota: provider inactive")
	}
	if nativeQuotaAccount(a).Revision != account.Revision {
		return nativequota.Observation{}, fmt.Errorf("native-quota: account changed")
	}
	// A shadow read cannot initiate credential refresh or recover a quota 401.
	// The ordinary auto-refresh lifecycle remains the sole owner in shadow.
	rejectionAuthorization := e.AuthorizeAntigravityRejection()
	allowRefresh := e.Config().Mode == nativequota.ModeEnforce
	if !allowRefresh {
		rejectionAuthorization = nativequota.RejectionAuthorization{}
	}
	if rt := m.roundTripperFor(a); rt != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, rt)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", rt)
	}
	// Reuse the manager's existing refresh serialization and persistence. Never
	// exchange OAuth tokens in this collector or send a generation probe.
	now := time.Now()
	expiry, knownExpiry := a.AccessTokenExpirationTime()
	validToken := a.HasValidAccessToken(now)
	if !allowRefresh && !validToken {
		return nativequota.Observation{}, fmt.Errorf("native-quota: credentials require ordinary refresh")
	}
	needsRefresh := allowRefresh && (!validToken || (knownExpiry && !expiry.After(now.Add(2*time.Minute))))
	if needsRefresh {
		if !authHasRefreshCredential(a) {
			return nativequota.Observation{}, fmt.Errorf("native-quota: credentials require refresh")
		}
		var err error
		a, err = m.refreshNativeQuotaAuth(ctx, a, allowRefresh)
		if err != nil || a == nil {
			return nativequota.Observation{}, fmt.Errorf("native-quota: managed credential refresh failed")
		}
	}
	if strings.EqualFold(a.Provider, "codex") {
		raw, err := m.nativeQuotaRead(ctx, a, http.MethodGet, nativeQuotaCodexURL, nil, allowRefresh)
		if err != nil {
			return nativequota.Observation{}, err
		}
		return nativequota.ParseCodexUsage(raw, a.ID, time.Now())
	}
	if !strings.EqualFold(a.Provider, "antigravity") {
		return nativequota.Observation{}, fmt.Errorf("native-quota: unsupported provider")
	}
	body := map[string]string{}
	if project, okProject := a.Metadata["project_id"].(string); okProject && strings.TrimSpace(project) != "" {
		body["project"] = project
	}
	payload, _ := json.Marshal(body)
	var lastErr error
	var collected nativequota.Observation
	endpoints := []string{nativeQuotaAGDailyURL, nativeQuotaAGSandboxURL, nativeQuotaAGMainURL}
	for _, endpoint := range append(append([]string{}, endpoints...), nativeQuotaAGModelsURLs()...) {
		raw, err := m.nativeQuotaRead(ctx, a, http.MethodPost, endpoint, payload, allowRefresh)
		if err != nil {
			var rejection *nativequota.RejectionError
			if errors.As(err, &rejection) {
				// A confirmed structured refusal is terminal, even after partial
				// quota evidence. Never probe another endpoint to restore it.
				if errApply := m.applyNativeQuotaRejection(ctx, a, rejectionAuthorization); errApply != nil {
					return nativequota.Observation{}, errors.Join(err, errApply)
				}
				return nativequota.Observation{}, err
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		ob, err := nativequota.ParseAntigravity(raw, a.ID, time.Now())
		if err != nil {
			lastErr = err
			continue
		}
		mergeNativeQuotaAG(&collected, ob)
		if completeNativeQuotaAG(collected) {
			return collected, nil
		}
		lastErr = fmt.Errorf("native-quota: incomplete antigravity quota response")
	}
	// Real partial evidence is retained with its original age and scope. The
	// engine merges it instead of erasing existing weekly or sibling balances.
	if len(collected.Pools) != 0 {
		collected.Partial = true
		return collected, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("native-quota: quota unavailable")
	}
	return nativequota.Observation{}, lastErr
}

func nativeQuotaAGModelsURLs() []string {
	return []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	}
}

func completeNativeQuotaAG(ob nativequota.Observation) bool {
	known := map[string]bool{}
	for _, p := range ob.Pools {
		short, weekly := false, false
		for _, w := range p.Windows {
			if w.Remaining == nil && !w.Blocked && !w.Disabled {
				continue
			}
			if w.DurationSeconds == 5*60*60 || w.ID == "short" {
				short = true
			}
			if w.DurationSeconds == 7*24*60*60 || w.ID == "weekly" {
				weekly = true
			}
		}
		known[p.ID] = short && weekly
	}
	return known["gemini"] && known["claude_gpt"]
}

func mergeNativeQuotaAG(dst *nativequota.Observation, src nativequota.Observation) {
	if dst.AuthID == "" {
		*dst = src
		dst.Partial = true
		return
	}
	// Prefer evidence from the earlier, higher-priority quota endpoint. Fallback
	// fills omissions but cannot erase a real weekly window or exact association.
	for _, incoming := range src.Pools {
		index := -1
		for i := range dst.Pools {
			if dst.Pools[i].ID == incoming.ID {
				index = i
				break
			}
		}
		if index < 0 {
			dst.Pools = append(dst.Pools, incoming)
			continue
		}
		old := &dst.Pools[index]
		if len(old.Windows) == 0 {
			*old = incoming
			continue
		}
		for _, window := range incoming.Windows {
			found := false
			for i, existing := range old.Windows {
				if existing.ID == window.ID {
					found = true
					if existing.Remaining == nil && !existing.Blocked {
						old.Windows[i] = window
					}
					break
				}
			}
			if !found {
				old.Windows = append(old.Windows, window)
			}
		}
	}
	if src.ObservedAt.After(dst.ObservedAt) {
		dst.ObservedAt = src.ObservedAt
	}
	dst.Partial = true
}

func (m *Manager) validateNativeQuotaRead(ctx context.Context, auth *Auth, requireEnforce bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := m.nativeQuotaFor(auth.Provider, false)
	if e == nil || (requireEnforce && e.Config().Mode != nativequota.ModeEnforce) {
		return fmt.Errorf("native-quota: provider inactive")
	}
	current, ok := m.GetByID(auth.ID)
	if !ok || current.Disabled || current.Status == StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || nativeQuotaAccount(current).Revision != nativeQuotaAccount(auth).Revision {
		return fmt.Errorf("native-quota: account changed")
	}
	return nil
}

func (m *Manager) refreshNativeQuotaAuth(ctx context.Context, auth *Auth, allowRefresh bool) (*Auth, error) {
	if !allowRefresh {
		return nil, fmt.Errorf("native-quota: ordinary refresh owns shadow credentials")
	}
	if err := m.validateNativeQuotaRead(ctx, auth, true); err != nil {
		return nil, err
	}
	ctx = m.nativeQuotaAttemptContext(ctx, auth)
	updated, err := m.refreshAuthForRequest(ctx, auth.ID, authAccessToken(auth))
	if err != nil {
		return nil, err
	}
	if updated == nil || nativeQuotaAccount(updated).Revision != nativeQuotaAccount(auth).Revision {
		return nil, fmt.Errorf("native-quota: account changed")
	}
	if err = m.validateNativeQuotaRead(ctx, auth, true); err != nil {
		return nil, err
	}
	return updated, nil
}

func (m *Manager) nativeQuotaRead(ctx context.Context, auth *Auth, method, endpoint string, body []byte, allowRefresh bool) ([]byte, error) {
	allowed := endpoint == nativeQuotaAGDailyURL || endpoint == nativeQuotaAGSandboxURL || endpoint == nativeQuotaAGMainURL || endpoint == nativeQuotaCodexURL
	for _, modelsURL := range nativeQuotaAGModelsURLs() {
		allowed = allowed || endpoint == modelsURL
	}
	if !allowed {
		return nil, fmt.Errorf("native-quota: endpoint not allowed")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.validateNativeQuotaRead(ctx, auth, false); err != nil {
			return nil, err
		}
		if !auth.HasValidAccessToken(time.Now()) {
			return nil, fmt.Errorf("native-quota: credentials require ordinary refresh")
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("native-quota: invalid request")
		}
		req.Header.Set("Accept", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", nativeQuotaAGUserAgent)
		} else {
			req.Header.Set("User-Agent", "codex-cli")
			if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
				req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
			}
		}
		resp, err := m.HttpRequest(ctx, auth, req)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return nil, fmt.Errorf("native-quota: quota transport failed")
		}
		if resp == nil || resp.Body == nil {
			return nil, fmt.Errorf("native-quota: empty quota response")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, nativeQuotaMaxResponse+1))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && allowRefresh && authHasRefreshCredential(auth) {
			updated, refreshErr := m.refreshNativeQuotaAuth(ctx, auth, allowRefresh)
			if refreshErr != nil || updated == nil {
				return nil, fmt.Errorf("native-quota: managed credential refresh failed")
			}
			auth = updated
			continue
		}
		if readErr == nil && len(raw) <= nativeQuotaMaxResponse && strings.EqualFold(auth.Provider, "antigravity") && endpoint != nativeQuotaCodexURL &&
			nativequota.IsAntigravityTOSViolation(resp.StatusCode, raw) {
			return nil, &nativequota.RejectionError{}
		}
		if err := m.validateNativeQuotaRead(ctx, auth, false); err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("native-quota: quota response status %d", resp.StatusCode)
		}
		if readErr != nil || len(raw) > nativeQuotaMaxResponse {
			return nil, fmt.Errorf("native-quota: invalid quota response size")
		}
		return raw, nil
	}
	return nil, fmt.Errorf("native-quota: quota authorization failed")
}

package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex device fingerprint convergence ("device" mode).
//
// When enabled, every Codex OAuth request routed through an auth presents one
// stable UUIDv4 installation id upstream: client_metadata.x-codex-installation-id,
// the installation_id inside x-codex-turn-metadata (body and header) are
// rewritten, and any X-Codex-Installation-Id header is removed. Session, thread,
// window, turn and prompt cache identifiers are left untouched.

const (
	codexDeviceFPModeDevice = "device"

	codexDeviceFPAttrMode           = "codex_device_fingerprint"
	codexDeviceFPAttrInstallationID = "codex_installation_id"

	codexDeviceFPSeedPrefix = "cli-proxy-api:codex:device-fingerprint:v2:"

	codexDeviceFPSourceExplicit    = "explicit"
	codexDeviceFPSourceAccountUser = "account-user"
	codexDeviceFPSourceAccount     = "account"
	codexDeviceFPSourceAuthID      = "auth-id"
	codexDeviceFPSourceSIWCDevice  = "siwc-device"

	codexInstallationIDKey      = "x-codex-installation-id"
	codexTurnMetadataKey        = "x-codex-turn-metadata"
	codexTurnInstallationIDKey  = "installation_id"
	codexInstallationIDHeader   = "X-Codex-Installation-Id"
	codexTurnMetadataHeaderName = "X-Codex-Turn-Metadata"
)

// codexTeamLikePlans lists plan types whose workspaces may share one account_id
// across members, so the per-user id is mixed into the seed. The plan names are
// not verified against upstream; keep them here as the single source.
var codexTeamLikePlans = map[string]struct{}{
	"team":       {},
	"business":   {},
	"enterprise": {},
	"edu":        {},
}

type codexDeviceIdentity struct {
	InstallationID string
	Source         string // explicit | account-user | account | auth-id | siwc-device
	AllowInject    bool   // true only for non-SIWC requests with inject-missing enabled
}

// codexDeviceStickyIdentity remembers the last non-empty account identity seen
// for an auth so a refresh that temporarily clears account_id/id_token cannot
// flip the device id within a process.
type codexDeviceStickyIdentity struct {
	accountID string
	userID    string
}

type codexDeviceIDTokenClaims struct {
	tokenHash string
	accountID string
	userID    string
}

var (
	codexDeviceStickyMu sync.Mutex
	codexDeviceSticky   = map[string]codexDeviceStickyIdentity{}

	codexDeviceIDTokenMu    sync.Mutex
	codexDeviceIDTokenCache = map[string]codexDeviceIDTokenClaims{}

	codexDeviceFPWarnOnce  sync.Map // authID + "\x00" + kind -> struct{}
	codexDeviceFPInfoState sync.Map // authID -> installation id last announced

	codexDeviceFPOuterEncodingMismatch atomic.Int64
)

// resolveCodexDeviceIdentity returns the converged installation identity for the
// request, or false when device mode is not active for the auth.
func resolveCodexDeviceIdentity(cfg *config.Config, auth *cliproxyauth.Auth, useSIWC bool) (codexDeviceIdentity, bool) {
	if auth == nil || codexAuthUsesAPIKey(auth) {
		return codexDeviceIdentity{}, false
	}
	if codexDeviceFPMode(cfg, auth) != codexDeviceFPModeDevice {
		return codexDeviceIdentity{}, false
	}
	var identity codexDeviceIdentity
	if useSIWC {
		if deviceID, ok := codexSIWCDeviceID(auth); ok {
			identity = codexDeviceIdentity{InstallationID: deviceID, Source: codexDeviceFPSourceSIWCDevice}
		}
	}
	if identity.InstallationID == "" {
		id, source, ok := codexDeriveDeviceID(auth)
		if !ok {
			return codexDeviceIdentity{}, false
		}
		identity = codexDeviceIdentity{InstallationID: id, Source: source}
	}
	if !useSIWC {
		identity.AllowInject = codexDeviceFPInjectMissing(cfg)
	}
	codexDeviceFPAnnounce(cfg, auth, identity)
	return identity, true
}

// codexDeviceKey returns the installation id the request presents upstream, or
// "" when device mode is off. Websocket sessions record it per connection.
func codexDeviceKey(cfg *config.Config, auth *cliproxyauth.Auth, useSIWC bool) string {
	identity, ok := resolveCodexDeviceIdentity(cfg, auth, useSIWC)
	if !ok {
		return ""
	}
	return identity.InstallationID
}

// applyCodexDeviceFingerprintRequestHeaders rewrites upstream headers when
// device mode is active for the auth.
func applyCodexDeviceFingerprintRequestHeaders(headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth, useSIWC bool) {
	if identity, ok := resolveCodexDeviceIdentity(cfg, auth, useSIWC); ok {
		applyCodexDeviceFingerprintHeaders(headers, identity.InstallationID)
	}
}

func codexDeviceFPMode(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if raw := strings.TrimSpace(auth.Attributes[codexDeviceFPAttrMode]); raw != "" {
			return strings.ToLower(raw)
		}
		if raw, ok := auth.Metadata[codexDeviceFPAttrMode].(string); ok && strings.TrimSpace(raw) != "" {
			return strings.ToLower(strings.TrimSpace(raw))
		}
	}
	if cfg == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Codex.DeviceFingerprintMode))
}

func codexDeviceFPInjectMissing(cfg *config.Config) bool {
	return cfg != nil && cfg.Codex.DeviceFingerprintInjectMissing != nil && *cfg.Codex.DeviceFingerprintInjectMissing
}

func codexSIWCDeviceID(auth *cliproxyauth.Auth) (string, bool) {
	if auth == nil || auth.Metadata == nil {
		return "", false
	}
	siwcMap, ok := auth.Metadata["siwc"].(map[string]any)
	if !ok {
		return "", false
	}
	raw, ok := siwcMap["device_id"].(string)
	if !ok {
		return "", false
	}
	return codexValidV4(raw)
}

// codexDeriveDeviceID implements the seed priority: explicit override, account
// identity (with the ChatGPT user for team-like plans), then the auth ID.
func codexDeriveDeviceID(auth *cliproxyauth.Auth) (string, string, bool) {
	if auth == nil {
		return "", "", false
	}
	authID := strings.TrimSpace(auth.ID)
	for _, raw := range []string{
		strings.TrimSpace(auth.Attributes[codexDeviceFPAttrInstallationID]),
		codexMetadataString(auth.Metadata, codexDeviceFPAttrInstallationID),
	} {
		if raw == "" {
			continue
		}
		if id, ok := codexValidV4(raw); ok {
			return id, codexDeviceFPSourceExplicit, true
		}
		codexDeviceFPWarn(authID, "invalid-override", "codex device fingerprint: auth=%s has invalid codex_installation_id (must be a canonical 8-4-4-4-12 RFC 4122 UUIDv4); falling back to derived id", authID)
	}

	accountID := codexMetadataString(auth.Metadata, "account_id")
	idTokenClaims := codexDeviceIDTokenIdentity(authID, codexMetadataString(auth.Metadata, "id_token"))
	if accountID == "" {
		accountID = idTokenClaims.accountID
	}
	userID := idTokenClaims.userID
	accountID, userID = codexDeviceStickyResolve(authID, accountID, userID)

	if accountID != "" {
		planType := strings.ToLower(codexMetadataString(auth.Metadata, "plan_type"))
		if planType == "" {
			planType = strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"]))
		}
		if _, teamLike := codexTeamLikePlans[planType]; teamLike {
			if userID != "" {
				return codexStableUUIDv4(codexDeviceFPSeedPrefix + "account-user\x00" + accountID + "\x00" + userID), codexDeviceFPSourceAccountUser, true
			}
			codexDeviceFPWarn(authID, "team-without-user", "codex device fingerprint: auth=%s team auth without user id; set codex_installation_id", authID)
		}
		return codexStableUUIDv4(codexDeviceFPSeedPrefix + "account\x00" + accountID), codexDeviceFPSourceAccount, true
	}
	if authID == "" {
		return "", "", false
	}
	codexDeviceFPWarn(authID, "auth-id-fallback", "codex device fingerprint: auth=%s auth lacks account identity; device id will change once account_id/id_token appears", authID)
	return codexStableUUIDv4(codexDeviceFPSeedPrefix + "auth-id\x00" + authID), codexDeviceFPSourceAuthID, true
}

func codexMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}

// codexDeviceIDTokenIdentity parses chatgpt_account_id/chatgpt_user_id from the
// id_token once per (auth, token) pair.
func codexDeviceIDTokenIdentity(authID string, idToken string) codexDeviceIDTokenClaims {
	if idToken == "" {
		return codexDeviceIDTokenClaims{}
	}
	sum := sha256.Sum256([]byte(idToken))
	tokenHash := hex.EncodeToString(sum[:16])
	codexDeviceIDTokenMu.Lock()
	cached, ok := codexDeviceIDTokenCache[authID]
	codexDeviceIDTokenMu.Unlock()
	if ok && cached.tokenHash == tokenHash {
		return cached
	}
	parsed := codexDeviceIDTokenClaims{tokenHash: tokenHash}
	if claims, errParse := codexauth.ParseJWTToken(idToken); errParse == nil && claims != nil {
		parsed.accountID = strings.TrimSpace(claims.GetAccountID())
		parsed.userID = strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID)
	}
	codexDeviceIDTokenMu.Lock()
	codexDeviceIDTokenCache[authID] = parsed
	codexDeviceIDTokenMu.Unlock()
	return parsed
}

// codexDeviceStickyResolve keeps the last non-empty account/user identity per
// auth. Empty reads reuse the cached value; a different non-empty value wins and
// is reported because it means the account actually changed.
func codexDeviceStickyResolve(authID string, accountID string, userID string) (string, string) {
	if authID == "" {
		return accountID, userID
	}
	codexDeviceStickyMu.Lock()
	defer codexDeviceStickyMu.Unlock()
	cached := codexDeviceSticky[authID]
	if accountID == "" {
		accountID = cached.accountID
	} else if cached.accountID != "" && cached.accountID != accountID {
		log.Warnf("codex device fingerprint: auth=%s account identity changed; device id follows the new account", authID)
	}
	if userID == "" {
		userID = cached.userID
	} else if cached.userID != "" && cached.userID != userID {
		log.Warnf("codex device fingerprint: auth=%s chatgpt user identity changed; device id follows the new user", authID)
	}
	codexDeviceSticky[authID] = codexDeviceStickyIdentity{accountID: accountID, userID: userID}
	return accountID, userID
}

func codexDeviceFPWarn(authID string, kind string, format string, args ...any) {
	if _, loaded := codexDeviceFPWarnOnce.LoadOrStore(authID+"\x00"+kind, struct{}{}); loaded {
		return
	}
	log.Warnf(format, args...)
}

func codexDeviceFPAnnounce(cfg *config.Config, auth *cliproxyauth.Auth, identity codexDeviceIdentity) {
	authID := strings.TrimSpace(auth.ID)
	if previous, loaded := codexDeviceFPInfoState.Swap(authID, identity.InstallationID); loaded && previous == identity.InstallationID {
		return
	}
	prefix := identity.InstallationID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	log.Infof("codex device fingerprint: auth=%s source=%s device=%s…", authID, identity.Source, prefix)
	if cfg != nil && isCodexCloakingDisabled(cfg, auth) {
		codexDeviceFPWarn(authID, "cloaking-disabled", "codex device fingerprint: auth=%s converges installation id while codex cloaking is disabled; client User-Agent is forwarded as-is", authID)
	}
}

// codexValidV4 accepts only the canonical 8-4-4-4-12 spelling of an RFC 4122
// version 4 UUID and returns it lowercased.
func codexValidV4(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return "", false
	}
	parsed, errParse := uuid.Parse(s)
	if errParse != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
		return "", false
	}
	return parsed.String(), true
}

// codexStableUUIDv4 derives a deterministic UUIDv4 from sha256(name).
func codexStableUUIDv4(name string) string {
	sum := sha256.Sum256([]byte(name))
	b := [16]byte(sum[:16])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return uuid.UUID(b).String()
}

// codexDeviceFPBodyEligible limits body rewriting to Responses endpoints.
func codexDeviceFPBodyEligible(rawURL string) bool {
	parsed, errParse := neturl.Parse(rawURL)
	if errParse != nil {
		return false
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact")
}

// applyCodexDeviceFingerprintBody rewrites the installation identity carried in
// client_metadata. Missing x-codex-installation-id is injected only when inject
// is true. Bodies that are not JSON objects, or whose client_metadata is not an
// object, are returned unchanged.
func applyCodexDeviceFingerprintBody(body []byte, id string, inject bool) []byte {
	if id == "" || len(body) == 0 {
		return body
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	clientMetadata := gjson.GetBytes(body, "client_metadata")
	if !clientMetadata.Exists() {
		if !inject {
			return body
		}
		updated, errSet := sjson.SetBytes(body, "client_metadata."+codexInstallationIDKey, id)
		if errSet != nil {
			return body
		}
		return updated
	}
	if !clientMetadata.IsObject() {
		return body
	}

	installationCount, turnMetadataCount := 0, 0
	clientMetadata.ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case codexInstallationIDKey:
			installationCount++
		case codexTurnMetadataKey:
			turnMetadataCount++
		}
		return true
	})

	if installationCount > 1 || turnMetadataCount > 1 {
		return applyCodexDeviceFingerprintBodySlow(body, clientMetadata, id, inject && installationCount == 0)
	}

	installationPath := "client_metadata." + codexInstallationIDKey
	if installationCount == 1 {
		current := clientMetadata.Get(codexInstallationIDKey)
		if current.Type != gjson.String || current.Str != id {
			if updated, errSet := sjson.SetBytes(body, installationPath, id); errSet == nil {
				body = updated
			}
		}
	} else if inject {
		if updated, errSet := sjson.SetBytes(body, installationPath, id); errSet == nil {
			body = updated
		}
	}
	if turnMetadataCount == 1 {
		turnPath := "client_metadata." + codexTurnMetadataKey
		current := gjson.GetBytes(body, turnPath)
		raw, keep, changed := rewriteCodexBodyTurnMetadata(current, id)
		switch {
		case !keep:
			if updated, errDelete := sjson.DeleteBytes(body, turnPath); errDelete == nil {
				body = updated
			}
		case changed:
			if updated, errSet := sjson.SetRawBytes(body, turnPath, raw); errSet == nil {
				body = updated
			}
		}
	}
	return body
}

// applyCodexDeviceFingerprintBodySlow rebuilds client_metadata so every
// duplicated installation key is rewritten (gjson/sjson only address the first).
func applyCodexDeviceFingerprintBodySlow(body []byte, clientMetadata gjson.Result, id string, inject bool) []byte {
	quotedID := `"` + id + `"`
	parts := make([]string, 0, 8)
	clientMetadata.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case codexInstallationIDKey:
			parts = append(parts, key.Raw+":"+quotedID)
		case codexTurnMetadataKey:
			raw, keep, changed := rewriteCodexBodyTurnMetadata(value, id)
			if !keep {
				return true
			}
			if changed {
				parts = append(parts, key.Raw+":"+string(raw))
			} else {
				parts = append(parts, key.Raw+":"+value.Raw)
			}
		default:
			parts = append(parts, key.Raw+":"+value.Raw)
		}
		return true
	})
	if inject {
		parts = append(parts, `"`+codexInstallationIDKey+`":`+quotedID)
	}
	updated, errSet := sjson.SetRawBytes(body, "client_metadata", []byte("{"+strings.Join(parts, ",")+"}"))
	if errSet != nil {
		return body
	}
	return updated
}

// rewriteCodexBodyTurnMetadata handles the JSON-string encoded turn metadata in
// the body. It returns the new raw (quoted) value, whether the key is kept, and
// whether the value changed. Non-string, invalid or non-object payloads are
// dropped instead of being rebuilt into a shape native clients never send.
func rewriteCodexBodyTurnMetadata(value gjson.Result, id string) ([]byte, bool, bool) {
	if value.Type != gjson.String {
		return nil, false, false
	}
	inner := value.Str
	updated, keep := rewriteCodexTurnMetadataInstallationID(inner, id)
	if !keep {
		return nil, false, false
	}
	if updated == inner {
		return nil, true, false
	}
	if !bytes.Equal(encodeTurnMetadataOuter(inner), []byte(value.Raw)) {
		total := codexDeviceFPOuterEncodingMismatch.Add(1)
		log.Debugf("codex device fingerprint: outer_encoding_mismatch total=%d", total)
	}
	return encodeTurnMetadataOuter(updated), true, true
}

// rewriteCodexTurnMetadataInstallationID rewrites every top-level installation_id
// in a turn metadata JSON object. keep=false means the value must be dropped
// because it is not a valid JSON object.
func rewriteCodexTurnMetadataInstallationID(inner, id string) (string, bool) {
	if !gjson.Valid(inner) {
		return "", false
	}
	parsed := gjson.Parse(inner)
	if !parsed.IsObject() {
		return "", false
	}
	count := 0
	alreadyConverged := true
	parsed.ForEach(func(key, value gjson.Result) bool {
		if key.String() == codexTurnInstallationIDKey {
			count++
			if value.Type != gjson.String || value.Str != id {
				alreadyConverged = false
			}
		}
		return true
	})
	if count == 0 || alreadyConverged {
		return inner, true
	}
	if count == 1 {
		updated, errSet := sjson.Set(inner, codexTurnInstallationIDKey, id)
		if errSet != nil {
			return "", false
		}
		return updated, true
	}
	quotedID := `"` + id + `"`
	parts := make([]string, 0, 8)
	parsed.ForEach(func(key, value gjson.Result) bool {
		if key.String() == codexTurnInstallationIDKey {
			parts = append(parts, key.Raw+":"+quotedID)
		} else {
			parts = append(parts, key.Raw+":"+value.Raw)
		}
		return true
	})
	return "{" + strings.Join(parts, ",") + "}", true
}

// encodeTurnMetadataOuter encodes s as a quoted JSON string, escaping only what
// JSON requires. Native Codex emits ASCII-only compact turn metadata, so this
// matches its outer encoding byte for byte; it never emits \u003c-style escapes.
func encodeTurnMetadataOuter(s string) []byte {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(s)+8)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		default:
			if c < 0x20 {
				out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
			} else {
				out = append(out, c)
			}
		}
	}
	out = append(out, '"')
	return out
}

// applyCodexDeviceFingerprintHeaders rewrites installation_id in every
// X-Codex-Turn-Metadata value (any key casing) and removes every
// X-Codex-Installation-Id header. Invalid turn metadata values are dropped.
func applyCodexDeviceFingerprintHeaders(headers http.Header, id string) {
	if headers == nil || id == "" {
		return
	}
	for key, values := range headers {
		if strings.EqualFold(key, codexInstallationIDHeader) {
			delete(headers, key)
			continue
		}
		if !strings.EqualFold(key, codexTurnMetadataHeaderName) {
			continue
		}
		kept := make([]string, 0, len(values))
		for _, value := range values {
			if updated, keep := rewriteCodexTurnMetadataInstallationID(value, id); keep {
				kept = append(kept, updated)
			}
		}
		if len(kept) == 0 {
			delete(headers, key)
		} else {
			headers[key] = kept
		}
	}
}

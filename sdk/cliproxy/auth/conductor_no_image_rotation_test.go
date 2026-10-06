package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type noImageRotationSelector struct{}

func (noImageRotationSelector) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	var selected *Auth
	for _, auth := range auths {
		if auth != nil && (selected == nil || auth.ID < selected.ID) {
			selected = auth
		}
	}
	return selected, nil
}

// noImageRotationExecutor returns a text-only payload plus a
// NoImageContentError for auths listed in textOnlyAuths, and an image payload
// for every other auth.
type noImageRotationExecutor struct {
	textOnlyAuths map[string]bool
}

func (*noImageRotationExecutor) Identifier() string { return "antigravity" }

func (*noImageRotationExecutor) ShouldPrepareRequestAuth(*Auth) bool { return false }

func (*noImageRotationExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *noImageRotationExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.textOnlyAuths[auth.ID] {
		cooldown := time.Minute
		return cliproxyexecutor.Response{Payload: []byte(`{"candidates":[{"content":{"parts":[{"text":"quota text from ` + auth.ID + `"}]}}]}`)},
			&cliproxyexecutor.NoImageContentError{Model: "gemini-3.1-flash-image-preview", Cooldown: &cooldown}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"from-` + auth.ID + `"}}]}}]}`)}, nil
}

func (*noImageRotationExecutor) ExecuteStream(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*noImageRotationExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*noImageRotationExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*noImageRotationExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func newNoImageRotationManager(t *testing.T, textOnlyAuths map[string]bool, authIDs ...string) *Manager {
	t.Helper()
	manager := NewManager(nil, noImageRotationSelector{}, nil)
	manager.RegisterExecutor(&noImageRotationExecutor{textOnlyAuths: textOnlyAuths})
	model := "gemini-3.1-flash-image-preview"
	for _, id := range authIDs {
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID:       id,
			Provider: "antigravity",
			Status:   StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return manager
}

// TestExecuteRotatesCredentialOnNoImageContent verifies that a text-only
// response from an image model rotates to the next credential and returns its
// image response.
func TestExecuteRotatesCredentialOnNoImageContent(t *testing.T) {
	manager := newNoImageRotationManager(t, map[string]bool{"noimage-a": true}, "noimage-a", "noimage-b")

	resp, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-3.1-flash-image-preview"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want nil (rotation should recover)", errExecute)
	}
	if !strings.Contains(string(resp.Payload), "from-noimage-b") {
		t.Errorf("payload = %s, want image response from second credential", resp.Payload)
	}
}

// TestExecuteReturnsOriginalResponseWhenAllCredentialsNoImage verifies the
// fallback: when every credential answers with text only, the original
// response is returned instead of a rotation-synthesized error.
func TestExecuteReturnsOriginalResponseWhenAllCredentialsNoImage(t *testing.T) {
	manager := newNoImageRotationManager(t, map[string]bool{"noimage-a": true, "noimage-b": true}, "noimage-a", "noimage-b")

	resp, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-3.1-flash-image-preview"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want nil (original response should be returned)", errExecute)
	}
	if !strings.Contains(string(resp.Payload), "quota text from") {
		t.Errorf("payload = %s, want original text-only response", resp.Payload)
	}
}

// TestExecuteReturnsOriginalResponseWhenSingleCredentialNoImage mirrors the
// production incident shape: only one credential exists, it answers text-only,
// and the client must still receive that response.
func TestExecuteReturnsOriginalResponseWhenSingleCredentialNoImage(t *testing.T) {
	manager := newNoImageRotationManager(t, map[string]bool{"noimage-a": true}, "noimage-a")

	resp, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-3.1-flash-image-preview"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute error = %v, want nil (original response should be returned)", errExecute)
	}
	if !strings.Contains(string(resp.Payload), "quota text from noimage-a") {
		t.Errorf("payload = %s, want original text-only response", resp.Payload)
	}
}

// TestExecuteCoolsDownNoImageCredential verifies the text-only credential is
// briefly cooled down so subsequent requests prefer other credentials.
func TestExecuteCoolsDownNoImageCredential(t *testing.T) {
	manager := newNoImageRotationManager(t, map[string]bool{"noimage-a": true}, "noimage-a", "noimage-b")

	if _, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: "gemini-3.1-flash-image-preview"}, cliproxyexecutor.Options{}); errExecute != nil {
		t.Fatalf("Execute error = %v", errExecute)
	}

	auth, ok := manager.GetByID("noimage-a")
	if !ok {
		t.Fatal("auth noimage-a not found")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Error("NextRetryAfter is zero, want cooldown from NoImageContentError.RetryAfter")
	}
}

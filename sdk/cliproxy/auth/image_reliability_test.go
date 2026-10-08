package auth

import (
	"context"
	"errors"
	"fmt"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"sync/atomic"
	"testing"
)

type imageSequenceExecutor struct {
	noImageRotationExecutor
	failures  []error
	calls     atomic.Int32
	refreshes atomic.Int32
}

func (e *imageSequenceExecutor) Execute(ctx context.Context, _ *Auth, _ core.Request, _ core.Options) (core.Response, error) {
	if err := core.ConsumeImageGeneration(ctx); err != nil {
		return core.Response{}, err
	}
	core.MarkUpstreamAttempt(ctx)
	i := int(e.calls.Add(1)) - 1
	if i < len(e.failures) {
		return core.Response{Payload: []byte(`{"candidates":[]}`)}, e.failures[i]
	}
	return core.Response{Payload: []byte(`{"image":"test success"}`)}, nil
}

func (e *imageSequenceExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshes.Add(1)
	auth.Metadata["access_token"] = "refreshed-test-token"
	return auth, nil
}

func TestImageUnauthorizedRefreshCannotReplayFifthGeneration(t *testing.T) {
	empty := &core.NoImageContentError{}
	unauthorized := &Error{HTTPStatus: 401, Message: "test unauthorized"}
	m := newNoImageRotationManager(t, nil, "refresh-1", "refresh-2", "refresh-3", "refresh-4", "refresh-5")
	for _, auth := range m.List() {
		auth.Metadata = map[string]any{"access_token": "old-test-token", "refresh_token": "test-refresh"}
		if _, err := m.Update(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	e := &imageSequenceExecutor{failures: []error{empty, empty, empty, unauthorized, empty}}
	m.RegisterExecutor(e)
	m.maxRetryCredentials.Store(6)
	_, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: "gemini-3.1-flash-image-preview"}, core.Options{})
	if !errors.Is(err, unauthorized) || e.calls.Load() != 4 || e.refreshes.Load() != 1 {
		t.Fatalf("err=%v calls=%d refreshes=%d", err, e.calls.Load(), e.refreshes.Load())
	}
}

func TestImageFailurePriorityAndTotalBudget(t *testing.T) {
	quota := &core.ImageFailure{Code: "upstream_quota_exhausted", Message: "quota", Status: 429}
	empty := &core.NoImageContentError{}
	network := errors.New("connection reset")
	policy := &core.ImageFailure{Code: "content_policy_violation", Message: "blocked", Status: 400, Stop: true}
	for _, tc := range []struct {
		name         string
		failures     []error
		want         error
		calls, limit int
	}{
		{"empty-success", []error{empty}, nil, 2, 4},
		{"quota-empty-selection", []error{quota, empty}, quota, 2, 4},
		{"empty-quota-selection", []error{empty, quota}, quota, 2, 4},
		{"empty-quota-limit", []error{empty, quota}, quota, 2, 2},
		{"quota-empty-limit", []error{quota, empty}, quota, 2, 2},
		{"network-empty", []error{network, empty}, network, 2, 4},
		{"network-empty-limit", []error{network, empty}, network, 2, 2},
		{"all-empty-limit", []error{empty, empty}, empty, 2, 2},
		{"budget", []error{empty, empty, empty, empty, empty, empty}, empty, 4, 6},
		{"policy-stops", []error{policy, empty}, policy, 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := len(tc.failures)
			if tc.want == nil {
				count++
			}
			ids := make([]string, count)
			for i := range ids {
				ids[i] = fmt.Sprintf("sequence-%s-%d", tc.name, i)
			}
			m := newNoImageRotationManager(t, nil, ids...)
			e := &imageSequenceExecutor{failures: tc.failures}
			m.RegisterExecutor(e)
			m.maxRetryCredentials.Store(int32(tc.limit))
			m.requestRetry.Store(3)
			_, err := m.Execute(context.Background(), []string{"antigravity"}, core.Request{Model: "gemini-3.1-flash-image-preview"}, core.Options{})
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if int(e.calls.Load()) != tc.calls {
				t.Fatalf("calls=%d want=%d", e.calls.Load(), tc.calls)
			}
			if tc.want == policy {
				a, _ := m.GetByID(ids[0])
				if a.Unavailable {
					t.Fatal("request rejection cooled credential")
				}
			}
		})
	}
}

func TestImageCancelledRequestMakesNoGenerationCall(t *testing.T) {
	m := newNoImageRotationManager(t, nil, "cancelled")
	e := &imageSequenceExecutor{}
	m.RegisterExecutor(e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.Execute(ctx, []string{"antigravity"}, core.Request{Model: "gemini-3.1-flash-image-preview"}, core.Options{})
	if !errors.Is(err, context.Canceled) || e.calls.Load() != 0 {
		t.Fatalf("cancelled request: err=%v calls=%d", err, e.calls.Load())
	}
}

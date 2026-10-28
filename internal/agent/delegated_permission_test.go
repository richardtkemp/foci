package agent

import (
	"context"
	"errors"
	"testing"

	"foci/internal/delegator"
)

// mockPermBackend records every decision it is handed through the unified
// delegator.PermissionResponder (#2154 Phase 3). Unused Delegator methods
// panic via the nil embed.
type mockPermBackend struct {
	delegator.Delegator

	calls      []permCall
	respondErr error
}

type permCall struct {
	requestID string
	d         delegator.PermissionDecision
}

func (m *mockPermBackend) RespondToPermission(requestID string, d delegator.PermissionDecision) error {
	m.calls = append(m.calls, permCall{requestID, d})
	return m.respondErr
}

func (m *mockPermBackend) IsRunning() bool { return true }

// noResponderBackend raises no permission prompts: it has no responder.
type noResponderBackend struct{ delegator.Delegator }

func (noResponderBackend) IsRunning() bool { return true }

// setupAgentWithMockBackend wires a real DelegatedManager with be pre-seeded
// under "test/s", so Get() returns it without Start().
func setupAgentWithMockBackend(t *testing.T, be delegator.Delegator) *Agent {
	t.Helper()
	dm := &DelegatedManager{
		backends: make(map[string]*managedBackend),
		NewBackend: func() (delegator.Delegator, error) {
			return be, nil
		},
		StartOpts: delegator.StartOptions{},
	}
	dm.backends["test/s"] = &managedBackend{be: be}
	return &Agent{DelegatedManager: dm}
}

// TestSendPermissionResponse_NilManager verifies that a nil DelegatedManager
// returns nil immediately (no panic, no error).
func TestSendPermissionResponse_NilManager(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	if err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestSendPermissionResponse_ChoiceToDecision pins how each backend's button
// data becomes the one PermissionDecision every backend answers. The mapping
// is the union of the three shapes it replaced: ccstream/codex (allow, deny,
// "allow_always:<prefix>" rule) and opencode (allow, deny, "always" remember).
func TestSendPermissionResponse_ChoiceToDecision(t *testing.T) {
	t.Parallel()
	deny := delegator.PermissionDecision{Message: "User denied permission"}
	for _, tc := range []struct {
		choice string
		want   delegator.PermissionDecision
	}{
		{"allow", delegator.PermissionDecision{Allow: true}},
		{"deny", deny},
		{"reject", deny},
		{"allow_always:Bash:git *", delegator.PermissionDecision{Allow: true, Remember: true, RulePrefix: "Bash:git *"}},
		{"allow_always", delegator.PermissionDecision{Allow: true, Remember: true}},
		{"always", delegator.PermissionDecision{Allow: true, Remember: true}},
	} {
		t.Run(tc.choice, func(t *testing.T) {
			be := &mockPermBackend{}
			a := setupAgentWithMockBackend(t, be)
			if err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", tc.choice); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(be.calls) != 1 {
				t.Fatalf("expected 1 respond call, got %d", len(be.calls))
			}
			if c := be.calls[0]; c.requestID != "req-1" || c.d != tc.want {
				t.Errorf("got %q %+v, want req-1 %+v", c.requestID, c.d, tc.want)
			}
		})
	}
}

func TestSendPermissionResponse_GetError(t *testing.T) {
	t.Parallel()
	dm := &DelegatedManager{
		backends: make(map[string]*managedBackend),
		NewBackend: func() (delegator.Delegator, error) {
			return nil, errors.New("backend unavailable")
		},
		StartOpts: delegator.StartOptions{},
	}
	a := &Agent{DelegatedManager: dm}
	if err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow"); err == nil {
		t.Fatal("expected error from Get, got nil")
	}
}

// TestSendPermissionResponse_EmptyRequestID verifies that a response with no
// requestID reaches no responder and is reported as an error, not dropped.
func TestSendPermissionResponse_EmptyRequestID(t *testing.T) {
	t.Parallel()
	be := &mockPermBackend{}
	a := setupAgentWithMockBackend(t, be)
	if err := a.SendPermissionResponse(context.Background(), "test/s", "", "y"); err == nil {
		t.Fatal("expected an error for an unanswerable prompt, got nil")
	}
	if len(be.calls) != 0 {
		t.Errorf("expected 0 respond calls with empty requestID, got %d", len(be.calls))
	}
}

// TestSendPermissionResponse_NoResponder: a backend without a
// PermissionResponder can't answer, which is an error rather than a drop.
func TestSendPermissionResponse_NoResponder(t *testing.T) {
	t.Parallel()
	a := setupAgentWithMockBackend(t, noResponderBackend{})
	if err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow"); err == nil {
		t.Fatal("expected an error from a backend with no responder, got nil")
	}
}

// TestSendPermissionResponse_RespondError verifies that the backend's error
// is propagated.
func TestSendPermissionResponse_RespondError(t *testing.T) {
	t.Parallel()
	be := &mockPermBackend{respondErr: errors.New("protocol error")}
	a := setupAgentWithMockBackend(t, be)
	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow_always:Bash:*")
	if err == nil || err.Error() != "protocol error" {
		t.Fatalf("error = %v, want protocol error", err)
	}
}

// questionPermBackend refuses a permission answer for an ID it holds as a
// question, as ccstream does not and other backends may (#2608).
type questionPermBackend struct {
	mockPermBackend
	cancelled []string
}

func (q *questionPermBackend) RespondToQuestion(string, string) error { return nil }
func (q *questionPermBackend) CancelQuestion(requestID string) error {
	q.cancelled = append(q.cancelled, requestID)
	return nil
}
func (q *questionPermBackend) HasPendingQuestion() string { return "" }

// TestFailUndeliverablePrompt (#2608): a prompt no chat could show is refused
// to the backend with the reason as its message; a backend that holds the ID
// as a question has it cancelled instead; one that can answer nothing is an
// error, never a silent success.
func TestFailUndeliverablePrompt(t *testing.T) {
	t.Parallel()
	t.Run("permission denied with reason", func(t *testing.T) {
		be := &mockPermBackend{}
		a := setupAgentWithMockBackend(t, be)
		if err := a.FailUndeliverablePrompt(context.Background(), "test/s", "req-1", "no chat"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := delegator.PermissionDecision{Message: "no chat"}
		if len(be.calls) != 1 || be.calls[0].requestID != "req-1" || be.calls[0].d != want {
			t.Fatalf("calls = %+v, want one deny of req-1 with the reason", be.calls)
		}
	})
	t.Run("question cancelled", func(t *testing.T) {
		be := &questionPermBackend{mockPermBackend: mockPermBackend{respondErr: errors.New("not a permission")}}
		a := setupAgentWithMockBackend(t, be)
		if err := a.FailUndeliverablePrompt(context.Background(), "test/s", "req-q", "no chat"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(be.cancelled) != 1 || be.cancelled[0] != "req-q" {
			t.Fatalf("cancelled = %v, want [req-q]", be.cancelled)
		}
	})
	t.Run("no responder is an error", func(t *testing.T) {
		a := setupAgentWithMockBackend(t, noResponderBackend{})
		if err := a.FailUndeliverablePrompt(context.Background(), "test/s", "req-1", "no chat"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}

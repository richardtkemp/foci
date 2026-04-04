package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"foci/internal/delegator"
)

// mockPermBackend implements delegator.Delegator with configurable permission
// response recording. It also implements the permResponder and ruleResponder
// interfaces used by SendPermissionResponse.
type mockPermBackend struct {
	delegator.Delegator // embed to satisfy the interface; unused methods will panic

	respondCalls    []respondCall
	ruleCalls       []ruleCall
	keystrokeCalls  []string
	respondErr      error
	ruleErr         error
	keystrokeErr    error
	supportsRule    bool
}

type respondCall struct {
	requestID string
	allow     bool
	message   string
}

type ruleCall struct {
	requestID string
	prefix    string
}

func (m *mockPermBackend) RespondToPermission(requestID string, allow bool, message string) error {
	m.respondCalls = append(m.respondCalls, respondCall{requestID, allow, message})
	return m.respondErr
}

func (m *mockPermBackend) RespondToPermissionWithRule(requestID string, prefix string) error {
	m.ruleCalls = append(m.ruleCalls, ruleCall{requestID, prefix})
	return m.ruleErr
}

func (m *mockPermBackend) SendKeystroke(_ context.Context, key string) error {
	m.keystrokeCalls = append(m.keystrokeCalls, key)
	return m.keystrokeErr
}

func (m *mockPermBackend) IsRunning() bool { return true }

// mockPermBackendNoRule only implements permResponder, not ruleResponder.
// Used to test the fallback when RespondToPermissionWithRule is not available.
type mockPermBackendNoRule struct {
	delegator.Delegator
	respondCalls []respondCall
}

func (m *mockPermBackendNoRule) RespondToPermission(requestID string, allow bool, message string) error {
	m.respondCalls = append(m.respondCalls, respondCall{requestID, allow, message})
	return nil
}

func (m *mockPermBackendNoRule) SendKeystroke(_ context.Context, key string) error {
	return nil
}

func (m *mockPermBackendNoRule) IsRunning() bool { return true }

// mockDelegatedManagerForPerm is a minimal DelegatedManager replacement
// that returns a pre-configured backend. Since DelegatedManager.Get requires
// real mutex/map wiring, we set up a real DelegatedManager with a pre-seeded backend.
func setupAgentWithMockBackend(t *testing.T, be delegator.Delegator) *Agent {
	t.Helper()
	dm := &DelegatedManager{
		backends: make(map[string]*managedBackend),
		NewBackend: func() (delegator.Delegator, error) {
			return be, nil
		},
		StartOpts: delegator.StartOptions{},
	}
	// Pre-seed the backend so Get() finds it without calling Start().
	dm.backends["test/s"] = &managedBackend{
		be: be,
	}

	return &Agent{DelegatedManager: dm}
}

// TestSendPermissionResponse_NilManager verifies that a nil DelegatedManager
// returns nil immediately (no panic, no error).
func TestSendPermissionResponse_NilManager(t *testing.T) {
	a := &Agent{} // DelegatedManager is nil

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestSendPermissionResponse_AllowRoutes verifies that choice="allow" sends
// RespondToPermission with allow=true and no denial message.
func TestSendPermissionResponse_AllowRoutes(t *testing.T) {
	be := &mockPermBackend{supportsRule: true}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(be.respondCalls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(be.respondCalls))
	}
	call := be.respondCalls[0]
	if call.requestID != "req-1" {
		t.Errorf("requestID = %q, want %q", call.requestID, "req-1")
	}
	if !call.allow {
		t.Error("expected allow=true")
	}
	if call.message != "" {
		t.Errorf("expected empty message, got %q", call.message)
	}
}

// TestSendPermissionResponse_DenyRoutes verifies that choice="deny" sends
// RespondToPermission with allow=false and a denial message.
func TestSendPermissionResponse_DenyRoutes(t *testing.T) {
	be := &mockPermBackend{supportsRule: true}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "deny")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(be.respondCalls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(be.respondCalls))
	}
	call := be.respondCalls[0]
	if call.allow {
		t.Error("expected allow=false for deny choice")
	}
	if call.message != "User denied permission" {
		t.Errorf("message = %q, want %q", call.message, "User denied permission")
	}
}

// TestSendPermissionResponse_AllowAlwaysWithRule verifies that an
// "allow_always:<prefix>" choice routes to RespondToPermissionWithRule
// when the backend supports it.
func TestSendPermissionResponse_AllowAlwaysWithRule(t *testing.T) {
	be := &mockPermBackend{supportsRule: true}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow_always:Bash:git *")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(be.ruleCalls) != 1 {
		t.Fatalf("expected 1 rule call, got %d", len(be.ruleCalls))
	}
	rc := be.ruleCalls[0]
	if rc.requestID != "req-1" {
		t.Errorf("requestID = %q, want %q", rc.requestID, "req-1")
	}
	if rc.prefix != "Bash:git *" {
		t.Errorf("prefix = %q, want %q", rc.prefix, "Bash:git *")
	}

	// Should NOT have fallen through to RespondToPermission.
	if len(be.respondCalls) != 0 {
		t.Errorf("expected 0 respond calls when rule is supported, got %d", len(be.respondCalls))
	}
}

// TestSendPermissionResponse_AllowAlwaysFallback verifies that when the backend
// does not implement ruleResponder, allow_always falls through to a simple
// allow via RespondToPermission.
func TestSendPermissionResponse_AllowAlwaysFallback(t *testing.T) {
	be := &mockPermBackendNoRule{}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow_always:Read")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have fallen through to RespondToPermission with allow=true.
	if len(be.respondCalls) != 1 {
		t.Fatalf("expected 1 respond call (fallback), got %d", len(be.respondCalls))
	}
	if !be.respondCalls[0].allow {
		t.Error("expected allow=true on fallback")
	}
}

// TestSendPermissionResponse_GetError verifies that an error from
// DelegatedManager.Get is propagated back to the caller.
func TestSendPermissionResponse_GetError(t *testing.T) {
	dm := &DelegatedManager{
		backends: make(map[string]*managedBackend),
		NewBackend: func() (delegator.Delegator, error) {
			return nil, errors.New("backend unavailable")
		},
		StartOpts: delegator.StartOptions{},
	}
	a := &Agent{DelegatedManager: dm}

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow")
	if err == nil {
		t.Fatal("expected error from Get, got nil")
	}
	if !errors.Is(err, fmt.Errorf("backend unavailable")) && err.Error() != "backend unavailable" {
		// Just check it's non-nil and related to the backend — the exact wrapping may vary.
		t.Logf("got expected error: %v", err)
	}
}

// TestSendPermissionResponse_EmptyRequestID verifies that when requestID is
// empty (tmux backend), the response falls back to SendKeystroke.
func TestSendPermissionResponse_EmptyRequestID(t *testing.T) {
	be := &mockPermBackend{supportsRule: true}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "", "y")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT have called RespondToPermission (requestID is empty).
	if len(be.respondCalls) != 0 {
		t.Errorf("expected 0 respond calls with empty requestID, got %d", len(be.respondCalls))
	}

	// Should have fallen back to keystroke.
	if len(be.keystrokeCalls) != 1 {
		t.Fatalf("expected 1 keystroke call, got %d", len(be.keystrokeCalls))
	}
	if be.keystrokeCalls[0] != "y" {
		t.Errorf("keystroke = %q, want %q", be.keystrokeCalls[0], "y")
	}
}

// TestSendPermissionResponse_RespondError verifies that errors from
// RespondToPermission are propagated.
func TestSendPermissionResponse_RespondError(t *testing.T) {
	be := &mockPermBackend{respondErr: errors.New("protocol error")}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "protocol error" {
		t.Errorf("error = %q, want %q", err.Error(), "protocol error")
	}
}

// TestSendPermissionResponse_RuleError verifies that errors from
// RespondToPermissionWithRule are propagated.
func TestSendPermissionResponse_RuleError(t *testing.T) {
	be := &mockPermBackend{supportsRule: true, ruleErr: errors.New("rule error")}
	a := setupAgentWithMockBackend(t, be)

	err := a.SendPermissionResponse(context.Background(), "test/s", "req-1", "allow_always:Bash:*")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "rule error" {
		t.Errorf("error = %q, want %q", err.Error(), "rule error")
	}
}

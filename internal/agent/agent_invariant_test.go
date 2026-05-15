package agent

import (
	"context"
	"strings"
	"testing"
)

// TestValidateSessionOwnership_CrossAgent verifies that the invariant guard
// rejects a session key owned by a different agent. This catches cross-agent
// routing bugs where a foreign session ends up in the wrong workdir / backend
// / permission scope.
//
// Trigger: yesterday's incident where fotini's send_to_session targeted a
// clutch session via the reply_to=caller path; fotini's Agent processed
// clutch's session in fotini's workdir, leaving an unrecoverable cc_resume_id.
func TestValidateSessionOwnership_CrossAgent(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "fotini"}
	err := a.validateSessionOwnership("clutch/c5970082313/1775812617")
	if err == nil {
		t.Fatal("validateSessionOwnership with cross-agent key returned nil, want invariant violation")
	}
	if !strings.Contains(err.Error(), "invariant violation") {
		t.Errorf("error = %q, want it to mention invariant violation", err.Error())
	}
	if !strings.Contains(err.Error(), "fotini") || !strings.Contains(err.Error(), "clutch") {
		t.Errorf("error = %q, should name both the receiving agent and the owning agent", err.Error())
	}
}

// TestValidateSessionOwnership_SameAgent verifies that an Agent processing
// its own session passes the guard cleanly. This is the common happy path.
func TestValidateSessionOwnership_SameAgent(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "clutch"}
	if err := a.validateSessionOwnership("clutch/c5970082313/1775812617"); err != nil {
		t.Errorf("validateSessionOwnership(own-session) returned error: %v", err)
	}
}

// TestValidateSessionOwnership_BranchKey verifies the guard parses branch-style
// keys correctly (4 segments instead of 3). The AgentID still comes from the
// first segment.
func TestValidateSessionOwnership_BranchKey(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "fotini"}
	// Own branch: pass
	if err := a.validateSessionOwnership("fotini/c8792716180/1741826250/b1741826300"); err != nil {
		t.Errorf("own branch key returned error: %v", err)
	}
	// Cross-agent branch: fail
	if err := a.validateSessionOwnership("clutch/c5970082313/1775812617/b1776233668"); err == nil {
		t.Fatal("cross-agent branch key returned nil, want invariant violation")
	}
}

// TestValidateSessionOwnership_LegacyKeyExempt verifies that unparseable
// legacy session keys (test-only formats like "test/s") are exempt from
// the guard. Production code uses structured keys, but lots of pre-existing
// tests use these short formats.
func TestValidateSessionOwnership_LegacyKeyExempt(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "fotini"}
	for _, key := range []string{"test/s", "sess/A", "agent:clutch:main"} {
		if err := a.validateSessionOwnership(key); err != nil {
			t.Errorf("legacy key %q returned error: %v", key, err)
		}
	}
}

// TestValidateSessionOwnership_EmptyAgentIDExempt verifies that an Agent
// without an AgentID set (test mode) bypasses the guard entirely. Without
// this exemption, most existing agent tests that don't bother setting
// AgentID would break.
func TestValidateSessionOwnership_EmptyAgentIDExempt(t *testing.T) {
	t.Parallel()
	a := &Agent{} // no AgentID
	if err := a.validateSessionOwnership("clutch/c5970082313/1775812617"); err != nil {
		t.Errorf("empty AgentID returned error: %v", err)
	}
}

// TestHandleMessage_RejectsCrossAgentSessionKey verifies that the invariant
// guard is wired into HandleMessage itself — not just the helper. This is the
// path the notifier and inbox both go through; a bypass here would defeat the
// guard's purpose. The cross-agent error case returns before any state load,
// so this test can use a bare Agent without panicking on nil dependencies.
func TestHandleMessage_RejectsCrossAgentSessionKey(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "fotini"}
	err := a.HandleMessage(context.Background(), "clutch/c5970082313/1775812617", []string{"ping"}, nil)
	if err == nil {
		t.Fatal("HandleMessage with cross-agent key returned nil, want invariant violation")
	}
	if !strings.Contains(err.Error(), "invariant violation") {
		t.Errorf("error = %q, want it to mention invariant violation", err.Error())
	}
}

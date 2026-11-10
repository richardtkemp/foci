package main

import (
	"testing"
)

// TestSendSystemNotice_SystemRoleToOwnSession (#3005): the delegated
// manager's out-of-band notices ("Transcript ... not found ... Starting
// fresh", "Busy right now ... queued") are foci's words, not the agent's. They
// go through the injected (system-role) send, addressed to the session they
// concern, never the agent-role SendText/SendToSession.
func TestSendSystemNotice_SystemRoleToOwnSession(t *testing.T) {
	t.Parallel()
	conn := newRecordingConn("clutch/c42")
	cm := recordingConnMgr{sessionKey: "clutch/c42", conn: conn}

	sendSystemNotice(cm, "clutch", "clutch/c42", "⏳ Busy right now.")

	if _, agentRole := conn.snapshot(); len(agentRole) != 0 {
		t.Errorf("notice sent with the agent role: %v", agentRole)
	}
	got := conn.injectedSnapshot()
	if len(got) != 1 || got[0] != "clutch/c42|⏳ Busy right now." {
		t.Errorf("injected sends = %q, want one system notice to clutch/c42", got)
	}
}

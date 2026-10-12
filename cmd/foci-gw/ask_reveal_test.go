package main

import (
	"testing"
)

// TestNewAskRevealFn_PostsToSessionChat proves the quiz-reveal wiring (#1316)
// actually leaves the gateway: newAskRevealFn's hook must route through
// route.NotifySessionChat — to the asking session's own chat (the
// SessionNotifier path), under the wiring agent's ID — so a quiz reveal lands
// with the user who answered, as the plain text the notification path
// delivers. Without the wiring the hook is a no-op and a chat user who answers
// a quiz question never sees the answer+explanation. Mutation arm: point the
// hook at a broadcast (plain SendNotification) or another agent's ID and the
// toSession/agentIDs assertions go red.
func TestNewAskRevealFn_PostsToSessionChat(t *testing.T) {
	conn := &sessionNotifyConn{stubConn: &stubConn{sessionKey: "x/c1"}}
	connMgr := &sessionRoutingConnMgr{conn: conn}

	newAskRevealFn("x", connMgr)("x/c1", "Capital?\nAnswer: 2. Oslo")

	if len(conn.broadcasts) != 0 {
		t.Errorf("reveal must not broadcast, got %v", conn.broadcasts)
	}
	if len(conn.toSession) != 1 || conn.toSession[0] != "x/c1|Capital?\nAnswer: 2. Oslo" {
		t.Errorf("toSession = %v, want the reveal verbatim in the session's chat", conn.toSession)
	}
	if len(connMgr.agentIDs) != 1 || connMgr.agentIDs[0] != "x" {
		t.Errorf("agentIDs = %v, want [x] (the wiring agent)", connMgr.agentIDs)
	}
}

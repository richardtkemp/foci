package app

import "testing"

// TestDurableConnFor_NoHub verifies DurableConnFor returns a genuine nil
// interface when the app provider isn't running at all (activeHub unset).
func TestDurableConnFor_NoHub(t *testing.T) {
	setActiveHub(nil)
	if conn := DurableConnFor("ag/c1"); conn != nil {
		t.Fatalf("DurableConnFor with no active hub = %v, want nil", conn)
	}
}

// TestDurableConnFor_UnregisteredAgent verifies DurableConnFor returns a
// genuine nil interface — not a typed-nil *appConn silently wrapped as a
// non-nil platform.Connection — when the hub is active but the session's
// agent has no app registration at all (h.PrimaryBot(agentID) == nil inside
// hub.BotForSession). Without the explicit nil check in DurableConnFor, every
// caller's `conn == nil` guard downstream would silently break: a Go
// interface holding a nil concrete pointer still compares != nil.
func TestDurableConnFor_UnregisteredAgent(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	if conn := DurableConnFor("unregistered/c1"); conn != nil {
		t.Fatalf("DurableConnFor for an agent with no app registration = %v, want nil", conn)
	}
}

// TestDurableConnFor_RegisteredAgent_NoLiveClient is the clutch #1350
// follow-up contract: a session whose agent IS registered with the app
// provider gets a usable connection back even with zero live clients
// connected and no existing conversation binding for this exact session —
// autonomousTurnSink's DurableTurnSink fallback (internal/agent/in_flight.go)
// depends on this returning non-nil so an adopted turn's output doesn't fall
// through to the total-discard NopSink. Delivery through the returned
// connection still degrades gracefully (SendToSession/convBinding.send) if
// there's truly no binding to persist against at all.
func TestDurableConnFor_RegisteredAgent_NoLiveClient(t *testing.T) {
	h := newTestHub()
	h.agents["ag"] = &appConn{hub: h, agentID: "ag"}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	conn := DurableConnFor("ag/c1")
	if conn == nil {
		t.Fatal("DurableConnFor for a registered agent = nil, want a usable connection (clutch #1350 follow-up)")
	}
}

package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
)

// sessionNotifyConn records how a notification left the gateway: through the
// session's own chat (SendNotificationToSession — the SessionNotifier path of
// route.NotifySessionChat) or as a broadcast (SendNotification). The tests below
// drive the hooks synchronously, so plain slices need no locking.
type sessionNotifyConn struct {
	*stubConn
	toSession  []string // "sessionKey|text" per SendNotificationToSession call
	broadcasts []string // texts sent via SendNotification
}

func (c *sessionNotifyConn) SendNotificationToSession(sessionKey, text string) string {
	c.toSession = append(c.toSession, sessionKey+"|"+text)
	return ""
}

func (c *sessionNotifyConn) EditNotificationInSession(sessionKey, msgID, text string) error {
	return nil
}

func (c *sessionNotifyConn) SendNotification(text string) {
	c.broadcasts = append(c.broadcasts, text)
}

// sessionRoutingConnMgr resolves ForSessionOrPrimary to one fixed connection and
// records the agent ID it was asked for — stubConnMgr returns nil there, so
// observing session-scoped delivery needs this override.
type sessionRoutingConnMgr struct {
	stubConnMgr
	conn     platform.Connection
	agentIDs []string
}

func (m *sessionRoutingConnMgr) ForSessionOrPrimary(_, agentID string) platform.Connection {
	m.agentIDs = append(m.agentIDs, agentID)
	return m.conn
}

// TestWireAgentPlatformCallbacks_TurnQueuedNotifyDeliversToSessionChat proves the
// #2281 queued-turn notice actually leaves the gateway: wireAgentPlatformCallbacks
// must set Agent.TurnQueuedNotifyFunc and route it through route.NotifySessionChat —
// to the waiting session's own chat (SessionNotifier), under the wiring agent's ID —
// not as a broadcast. Without the wiring the hook stays nil and a user turn that
// waits for a turn-limit slot is never announced.
func TestWireAgentPlatformCallbacks_TurnQueuedNotifyDeliversToSessionChat(t *testing.T) {
	conn := &sessionNotifyConn{stubConn: &stubConn{sessionKey: "x/c1"}}
	connMgr := &sessionRoutingConnMgr{conn: conn}
	acfg := config.AgentConfig{ID: "x"}
	live := config.NewLiveValue(config.Resolve(&config.Config{}, acfg))

	ag := &agent.Agent{}
	wireAgentPlatformCallbacks(ag, acfg, live, connMgr, nil)

	if ag.TurnQueuedNotifyFunc == nil {
		t.Fatal("TurnQueuedNotifyFunc is nil after wiring — a user turn that waits for a turn-limit slot sends no queued notice (#2281)")
	}
	// The inbox fires this hook in its own goroutine so a slow platform cannot
	// stall a worker; calling it here is synchronous, so the recording above is
	// complete when the call returns.
	ag.TurnQueuedNotifyFunc("x/c1", "queued text")

	if len(conn.toSession) != 1 || conn.toSession[0] != "x/c1|queued text" {
		t.Errorf("SendNotificationToSession calls = %v, want exactly [x/c1|queued text] — the notice must reach the waiting session's chat", conn.toSession)
	}
	if len(conn.broadcasts) != 0 {
		t.Errorf("SendNotification calls = %v, want none — the queued notice is session-scoped, not a broadcast", conn.broadcasts)
	}
	if len(connMgr.agentIDs) != 1 || connMgr.agentIDs[0] != "x" {
		t.Errorf("ForSessionOrPrimary agent IDs = %v, want [x] — the notice must be routed under the wiring agent's ID", connMgr.agentIDs)
	}
}

// TestConfigureUniversalWiresTurnLimiters proves the #2281 turn limits leave the
// resolved config and reach the agent: configureUniversal must build the per-agent
// limiter from the resolved [behavior] cascade and install the ONE shared
// gateway-wide limiter (the same pointer for every agent, built once in main). A
// nil field would silently disable its cap, because the inbox takes no slot for a
// nil limiter.
func TestConfigureUniversalWiresTurnLimiters(t *testing.T) {
	t.Parallel()

	p := minimalSetupParams(t, "test")
	p.resolved.Behavior.MaxConcurrentTurns = 3
	global := agent.NewTurnLimiter(5)
	p.globalTurnLimit = global

	ag := &agent.Agent{}
	configureUniversal(ag, p, nil)

	if ag.TurnLimit == nil || ag.TurnLimit.Size() != 3 {
		t.Errorf("TurnLimit = %v, want a limiter of size 3 built from resolved Behavior.MaxConcurrentTurns", ag.TurnLimit)
	}
	if ag.GlobalTurnLimit != global {
		t.Errorf("GlobalTurnLimit = %v, want the shared limiter passed via setupParams (one pointer for every agent)", ag.GlobalTurnLimit)
	}
}

// TestConfigureUniversalTurnLimitersNilWhenUnlimited pins the unset default: a
// resolved max_concurrent_turns of 0 and no global limiter leave both agent fields
// nil — which is what keeps the inbox's no-slot fast path (no wait, no notice, no
// log) active when the caps are not configured.
func TestConfigureUniversalTurnLimitersNilWhenUnlimited(t *testing.T) {
	t.Parallel()

	p := minimalSetupParams(t, "test") // no MaxConcurrentTurns, no globalTurnLimit

	ag := &agent.Agent{}
	configureUniversal(ag, p, nil)

	if ag.TurnLimit != nil {
		t.Errorf("TurnLimit = %v, want nil when max_concurrent_turns is 0 (unlimited)", ag.TurnLimit)
	}
	if ag.GlobalTurnLimit != nil {
		t.Errorf("GlobalTurnLimit = %v, want nil when no gateway-wide cap is configured", ag.GlobalTurnLimit)
	}
}

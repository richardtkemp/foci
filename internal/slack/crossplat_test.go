package slack

// crossplat_test.go: requirement 5's cross-platform clause (#2459 A1) — with
// a second platform registered beside Slack, every send for a Slack session
// reaches Slack only: never the other platform's connection, and never a
// guessed "default" conversation. Runs the REAL aggregating connection
// manager (platform.InitMessaging) over the real Slack provider (against the
// fake Slack) and a second, recording platform, so the ownership rows the
// transport writes are proven to steer the aggregator end to end.

import (
	"context"
	"sync"
	"testing"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/route"
)

// crossplatName is the second platform's registry name in this file's tests.
const crossplatName = "faketg"

// crossplatConn is the other platform's connection: every session-blind and
// session send is recorded, everything else is inert (the embedded nil
// interface keeps the stub minimal — the shape route's tests use).
type crossplatConn struct {
	platform.Connection
	mu    sync.Mutex
	sends []string
}

func (c *crossplatConn) record(what string) {
	c.mu.Lock()
	c.sends = append(c.sends, what)
	c.mu.Unlock()
}

func (c *crossplatConn) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sends...)
}

func (c *crossplatConn) PlatformName() string               { return crossplatName }
func (c *crossplatConn) SessionKey() string                 { return "" }
func (c *crossplatConn) SendNotification(text string)       { c.record("notify:" + text) }
func (c *crossplatConn) SendToSession(_, text string) error { c.record("session:" + text); return nil }
func (c *crossplatConn) SendText(text string) error         { c.record("text:" + text); return nil }

// crossplatMgr is the other platform's connection manager: one recorded
// connection, live for every agent that had SetupAgentConnection called.
type crossplatMgr struct {
	conn   *crossplatConn
	agents map[string]bool
}

func (m *crossplatMgr) Primary(agentID string) platform.Connection {
	if m.agents[agentID] {
		return m.conn
	}
	return nil
}

func (m *crossplatMgr) AllForAgent(agentID string) []platform.Connection {
	if m.agents[agentID] {
		return []platform.Connection{m.conn}
	}
	return nil
}

func (m *crossplatMgr) ForSession(string) platform.Connection { return nil }
func (m *crossplatMgr) ForSessionOrPrimary(_, agentID string) platform.Connection {
	return m.Primary(agentID)
}
func (m *crossplatMgr) AcquireFacet(string) (platform.Connection, bool) { return nil, false }
func (m *crossplatMgr) HasFacet(string) bool                            { return false }
func (m *crossplatMgr) StartAll(context.Context)                        {}
func (m *crossplatMgr) Wait()                                           {}

// crossplatProvider adapts crossplatMgr to platform.MessagingProvider.
type crossplatProvider struct{ mgr *crossplatMgr }

func (p *crossplatProvider) Name() string                               { return crossplatName }
func (p *crossplatProvider) IsConfigured(*config.Config) (bool, string) { return true, "" }
func (p *crossplatProvider) Init(platform.ProviderDeps) error           { return nil }
func (p *crossplatProvider) ConnectionManager() platform.ConnectionManager {
	return p.mgr
}
func (p *crossplatProvider) SetupAgentConnection(params platform.AgentConnectionParams) *platform.SetupResult {
	p.mgr.agents[params.AgentID] = true
	return &platform.SetupResult{}
}
func (p *crossplatProvider) SetupSharedFacet(platform.SharedFacetParams)                  {}
func (p *crossplatProvider) RestoreFacetSessions(platform.RestoreParams)                  {}
func (p *crossplatProvider) SetLifecycleCallback(string, platform.LifecycleEvent, func()) {}
func (p *crossplatProvider) ToolDetailStore() platform.ToolDetailStore                    { return nil }
func (p *crossplatProvider) AgentPreFlight(string) []string                               { return nil }
func (p *crossplatProvider) DefaultPlatformConfig() config.PlatformConfig {
	return config.PlatformConfig{ID: crossplatName}
}
func (p *crossplatProvider) ValidateConfig(config.PlatformConfig) []string { return nil }
func (p *crossplatProvider) Close() error                                  { return nil }

// TestCrossPlatform_SlackSessionSendsNeverReachAnotherPlatform proves the
// unconfirmed clause of requirement 5 with a run: with a second platform
// registered beside Slack (the aggregator built by platform.InitMessaging),
// a Slack session's sends — ForSession and ForSessionOrPrimary alike — land
// only in the session's own Slack conversation, and agent-wide traffic
// (Primary, Broadcast) reaches the other platform without ever posting into
// a Slack conversation (#2459 req 5, 6).
func TestCrossPlatform_SlackSessionSendsNeverReachAnotherPlatform(t *testing.T) {
	f := newFakeSlack(t)
	deps := providerDeps(t, f, "xoxb-ok", "xapp-ok")
	// The interim access rule (req 7): with no allow list nobody may talk,
	// so the test's user must be listed on the platform entry.
	deps.Config.Platforms[0].Access.AllowedUsers = []string{"U1"}
	tg := &crossplatProvider{mgr: &crossplatMgr{conn: &crossplatConn{}, agents: map[string]bool{}}}
	platform.RegisterMessagingProvider(crossplatName, tg)

	m, err := platform.InitMessaging(deps.Config, deps)
	if err != nil {
		t.Fatalf("InitMessaging: %v", err)
	}

	// Wire the agent on BOTH platforms, as the gateway does at startup.
	ag := &setupHandler{}
	acfg := config.AgentConfig{ID: "helen"}
	if results := m.SetupAgentConnection(platform.AgentConnectionParams{
		AgentID:     "helen",
		Handler:     ag,
		AgentConfig: acfg,
		Resolved:    config.Resolve(deps.Config, acfg),
	}); len(results) != 2 {
		t.Fatalf("SetupAgentConnection results = %d, want 2 (slack + the other platform)", len(results))
	}

	// A Slack DM through the real provider's manager: it registers the
	// conversation and enqueues the turn.
	sp, ok := platform.GetProvider(platformName).(*slackProvider)
	if !ok || sp.mgr == nil {
		t.Fatal("slack provider not initialised by InitMessaging")
	}
	sp.mgr.botUserID.Store("UBOTID")
	sp.mgr.handleEnvelope(envelopeFor(dmEvent("U1", "D100", "hello", "1")))
	envs := ag.drained()
	if len(envs) != 1 {
		t.Fatalf("dm started %d turns, want 1", len(envs))
	}
	sessionKey := envs[0].SessionKey

	// The registration row is what steers the aggregator: Slack owns the chat.
	if got := deps.SessionIndex.PlatformForChat("helen", envs[0].ChatID); got != platformName {
		t.Fatalf("PlatformForChat = %q, want %q (the Slack registration row)", got, platformName)
	}

	cm := m.ConnectionManager()

	// Session sends through the aggregator reach Slack only: one more post
	// in the session's own DM per send, nothing on the other platform.
	sends := []struct {
		name string
		get  func() platform.Connection
		send func(c platform.Connection)
	}{
		{"ForSession", func() platform.Connection { return cm.ForSession(sessionKey) },
			func(c platform.Connection) { c.SendNotification("session notice") }},
		{"ForSessionOrPrimary", func() platform.Connection { return cm.ForSessionOrPrimary(sessionKey, "helen") },
			func(c platform.Connection) {
				if err := c.SendToSession(sessionKey, "agent reply"); err != nil {
					t.Fatalf("SendToSession: %v", err)
				}
			}},
	}
	for i, s := range sends {
		conn := s.get()
		if conn == nil {
			t.Fatalf("%s resolved no connection", s.name)
		}
		if conn.PlatformName() != platformName {
			t.Errorf("%s resolved %s, want the Slack connection", s.name, conn.PlatformName())
		}
		s.send(conn)
		if got := len(f.postsTo("D100")); got != i+1 {
			t.Errorf("%s: posts to the session's DM = %d, want %d (one per session send)", s.name, got, i+1)
		}
		if sends := tg.mgr.conn.recorded(); len(sends) != 0 {
			t.Errorf("%s: the other platform recorded %v, want nothing", s.name, sends)
		}
	}

	// Agent-wide traffic: the other platform is the primary and the only
	// broadcast target; Slack's unbound connection is never addressed.
	mark := f.totalCalls()
	primary := cm.Primary("helen")
	if primary == nil || primary.PlatformName() != crossplatName {
		t.Fatalf("Primary = %v, want the other platform (Slack is never a default)", primary)
	}
	primary.SendNotification("startup notice")
	for _, c := range route.Broadcast(cm, "helen") {
		c.SendNotification("broadcast")
	}
	if got := f.totalCalls(); got != mark {
		t.Errorf("agent-wide traffic made %d Slack API calls, want 0 (no guessed Slack conversation)", got-mark)
	}
	if sends := tg.mgr.conn.recorded(); len(sends) != 2 {
		t.Errorf("other platform sends = %v, want the primary notice and the broadcast", sends)
	}
}

package main

import (
	"context"
	"testing"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
)

// rlConn records the notifications delivered to it. confines models
// platform.ReplyConfiner (Slack's bound views): wired per test so the tests
// prove the delivery decision reads the capability, not the platform name.
// drops models platform.AgentWideDropper (Slack's unbound conn), whose
// SendNotification can address nothing.
type rlConn struct {
	*stubConn
	name     string
	confines bool
	drops    bool
	notices  []string
}

func (c *rlConn) SendNotification(s string) { c.notices = append(c.notices, s) }
func (c *rlConn) ConfinesReplies() bool     { return c.confines }
func (c *rlConn) DropsAgentWideSends() bool { return c.drops }
func (c *rlConn) PlatformName() string      { return c.name }

func newRLConn(name, sessionKey string) *rlConn {
	return &rlConn{stubConn: &stubConn{sessionKey: sessionKey}, name: name}
}

// rlConnMgr is a ConnectionManager whose session connection is a DIFFERENT
// object from the agent's primary — the only shape that can tell "delivered to
// the triggering chat" from "delivered to the default chat" apart.
// confinedKey models the aggregating manager's SessionConfinementResolver
// answer: the session's chat is owned by a platform whose provider declares
// session-reply confinement, so confinement holds with no live connection.
type rlConnMgr struct {
	primary     *rlConn
	sessionKey  string
	session     *rlConn
	confinedKey string
}

// SessionPlatformConfinesReplies is the owning-platform confinement answer
// (see the confinedKey field); empty answers false, so the ordinary-shape
// tests below are unaffected.
func (m *rlConnMgr) SessionPlatformConfinesReplies(sessionKey string) bool {
	return m.confinedKey != "" && sessionKey == m.confinedKey
}

func (m *rlConnMgr) Primary(string) platform.Connection {
	if m.primary == nil {
		return nil
	}
	return m.primary
}
func (m *rlConnMgr) AllForAgent(string) []platform.Connection { return nil }
func (m *rlConnMgr) ForSession(key string) platform.Connection {
	if m.session == nil || key == "" || key != m.sessionKey {
		return nil
	}
	return m.session
}
func (m *rlConnMgr) ForSessionOrPrimary(key, agentID string) platform.Connection {
	if c := m.ForSession(key); c != nil {
		return c
	}
	return m.Primary(agentID)
}
func (m *rlConnMgr) AcquireFacet(string) (platform.Connection, bool) { return nil, false }
func (m *rlConnMgr) HasFacet(string) bool                            { return false }
func (m *rlConnMgr) StartAll(context.Context)                        {}
func (m *rlConnMgr) Wait()                                           {}

const rlNotice = "⚠️ Approaching Anthropic 5-hour rate limit."

func newRLMgr() *rlConnMgr {
	return &rlConnMgr{
		primary:    newRLConn("primary", "clutch/main"),
		sessionKey: "clutch/facet-7",
		session:    newRLConn("session", "clutch/facet-7"),
	}
}

// TestDeliverRateLimitNoticeTargets pins #1857: a usage-limit notice raised by a
// facet/app session must reach THAT session's chat under the default config,
// while "default" preserves the old primary-only behaviour and "both" copies it.
func TestDeliverRateLimitNoticeTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		target      string
		wantSession int
		wantPrimary int
	}{
		{"default config routes to the triggering session", config.RateLimitNotifySession, 1, 0},
		{"default target keeps the pre-#1857 primary delivery", config.RateLimitNotifyDefault, 0, 1},
		{"both delivers to session and default chat", config.RateLimitNotifyBoth, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newRLMgr()
			deliverRateLimitNotice(m, "clutch", m.sessionKey, rlNotice, tc.target)

			if got := len(m.session.notices); got != tc.wantSession {
				t.Errorf("session conn got %d notices %v, want %d", got, m.session.notices, tc.wantSession)
			} else if tc.wantSession > 0 && m.session.notices[0] != rlNotice {
				t.Errorf("session conn got %q, want %q", m.session.notices[0], rlNotice)
			}
			if got := len(m.primary.notices); got != tc.wantPrimary {
				t.Errorf("primary conn got %d notices %v, want %d", got, m.primary.notices, tc.wantPrimary)
			}
		})
	}
}

// TestDeliverRateLimitNoticeFallsBackToPrimary: "session" must still deliver
// when the triggering session has no live connection of its own (a background
// or headless session) — that is PolicyFallback, and it is today's behaviour
// for every session that isn't separately connected.
func TestDeliverRateLimitNoticeFallsBackToPrimary(t *testing.T) {
	t.Parallel()
	m := newRLMgr()
	m.session = nil
	deliverRateLimitNotice(m, "clutch", "clutch/headless", rlNotice, config.RateLimitNotifySession)
	if got := len(m.primary.notices); got != 1 {
		t.Errorf("primary got %d notices, want 1 — a session with no live conn must fall back", got)
	}
}

// TestDeliverRateLimitNoticeBothCollapsesWhenSame: under "both", a session whose
// own connection IS the primary must be notified once, not twice.
func TestDeliverRateLimitNoticeBothCollapsesWhenSame(t *testing.T) {
	t.Parallel()
	m := newRLMgr()
	m.session = m.primary
	deliverRateLimitNotice(m, "clutch", m.sessionKey, rlNotice, config.RateLimitNotifyBoth)
	if got := len(m.primary.notices); got != 1 {
		t.Errorf("primary got %d notices, want 1 (session conn == primary must not double-send)", got)
	}
}

// TestDeliverRateLimitNoticeNoConnection: nothing live anywhere must not panic.
func TestDeliverRateLimitNoticeNoConnection(t *testing.T) {
	t.Parallel()
	m := &rlConnMgr{}
	for _, target := range config.ValidRateLimitNotifyTargets {
		deliverRateLimitNotice(m, "clutch", "clutch/main", rlNotice, target)
	}
}

// TestDeliverRateLimitNoticeBothConfinedSessionStaysInSession: under "both", a
// session whose connection confines replies (a Slack view) must NOT have its
// notice copied to the agent's primary — that would hand one user's usage
// notice to a different audience on another platform (#2459 req 5). The
// session's own conversation still gets it.
func TestDeliverRateLimitNoticeBothConfinedSessionStaysInSession(t *testing.T) {
	t.Parallel()
	m := newRLMgr()
	m.session.confines = true
	deliverRateLimitNotice(m, "clutch", m.sessionKey, rlNotice, config.RateLimitNotifyBoth)
	if got := len(m.session.notices); got != 1 {
		t.Errorf("session conn got %d notices %v, want 1", got, m.session.notices)
	}
	if got := len(m.primary.notices); got != 0 {
		t.Errorf("primary conn got %d notices %v, want 0 — a confined session's notice must not be copied out of its conversation", got, m.primary.notices)
	}
}

// TestDeliverRateLimitNoticeBothConfinedWithoutConnectionStaysOut proves the
// nil-connection arm of the same rule (#2459 req 5): under "both", a session
// whose OWNING platform confines replies — declared by the platform, not by
// any live connection — must not have its notice copied to the primary when
// no live connection serves it. The notice is dropped, never copied out.
func TestDeliverRateLimitNoticeBothConfinedWithoutConnectionStaysOut(t *testing.T) {
	t.Parallel()
	m := newRLMgr()
	m.session = nil
	m.confinedKey = m.sessionKey
	deliverRateLimitNotice(m, "clutch", m.sessionKey, rlNotice, config.RateLimitNotifyBoth)
	if got := len(m.primary.notices); got != 0 {
		t.Errorf("primary conn got %d notices %v, want 0 — a confined session with no live connection drops the notice, never copies it to another platform's chat", got, m.primary.notices)
	}
}

// TestDeliverRateLimitNoticeDropLoggedAsDrop proves the delivery log tells the
// truth for a connection that cannot address a session-blind notice (an
// agent-wide dropper — Slack's unbound conn, reached here through the
// "session" arm's fallback): the outcome line says the notice was dropped by
// that connection, never that it was delivered.
func TestDeliverRateLimitNoticeDropLoggedAsDrop(t *testing.T) {
	t.Parallel()
	logs := captureLog(t, log.DEBUG)
	session := "clutch/" + runToken("rldrop")
	dropper := newRLConn("slack", "")
	dropper.drops = true
	m := &rlConnMgr{primary: dropper, sessionKey: session}

	deliverRateLimitNotice(m, "clutch", session, rlNotice, config.RateLimitNotifySession)

	if got := logs.Lines("DEBUG", "notice dropped by "+dropper.name); len(got) != 1 {
		t.Errorf("drop DEBUG lines = %d, want exactly 1:\n%s", len(got), logs.String())
	}
	if got := logs.Lines("DEBUG", "notice delivered", session); len(got) != 0 {
		t.Errorf("a dropped notice was logged as delivered: %v", got)
	}
}

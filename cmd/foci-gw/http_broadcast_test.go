package main

// http_broadcast_test.go: the PolicyBroadcast delivery set. Two shapes:
//
//   - A session whose connection CONFINES replies (platform.ReplyConfiner —
//     Slack's views) receives the turn's text in its own conversation only:
//     no other platform is addressed, because copying a private
//     conversation's reply to the owner's chats would leak it (#2459 A1
//     req 5). Confinement is a declared capability on the session's
//     connection — never a platform-name comparison.
//   - A session whose platform carries agent-wide sends keeps its exact
//     pre-Slack behaviour through the fan-out alone: the shared-pointer
//     shape (Telegram/Discord) via SendToSession, the carrying-unbound shape
//     (the app, routing to its default conversation) via SendText.
//
// A dropper is never a fan-out target — every test here asserts it, because
// that skip is the one fan-out rule Slack added: an agent-wide send
// addressed at a dropper is a certain drop, so the carrying and
// shared-pointer shapes must reach it through Broadcast's filtered set,
// never through the stored connection list.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"foci/internal/log"
	"foci/internal/platform"
)

// broadcastConn models one connection of a broadcast fan-out: stubConn's
// no-op Connection surface plus recordings of the two delivery methods
// broadcastResponse chooses between. drops models platform.AgentWideDropper
// (Slack's unbound conn): its SendText is the drop the old code logged as a
// delivery ERROR. confines models platform.ReplyConfiner (Slack's views).
// sendErr makes SendToSession fail, for the confined failure arm.
type broadcastConn struct {
	*stubConn
	mu         sync.Mutex
	plat       string
	drops      bool
	confines   bool
	sendErr    error
	sendTexts  []string
	toSessions []string
}

func newBroadcastConn(platformName, sessionKey string, drops bool) *broadcastConn {
	return &broadcastConn{stubConn: &stubConn{sessionKey: sessionKey}, plat: platformName, drops: drops}
}

func (c *broadcastConn) PlatformName() string { return c.plat }

func (c *broadcastConn) DropsAgentWideSends() bool { return c.drops }

// ConfinesReplies is wired per test (the field, not the platform name) so
// the tests prove the delivery decision reads the capability, not "slack".
func (c *broadcastConn) ConfinesReplies() bool { return c.confines }

func (c *broadcastConn) SendText(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendTexts = append(c.sendTexts, text)
	if c.drops {
		return errors.New("slack: no session to address")
	}
	return nil
}

func (c *broadcastConn) SendToSession(key, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toSessions = append(c.toSessions, key+"\x00"+text)
	return c.sendErr
}

func (c *broadcastConn) snapshot() (texts, toSessions []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sendTexts...), append([]string(nil), c.toSessions...)
}

// fanoutConnMgr models the accessor shapes broadcastResponse meets:
// ForSession returns the session's bound view (a value the manager mints per
// call, never one of the stored connections), AllForAgent returns the
// platform primaries. confinedKey models the aggregating manager's
// SessionConfinementResolver answer: the session's chat is owned by a
// platform whose provider declares session-reply confinement, so
// confinement holds with no live connection too.
type fanoutConnMgr struct {
	sessionKey  string
	forSession  platform.Connection
	all         []platform.Connection
	confinedKey string
}

func (m *fanoutConnMgr) SessionPlatformConfinesReplies(sessionKey string) bool {
	return m.confinedKey != "" && sessionKey == m.confinedKey
}

func (m *fanoutConnMgr) Primary(string) platform.Connection { return nil }
func (m *fanoutConnMgr) AllForAgent(string) []platform.Connection {
	return append([]platform.Connection(nil), m.all...)
}
func (m *fanoutConnMgr) ForSession(sk string) platform.Connection {
	if sk == m.sessionKey {
		return m.forSession
	}
	return nil
}
func (m *fanoutConnMgr) ForSessionOrPrimary(sk, _ string) platform.Connection {
	return m.ForSession(sk)
}
func (m *fanoutConnMgr) AcquireFacet(string) (platform.Connection, bool) { return nil, false }
func (m *fanoutConnMgr) HasFacet(string) bool                            { return false }
func (m *fanoutConnMgr) StartAll(context.Context)                        {}
func (m *fanoutConnMgr) Wait()                                           {}

// TestBroadcastResponse_DroppingPlatformSessionGetsOwnConversation proves
// the confined shape (Slack): the session's text goes through SendToSession
// on the view ForSession returned, and NOWHERE else — the fan-out holds an
// ordinary platform's primary, and it is asked for nothing, because copying
// the session's reply there would hand a private conversation's text to a
// different audience. The unbound dropper is likewise never addressed.
func TestBroadcastResponse_DroppingPlatformSessionGetsOwnConversation(t *testing.T) {
	t.Parallel()
	sessionKey := "helen/c100"
	bound := newBroadcastConn("slack", sessionKey, false)
	bound.confines = true                               // the capability, not the platform name, confines
	slackUnbound := newBroadcastConn("slack", "", true) // no default chat: drops agent-wide sends
	telegram := newBroadcastConn("telegram", "helen/c555", false)
	cm := &fanoutConnMgr{sessionKey: sessionKey, forSession: bound, all: []platform.Connection{telegram, slackUnbound}}

	broadcastResponse(cm, "helen", sessionKey, "the reply", "bcast-test")

	_, toSessions := bound.snapshot()
	if len(toSessions) != 1 || toSessions[0] != sessionKey+"\x00the reply" {
		t.Errorf("bound view SendToSession = %v, want exactly one delivery of the reply to %s", toSessions, sessionKey)
	}
	if texts, _ := bound.snapshot(); len(texts) != 0 {
		t.Errorf("bound view was also SendText'd (%v); the session is addressed once, via SendToSession", texts)
	}
	if texts, _ := slackUnbound.snapshot(); len(texts) != 0 {
		t.Errorf("the dropping connection was treated as a delivery target (%v); addressing it can only drop", texts)
	}
	if texts, toSessions := telegram.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("another platform was sent the session's reply (SendText=%v SendToSession=%v); a confined session's text reaches its own conversation only", texts, toSessions)
	}
}

// TestBroadcastResponse_ConfinedSessionWithNoLiveConnectionIsDropped proves
// the nil-connection arm of the confinement rule (#2459 req 5): a session
// whose OWNING platform confines replies — declared by the platform, with
// ForSession returning no live view — is never fanned out. The reply is
// dropped and logged, never handed to another platform's default chat.
func TestBroadcastResponse_ConfinedSessionWithNoLiveConnectionIsDropped(t *testing.T) {
	t.Parallel()
	logs := captureLog(t, log.ERROR)
	sessionKey := "helen/c" + runToken("bcastnil")
	telegram := newBroadcastConn("telegram", "helen/c555", false)
	slackUnbound := newBroadcastConn("slack", "", true)
	cm := &fanoutConnMgr{all: []platform.Connection{telegram, slackUnbound}, confinedKey: sessionKey}

	broadcastResponse(cm, "helen", sessionKey, "the reply", "bcast-test")

	if texts, toSessions := telegram.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("LEAK: another platform was sent a confined session's reply (SendText=%v SendToSession=%v); with no live view the reply is dropped, never fanned out", texts, toSessions)
	}
	if texts, toSessions := slackUnbound.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("the dropper was treated as a delivery target (SendText=%v SendToSession=%v)", texts, toSessions)
	}
	if got := logs.Lines("ERROR", sessionKey, "dropped"); len(got) != 1 {
		t.Errorf("captured %d drop ERROR line(s) for the confined session, want exactly 1:\n%s", len(got), logs.String())
	}
}

// TestBroadcastResponse_ConfinedSendFailureIsLoggedNotFannedOut proves the
// confined shape's failure arm: when the reply-confining view cannot deliver
// to its own conversation (SendToSession fails), the failure is logged naming
// the session and platform, and NOTHING else is addressed — the fan-out must
// not run as a fallback, because its connections deliver to other people's
// chats (#2459 req 5: none of the session's text goes to another
// conversation, on success or failure).
func TestBroadcastResponse_ConfinedSendFailureIsLoggedNotFannedOut(t *testing.T) {
	t.Parallel()
	logs := captureLog(t, log.ERROR)
	sessionKey := "helen/c" + runToken("bcast")
	bound := newBroadcastConn("slack", sessionKey, false)
	bound.confines = true
	bound.sendErr = errors.New("slack: unroutable session")
	telegram := newBroadcastConn("telegram", "helen/c555", false)
	cm := &fanoutConnMgr{sessionKey: sessionKey, forSession: bound, all: []platform.Connection{telegram}}

	broadcastResponse(cm, "helen", sessionKey, "the reply", "bcast-test")

	if _, toSessions := bound.snapshot(); len(toSessions) != 1 || toSessions[0] != sessionKey+"\x00the reply" {
		t.Errorf("bound view SendToSession = %v, want exactly one attempted delivery to %s", toSessions, sessionKey)
	}
	if got := logs.Lines("ERROR", sessionKey, "via slack"); len(got) != 1 {
		t.Errorf("captured %d ERROR line(s) for the failed confined delivery, want exactly 1:\n%s", len(got), logs.String())
	}
	if texts, toSessions := telegram.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("a failed confined delivery fell through to the fan-out (SendText=%v SendToSession=%v); the reply must not reach another platform's chats", texts, toSessions)
	}
}

// TestBroadcastResponse_CarryingPlatformUnchanged proves the app shape keeps
// its exact pre-Slack behaviour with Slack added alongside: ForSession
// returns a fresh view, the fan-out holds the platform's unbound conn plus a
// Slack dropper, and that conn CARRIES agent-wide sends (to the default
// conversation) — the fan-out alone delivers, one SendText, and the session's
// view is not addressed on top. The dropper is asked for nothing: Broadcast
// filters it out, so the only agent-wide route in is the carrying conn.
func TestBroadcastResponse_CarryingPlatformUnchanged(t *testing.T) {
	t.Parallel()
	sessionKey := "helen/c100"
	bound := newBroadcastConn("app", sessionKey, false)
	appUnbound := newBroadcastConn("app", "", false) // unbound, but carries agent-wide sends (the app)
	slackUnbound := newBroadcastConn("slack", "", true)
	cm := &fanoutConnMgr{sessionKey: sessionKey, forSession: bound, all: []platform.Connection{appUnbound, slackUnbound}}

	broadcastResponse(cm, "helen", sessionKey, "the reply", "bcast-test")

	if texts, _ := appUnbound.snapshot(); len(texts) != 1 || texts[0] != "the reply" {
		t.Errorf("app unbound SendText = %v, want exactly [the reply] (its default-conversation route, unchanged)", texts)
	}
	if _, toSessions := bound.snapshot(); len(toSessions) != 0 {
		t.Errorf("session view SendToSession = %v; a carrying platform is covered by the fan-out and needs no extra delivery", toSessions)
	}
	if texts, toSessions := slackUnbound.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("the dropper was treated as a delivery target (SendText=%v SendToSession=%v); addressing it can only drop", texts, toSessions)
	}
}

// TestBroadcastResponse_SharedSessionConnDeliveredOnce proves the
// shared-pointer shape (Telegram/Discord) with Slack added alongside: the
// session's connection is the same value the fan-out holds, and it is
// delivered to exactly once — via SendToSession, never again via SendText.
// The dropper sharing the fan-out is asked for nothing, so the once-only
// delivery comes through Broadcast's filtered set, not by luck of ordering.
func TestBroadcastResponse_SharedSessionConnDeliveredOnce(t *testing.T) {
	t.Parallel()
	sessionKey := "helen/c555"
	telegram := newBroadcastConn("telegram", sessionKey, false)
	slackUnbound := newBroadcastConn("slack", "", true)
	cm := &fanoutConnMgr{sessionKey: sessionKey, forSession: telegram, all: []platform.Connection{telegram, slackUnbound}}

	broadcastResponse(cm, "helen", sessionKey, "the reply", "bcast-test")

	texts, toSessions := telegram.snapshot()
	if len(toSessions) != 1 || toSessions[0] != sessionKey+"\x00the reply" {
		t.Errorf("SendToSession = %v, want exactly one delivery to the session", toSessions)
	}
	if len(texts) != 0 {
		t.Errorf("the session's own connection was also SendText'd (%v); a session is addressed once", texts)
	}
	if texts, toSessions := slackUnbound.snapshot(); len(texts) != 0 || len(toSessions) != 0 {
		t.Errorf("the dropper was treated as a delivery target (SendText=%v SendToSession=%v); addressing it can only drop", texts, toSessions)
	}
}

package platform

import (
	"context"
	"testing"
)

// --- testConnMgr: a ConnectionManager backed by a map of session→connection and a primary ---

type testConnMgr struct {
	primary  Connection
	sessions map[string]Connection
}

func (m *testConnMgr) Primary(string) Connection                     { return m.primary }
func (m *testConnMgr) AllForAgent(string) []Connection               { return nil }
func (m *testConnMgr) ForSession(sk string) Connection               { return m.sessions[sk] }
func (m *testConnMgr) ForSessionOrPrimary(sk, aid string) Connection { return nil } // unused
func (m *testConnMgr) AcquireFacet(string) (Connection, bool)        { return nil, false }
func (m *testConnMgr) HasFacet(string) bool                          { return false }
func (m *testConnMgr) StartAll(context.Context)                      {}
func (m *testConnMgr) Wait()                                         {}

// namedConn is a mockConnection that reports a name for test assertions.
type namedConn struct {
	mockConnection
	name string
}

func (c *namedConn) Username() string { return c.name }

// TestForSessionOrPrimary_PlatformAwareRouting verifies that when ForSession
// returns nil, the aggregating manager uses chatPlatformFn to route to the
// correct platform's primary bot instead of returning the first-match fallback.
func TestForSessionOrPrimary_PlatformAwareRouting(t *testing.T) {
	telegramConn := &namedConn{name: "telegram-bot"}
	discordConn := &namedConn{name: "discord-bot"}

	telegramMgr := &testConnMgr{primary: telegramConn}
	discordMgr := &testConnMgr{primary: discordConn}

	// Registration order: discord first, telegram second.
	// Without platform-aware routing, Primary() would return discord-bot.
	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"discord":  discordMgr,
			"telegram": telegramMgr,
		},
		order: []string{"discord", "telegram"},
		chatPlatformFn: func(agentID string, chatID int64) string {
			if chatID == 42 {
				return "telegram"
			}
			return ""
		},
	}

	// Session key for agent "myagent", chat 42 (format: agent/c<chatID>).
	// ForSession won't match (no session mappings), so it falls through to
	// platform-aware primary.
	got := mgr.ForSessionOrPrimary("myagent/c42", "myagent")
	if got == nil {
		t.Fatal("ForSessionOrPrimary returned nil")
	}
	if got.Username() != "telegram-bot" {
		t.Errorf("got connection %q, want telegram-bot (platform-aware routing should skip discord)", got.Username())
	}
}

// TestForSessionOrPrimary_FallsBackToFirstPrimary verifies that when the
// chatPlatformFn returns "" (unknown platform), the generic first-match
// Primary fallback is used.
func TestForSessionOrPrimary_FallsBackToFirstPrimary(t *testing.T) {
	telegramConn := &namedConn{name: "telegram-bot"}
	discordConn := &namedConn{name: "discord-bot"}

	telegramMgr := &testConnMgr{primary: telegramConn}
	discordMgr := &testConnMgr{primary: discordConn}

	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"discord":  discordMgr,
			"telegram": telegramMgr,
		},
		order: []string{"discord", "telegram"},
		chatPlatformFn: func(string, int64) string {
			return "" // unknown platform
		},
	}

	got := mgr.ForSessionOrPrimary("myagent/c99", "myagent")
	if got == nil {
		t.Fatal("ForSessionOrPrimary returned nil")
	}
	if got.Username() != "discord-bot" {
		t.Errorf("got connection %q, want discord-bot (first in order)", got.Username())
	}
}

// TestForSessionOrPrimary_ForSessionHit verifies that when ForSession
// returns a connection, it is returned directly without consulting
// chatPlatformFn or Primary.
func TestForSessionOrPrimary_ForSessionHit(t *testing.T) {
	sessionConn := &namedConn{name: "session-specific"}
	primaryConn := &namedConn{name: "primary"}

	telegramMgr := &testConnMgr{
		primary:  primaryConn,
		sessions: map[string]Connection{"myagent/c42": sessionConn},
	}

	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"telegram": telegramMgr,
		},
		order: []string{"telegram"},
		chatPlatformFn: func(string, int64) string {
			return "telegram"
		},
	}

	got := mgr.ForSessionOrPrimary("myagent/c42", "myagent")
	if got == nil {
		t.Fatal("ForSessionOrPrimary returned nil")
	}
	if got.Username() != "session-specific" {
		t.Errorf("got connection %q, want session-specific", got.Username())
	}
}

// TestForSessionOrPrimary_NoChatPlatformFn verifies that when
// chatPlatformFn is nil, the generic Primary fallback is used.
func TestForSessionOrPrimary_NoChatPlatformFn(t *testing.T) {
	discordConn := &namedConn{name: "discord-bot"}
	discordMgr := &testConnMgr{primary: discordConn}

	mgr := &aggregatingConnMgr{
		named:          map[string]ConnectionManager{"discord": discordMgr},
		order:          []string{"discord"},
		chatPlatformFn: nil,
	}

	got := mgr.ForSessionOrPrimary("myagent/c42", "myagent")
	if got == nil {
		t.Fatal("ForSessionOrPrimary returned nil")
	}
	if got.Username() != "discord-bot" {
		t.Errorf("got connection %q, want discord-bot", got.Username())
	}
}

// TestForSession_ClaimedButDown_DoesNotMisroute verifies that when a platform owns
// the chat but its ForSession returns nil (connection momentarily down), ForSession
// returns nil rather than falling through and handing the session to another
// platform that happens to have a connection for the key (#990).
func TestForSession_ClaimedButDown_DoesNotMisroute(t *testing.T) {
	appConn := &namedConn{name: "app-bot"}
	telegramMgr := &testConnMgr{sessions: map[string]Connection{}}                  // owner, no live conn
	appMgr := &testConnMgr{sessions: map[string]Connection{"myagent/c42": appConn}} // would be picked by fallback

	mgr := &aggregatingConnMgr{
		named:          map[string]ConnectionManager{"app": appMgr, "telegram": telegramMgr},
		order:          []string{"app", "telegram"},
		chatPlatformFn: func(_ string, chatID int64) string { return map[int64]string{42: "telegram"}[chatID] },
	}

	if got := mgr.ForSession("myagent/c42"); got != nil {
		t.Errorf("ForSession = %q, want nil (telegram owns the chat but is down; must not misroute to app)", got.Username())
	}
}

// TestForSession_Unclaimed_UsesFallback verifies the promiscuous fallback still
// applies when no platform claims the chat (first-message-ever / facet case).
func TestForSession_Unclaimed_UsesFallback(t *testing.T) {
	appConn := &namedConn{name: "app-bot"}
	appMgr := &testConnMgr{sessions: map[string]Connection{"myagent/c99": appConn}}
	mgr := &aggregatingConnMgr{
		named:          map[string]ConnectionManager{"app": appMgr},
		order:          []string{"app"},
		chatPlatformFn: func(string, int64) string { return "" }, // nothing claims it
	}
	if got := mgr.ForSession("myagent/c99"); got != appConn {
		t.Error("ForSession returned nil/wrong conn; unclaimed chat should use the fallback")
	}
}

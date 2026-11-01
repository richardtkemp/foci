package platform

import (
	"context"
	"testing"

	"foci/internal/config"
	"foci/internal/session"
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

// TestAggregatingPrimary_OrderUnchangedWithoutNeverDefault pins the Primary
// fallback order for providers that are NOT never-default platforms: the
// platform owning the default session wins, and with no default session the
// alphabetical `order` fallback picks the first primary. The never-default
// reordering added for Slack (#2459) must leave this ladder unchanged.
func TestAggregatingPrimary_OrderUnchangedWithoutNeverDefault(t *testing.T) {
	telegramConn := &namedConn{name: "telegram-bot"}
	appConn := &namedConn{name: "app-bot"}
	telegramMgr := &testConnMgr{primary: telegramConn}
	appMgr := &testConnMgr{primary: appConn}

	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"telegram": telegramMgr,
			"app":      appMgr,
		},
		order: []string{"app", "telegram"}, // alphabetical, as newAggregatingConnMgr sorts
		defaultSessionKeyFn: func(string) string {
			return "ag/c42" // a default session on a telegram chat
		},
		chatPlatformFn: func(_ string, chatID int64) string {
			if chatID == 42 {
				return "telegram"
			}
			return ""
		},
	}

	// The platform owning the default session is preferred.
	if got := mgr.Primary("ag"); got.Username() != "telegram-bot" {
		t.Errorf("Primary (default session owner) = %q, want telegram-bot", got.Username())
	}

	// No default session at all: alphabetical fallback order.
	mgr.defaultSessionKeyFn = nil
	if got := mgr.Primary("ag"); got.Username() != "app-bot" {
		t.Errorf("Primary (fallback order) = %q, want app-bot (first in order)", got.Username())
	}

	// A default session whose platform has no connection falls back too.
	mgr.defaultSessionKeyFn = func(string) string { return "ag/c42" }
	mgr.chatPlatformFn = func(string, int64) string { return "" }
	if got := mgr.Primary("ag"); got.Username() != "app-bot" {
		t.Errorf("Primary (unowned default) = %q, want app-bot", got.Username())
	}
}

// TestAggregatingPrimary_NeverDefaultProviderOrderedLast proves the
// never-default partition (#2459): with a Slack provider (which declares
// NeverDefaultChat) beside ordinary providers and no default session, the
// Primary fallback prefers the ordinary platform — Slack's unbound
// connection (which can only drop agent-wide sends) is offered last.
func TestAggregatingPrimary_NeverDefaultProviderOrderedLast(t *testing.T) {
	slackConn := &namedConn{name: "slack-unbound"}
	telegramConn := &namedConn{name: "telegram-bot"}
	providers := []MessagingProvider{
		&fakeProvider{name: "slack", mgr: &testConnMgr{primary: slackConn}, never: true},
		&fakeProvider{name: "telegram", mgr: &testConnMgr{primary: telegramConn}},
	}
	// Registration order: slack BEFORE telegram; sorted alphabetically slack
	// would also come first — only the partition moves it last.
	mgr := newAggregatingConnMgr(providers, nil, nil, nil)
	if got := mgr.Primary("ag"); got.Username() != "telegram-bot" {
		t.Errorf("Primary = %q, want telegram-bot (never-default providers order last)", got.Username())
	}
	// With a default session owned by slack, rung resolution still skips it
	// in production because slack chats never carry a default session; here
	// the fallback alone is pinned (the rung's never_default exclusion is
	// pinned in internal/session).
	mgr.defaultSessionKeyFn = func(string) string { return "" }
	if got := mgr.Primary("ag"); got.Username() != "telegram-bot" {
		t.Errorf("Primary (no default) = %q, want telegram-bot", got.Username())
	}
	// Only the never-default platform: it is still returned (last resort
	// beats nil) — dropping happens in the connection, not the manager.
	onlySlack := newAggregatingConnMgr([]MessagingProvider{
		&fakeProvider{name: "slack", mgr: &testConnMgr{primary: slackConn}, never: true},
	}, nil, nil, nil)
	if got := onlySlack.Primary("ag"); got.Username() != "slack-unbound" {
		t.Errorf("Primary (slack only) = %q, want the slack connection (last resort)", got.Username())
	}
}

// TestAggregatingSessionPlatformConfinesReplies proves the aggregator answers
// session-reply confinement from the OWNING platform's declaration
// (ReplyConfiningProvider), with no live connection involved (#2459 req 5):
// true only for a chat claimed by a confining provider — false for an
// ordinary platform's chat, an unclaimed chat, a key with no chat id, and a
// manager with no platform lookup wired.
func TestAggregatingSessionPlatformConfinesReplies(t *testing.T) {
	providers := []MessagingProvider{
		&fakeProvider{name: "slack", mgr: &testConnMgr{}, never: true, confining: true},
		&fakeProvider{name: "telegram", mgr: &testConnMgr{}},
	}
	chatPlatformFn := func(_ string, chatID int64) string {
		return map[int64]string{7: "slack", 9: "telegram"}[chatID]
	}
	mgr := newAggregatingConnMgr(providers, chatPlatformFn, nil, nil)

	if !mgr.SessionPlatformConfinesReplies("ag/c7") {
		t.Error("chat owned by the confining platform: want the confinement declared")
	}
	if mgr.SessionPlatformConfinesReplies("ag/c9") {
		t.Error("chat owned by an ordinary platform: want no confinement")
	}
	if mgr.SessionPlatformConfinesReplies("ag/c42") {
		t.Error("unclaimed chat: want no confinement")
	}
	if mgr.SessionPlatformConfinesReplies("ag/iwork") {
		t.Error("key with no chat id: want no confinement")
	}

	// A key with no chat id has no owning platform to ask — the manager
	// must not consult the lookup at all for it (the guard has the same
	// shape as ForSession's), whatever a degenerate lookup would answer
	// for chat id 0.
	degenerate := newAggregatingConnMgr(providers, func(string, int64) string { return "slack" }, nil, nil)
	if degenerate.SessionPlatformConfinesReplies("ag/iwork") {
		t.Error("key with no chat id: want no confinement — no platform owns a chat that does not exist")
	}

	// No platform lookup at all: nothing is known to confine.
	noLookup := newAggregatingConnMgr(providers, nil, nil, nil)
	if noLookup.SessionPlatformConfinesReplies("ag/c7") {
		t.Error("manager without a platform lookup: want no confinement")
	}
}

// fakeProvider is a MessagingProvider stub whose optional capability
// answers (NeverDefaultChat, ConfinesSessionReplies) are configurable.
type fakeProvider struct {
	name      string
	mgr       ConnectionManager
	never     bool
	confining bool
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) IsConfigured(*config.Config) (bool, string) {
	return true, ""
}
func (p *fakeProvider) Init(ProviderDeps) error              { return nil }
func (p *fakeProvider) ConnectionManager() ConnectionManager { return p.mgr }
func (p *fakeProvider) SetupAgentConnection(AgentConnectionParams) *SetupResult {
	return nil
}
func (p *fakeProvider) SetupSharedFacet(SharedFacetParams)                  {}
func (p *fakeProvider) RestoreFacetSessions(RestoreParams)                  {}
func (p *fakeProvider) SetLifecycleCallback(string, LifecycleEvent, func()) {}
func (p *fakeProvider) ToolDetailStore() ToolDetailStore                    { return nil }
func (p *fakeProvider) AgentPreFlight(string) []string                      { return nil }
func (p *fakeProvider) DefaultPlatformConfig() config.PlatformConfig {
	return config.PlatformConfig{}
}
func (p *fakeProvider) ValidateConfig(config.PlatformConfig) []string { return nil }
func (p *fakeProvider) Close() error                                  { return nil }
func (p *fakeProvider) NeverDefaultChat() bool                        { return p.never }
func (p *fakeProvider) ConfinesSessionReplies() bool                  { return p.confining }

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

// TestForSessionOrPrimary_ClaimedButOwnerOffline is the #1493 regression: a chat CLAIMED by one
// platform whose session connection AND primary are both absent must return nil, not fall through
// to the first-available primary of some OTHER platform.
//
// Observed 2026-07-22 before the fix: an app-owned session, app client not yet reconnected after a
// restart, produced ten consecutive
//
//	ERROR [telegram:clutch] send: unable to sendMessage: Bad Request: chat not found
//
// because the app's conversation id was handed to the Telegram API. nil instead routes delivery to
// the owning platform's offline path (queue/push).
//
// Note the ordering here is deliberate: "app" is registered but has no primary, and "telegram" DOES
// have one and sorts first in `order` — i.e. the exact configuration that produced the misroute.
func TestForSessionOrPrimary_ClaimedButOwnerOffline(t *testing.T) {
	telegramConn := &namedConn{name: "telegram-bot"}

	appMgr := &testConnMgr{primary: nil}               // owner is up but has no primary yet
	telegramMgr := &testConnMgr{primary: telegramConn} // a tempting, wrong, first-in-order match

	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"telegram": telegramMgr,
			"app":      appMgr,
		},
		order: []string{"telegram", "app"},
		chatPlatformFn: func(string, int64) string {
			return "app" // the chat IS claimed, by the platform that is offline
		},
	}

	if got := mgr.ForSessionOrPrimary("myagent/c42", "myagent"); got != nil {
		t.Fatalf("claimed-but-offline chat routed to %q; want nil so delivery takes the offline path", got.Username())
	}
}

// TestForSessionOrPrimary_UnclaimedStillFallsBack guards the other half of the #1493 fix: the
// unclaimed fallback must be UNCHANGED. A chat with no owner (first message ever, facet bots, no
// chat_metadata row) has nothing to misroute away from, so guessing a primary is still correct.
// Without this, a fix that returned nil unconditionally would look green against the test above.
func TestForSessionOrPrimary_UnclaimedStillFallsBack(t *testing.T) {
	telegramConn := &namedConn{name: "telegram-bot"}
	telegramMgr := &testConnMgr{primary: telegramConn}
	appMgr := &testConnMgr{primary: nil}

	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{
			"telegram": telegramMgr,
			"app":      appMgr,
		},
		order: []string{"telegram", "app"},
		chatPlatformFn: func(string, int64) string {
			return "" // NOT claimed
		},
	}

	got := mgr.ForSessionOrPrimary("myagent/c99", "myagent")
	if got == nil {
		t.Fatal("unclaimed chat returned nil; the first-primary fallback must survive the #1493 fix")
	}
	if got.Username() != "telegram-bot" {
		t.Errorf("got %q, want telegram-bot (first in order)", got.Username())
	}
}

// #2662: a cross-agent spawn's session reaches no chat of the agent it runs
// as — neither its own (ForSession) nor the agent's primary.
func TestCrossAgentSpawnSessionReachesNoChat(t *testing.T) {
	appConn := &namedConn{name: "app-bot"}
	branch := session.CrossAgentSpawnRoot("helen") + "/b1793349772615083693"
	appMgr := &testConnMgr{primary: appConn, sessions: map[string]Connection{branch: appConn}}
	mgr := &aggregatingConnMgr{
		named: map[string]ConnectionManager{"app": appMgr},
		order: []string{"app"},
	}
	if c := mgr.ForSession(branch); c != nil {
		t.Errorf("ForSession(%s) = %v, want nil", branch, c)
	}
	if c := mgr.ForSessionOrPrimary(branch, "helen"); c != nil {
		t.Errorf("ForSessionOrPrimary(%s) = %v, want nil", branch, c)
	}
	// Control: an ordinary independent session still falls back to the primary.
	if c := mgr.ForSessionOrPrimary("helen/ibatch/b1", "helen"); c != appConn {
		t.Errorf("control: ForSessionOrPrimary(helen/ibatch/b1) = %v, want the primary", c)
	}
}

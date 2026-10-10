package main

import (
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/question"
)

// chatRoutingConn is a stubConn shaped like a Telegram/Discord primary bot: it
// implements BOTH ButtonSender (default-chat operations) and
// platform.SessionButtonSender (session-addressed operations), recording which
// surface each call used and what it carried — so the #2275 routing tests can
// assert which chat a prompt reached.
type chatRoutingConn struct {
	stubConn
	defaultSessionKey string

	mu               sync.Mutex
	sessionSends     int
	sessionTextEdits int
	sessionBtnEdits  int
	defaultSends     int
	defaultTextEdits int
	defaultBtnEdits  int
	lastSessionKey   string
	lastSendText     string
}

func (c *chatRoutingConn) PlatformName() string              { return "telegram" }
func (c *chatRoutingConn) DefaultSessionKey() string         { return c.defaultSessionKey }
func (c *chatRoutingConn) ChatID() int64                     { return 222 } // the LAST chat
func (c *chatRoutingConn) SendDocument(string, string) error { return nil }

func (c *chatRoutingConn) SendTextWithButtons(text string, _ []platform.ButtonChoice, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultSends++
	c.lastSendText = text
	return "m-default", nil
}

func (c *chatRoutingConn) EditMessageText(string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultTextEdits++
	return nil
}

func (c *chatRoutingConn) EditMessageWithButtons(string, string, []platform.ButtonChoice, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultBtnEdits++
	return nil
}

func (c *chatRoutingConn) SendTextWithButtonsToSession(sessionKey, text string, _ []platform.ButtonChoice, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionSends++
	c.lastSessionKey = sessionKey
	c.lastSendText = text
	return "m-session", nil
}

func (c *chatRoutingConn) EditMessageTextInSession(sessionKey, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionTextEdits++
	c.lastSessionKey = sessionKey
	return nil
}

func (c *chatRoutingConn) EditMessageWithButtonsInSession(sessionKey, _, _ string, _ []platform.ButtonChoice, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionBtnEdits++
	c.lastSessionKey = sessionKey
	return nil
}

// counts snapshots the recording counters.
func (c *chatRoutingConn) counts() (sends, sText, sBtn, dSends, dText, dBtn int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionSends, c.sessionTextEdits, c.sessionBtnEdits, c.defaultSends, c.defaultTextEdits, c.defaultBtnEdits
}

// TestSessionChatResolver_WrapsSessionCapableConn proves the session-chat
// resolver wraps a SessionButtonSender connection so its ButtonSender methods
// address the session's chat (#2275).
func TestSessionChatResolver_WrapsSessionCapableConn(t *testing.T) {
	conn := &chatRoutingConn{stubConn: stubConn{sessionKey: "main/c999"}, defaultSessionKey: "main/c999"}
	resolve := sessionChatResolver(oneConnMgr{conn: conn}, "main/c222", "main")

	bs, ok := resolve().(platform.ButtonSender)
	if !ok {
		t.Fatal("wrapped connection is not a ButtonSender")
	}
	if _, err := bs.SendTextWithButtons("Q?", nil, "im:"); err != nil {
		t.Fatal(err)
	}
	if err := bs.EditMessageText("1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := bs.EditMessageWithButtons("1", "again?", nil, "im:"); err != nil {
		t.Fatal(err)
	}

	sends, sText, sBtn, dSends, dText, dBtn := conn.counts()
	if sends != 1 || sText != 1 || sBtn != 1 {
		t.Errorf("session-addressed calls = (%d,%d,%d), want 1 each", sends, sText, sBtn)
	}
	if dSends != 0 || dText != 0 || dBtn != 0 {
		t.Errorf("default-chat calls = (%d,%d,%d), want 0 each (the wrapper must reroute them)", dSends, dText, dBtn)
	}
	if conn.lastSessionKey != "main/c222" {
		t.Errorf("session-addressed calls used session %q, want main/c222", conn.lastSessionKey)
	}
}

// TestSessionChatResolver_NilConnStaysNil proves a resolver over no live
// connection yields nil (the caller treats that as "skip the edit").
func TestSessionChatResolver_NilConnStaysNil(t *testing.T) {
	if got := sessionChatResolver(oneConnMgr{conn: nil}, "main/c222", "main")(); got != nil {
		t.Errorf("resolver returned %v, want nil", got)
	}
}

// TestSessionChatResolver_ReturnsAppConnUnchanged pins the app path: a
// connection without SessionButtonSender is already session-bound, so it is
// returned AS IS (its InteractiveHeaderSetter / InteractiveRemover /
// BatchButtonSender capabilities keep working through the resolver).
func TestSessionChatResolver_ReturnsAppConnUnchanged(t *testing.T) {
	conn := &appishConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c222"}}}
	got := sessionChatResolver(oneConnMgr{conn: conn}, "main/c222", "main")()
	if got != platform.Connection(conn) {
		t.Errorf("resolver returned %T, want the connection unchanged (not wrapped)", got)
	}
}

// appishConn is an app-like connection: ButtonSender, no SessionButtonSender.
type appishConn struct {
	buttonStubConn
	lastText string
}

func (c *appishConn) PlatformName() string { return "app" }

func (c *appishConn) SendTextWithButtons(text string, _ []platform.ButtonChoice, _ string) (string, error) {
	c.lastText = text
	return "m-1", nil
}

// askChoices is the shared ask question shape for the routing tests.
func askChoices() []question.Choice {
	return []question.Choice{{Label: "Yes", Data: "qa:0"}, {Label: "Cancel", Data: question.CancelData}}
}

// TestAskPrompt_PostsToAskingSessionChat proves #2275: through newAskPresentFn
// on a Telegram-like connection, an ask from a non-default chat (main/c222,
// default 999) is POSTED to the session's own chat, not the default one.
func TestAskPrompt_PostsToAskingSessionChat(t *testing.T) {
	conn := &chatRoutingConn{stubConn: stubConn{sessionKey: "main/c999"}, defaultSessionKey: "main/c999"}

	id := newAskPresentFn("main", oneConnMgr{conn: conn})("main/c222", "ask-routing-1", "Q?", "Q", askChoices(), func(string) {})

	if id != "m-session" {
		t.Errorf("platform msg id = %q, want the session-addressed send's id", id)
	}
	sends, _, _, dSends, _, _ := conn.counts()
	if sends != 1 || dSends != 0 {
		t.Errorf("session sends = %d, default sends = %d, want 1/0 (the ask goes to the asking session's chat)", sends, dSends)
	}
	if conn.lastSessionKey != "main/c222" {
		t.Errorf("send addressed session %q, want main/c222", conn.lastSessionKey)
	}
	platform.CancelInteractiveMessage("ask-routing-1", "done") // keep the global store clean for the sweep tests
}

// TestAskPrompt_CancelEditsAskingSessionChat proves a cancelled ask's message
// edit addresses the asking session's chat (the stored resolver is the
// session-chat one) (#2275).
func TestAskPrompt_CancelEditsAskingSessionChat(t *testing.T) {
	conn := &chatRoutingConn{stubConn: stubConn{sessionKey: "main/c999"}, defaultSessionKey: "main/c999"}

	newAskPresentFn("main", oneConnMgr{conn: conn})("main/c222", "ask-routing-2", "Q?", "Q", askChoices(), func(string) {})
	if err := platform.CancelInteractiveMessage("ask-routing-2", "❌ Cancelled"); err != nil {
		t.Fatalf("CancelInteractiveMessage: %v", err)
	}

	_, sText, _, _, dText, _ := conn.counts()
	if sText != 1 {
		t.Errorf("session-chat text edits = %d, want 1 (cancel edits the asking session's chat)", sText)
	}
	if dText != 0 {
		t.Errorf("default-chat text edits = %d, want 0", dText)
	}
	if conn.lastSessionKey != "main/c222" {
		t.Errorf("edit addressed session %q, want main/c222", conn.lastSessionKey)
	}
}

// TestAskPrompt_ExpiryEditsAskingSessionChat proves the expiry sweep's message
// edit addresses the asking session's chat too. Not parallel: it sweeps the
// process-global interactive prompt store.
func TestAskPrompt_ExpiryEditsAskingSessionChat(t *testing.T) {
	conn := &chatRoutingConn{stubConn: stubConn{sessionKey: "main/c999"}, defaultSessionKey: "main/c999"}

	newAskPresentFn("main", oneConnMgr{conn: conn})("main/c222", "ask-routing-3", "Q?", "Q", askChoices(), func(string) {})
	platform.CleanupExpiredInteractive(-time.Hour) // every entry is older than a cutoff in the future

	_, sText, _, _, dText, _ := conn.counts()
	if sText != 1 {
		t.Errorf("session-chat text edits = %d, want 1 (expiry edits the asking session's chat)", sText)
	}
	if dText != 0 {
		t.Errorf("default-chat text edits = %d, want 0", dText)
	}
}

// TestAskPrompt_RestoreCancelEditsAskingSessionChat proves a RESTORED ask (after
// a restart) edits in the asking session's chat when cancelled (#2275).
func TestAskPrompt_RestoreCancelEditsAskingSessionChat(t *testing.T) {
	conn := &chatRoutingConn{stubConn: stubConn{sessionKey: "main/c999"}, defaultSessionKey: "main/c999"}

	newAskRestoreFn("main", oneConnMgr{conn: conn})("main/c222", "ask-routing-4", "m-9", askChoices(), func(string) {})
	if err := platform.CancelInteractiveMessage("ask-routing-4", "❌ Cancelled"); err != nil {
		t.Fatalf("CancelInteractiveMessage: %v", err)
	}

	_, sText, _, _, dText, _ := conn.counts()
	if sText != 1 || dText != 0 {
		t.Errorf("edits session=%d default=%d, want 1/0 (a restored ask's cancel edit addresses the asking session's chat)", sText, dText)
	}
	if conn.lastSessionKey != "main/c222" {
		t.Errorf("edit addressed session %q, want main/c222", conn.lastSessionKey)
	}
}

// TestAskPrompt_AppConnUsedUnchanged pins the app path: through newAskPresentFn
// on an app-like connection (no SessionButtonSender), the connection is used
// unchanged — its own (session-bound) ButtonSender posts the ask (#2275
// characterisation).
func TestAskPrompt_AppConnUsedUnchanged(t *testing.T) {
	conn := &appishConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c222"}}}
	if id := newAskPresentFn("main", oneConnMgr{conn: conn})("main/c222", "ask-routing-5", "Q?", "Q", askChoices(), func(string) {}); id != "m-1" {
		t.Errorf("platform msg id = %q, want the app connection's own send id (m-1)", id)
	}
	platform.CancelInteractiveMessage("ask-routing-5", "done") // keep the global store clean for the sweep tests
}

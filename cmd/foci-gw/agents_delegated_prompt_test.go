package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// docRoutingConn extends chatRoutingConn with document-send recording, for
// the prompt-attachment tests.
type docRoutingConn struct {
	chatRoutingConn
	lastChat  int64
	lastPath  string
	docToChat int
	docs      int
}

func (c *docRoutingConn) SendDocument(path, _ string) error {
	c.docs++
	c.lastPath = path
	return nil
}

func (c *docRoutingConn) SendDocumentToChat(chatID int64, path, _ string) error {
	c.docToChat++
	c.lastChat = chatID
	c.lastPath = path
	return nil
}

// promptClosures builds the wired prompt pair (approval → default chat,
// question → session chat) against a fake connection manager: a bare agent
// (nil DelegatedManager would panic on RegisterPromptCancelListener; a
// zero-value manager accepts the registration as a no-op) and, when non-nil, a
// real session index for the origin line.
func promptClosures(conn platform.Connection, sessionIdx *session.SessionIndex) (approval, question func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)) {
	return backendPromptFuncs(backendPromptDeps{
		ag:         &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
		connMgr:    oneConnMgr{conn: conn},
		sessionIdx: sessionIdx,
		agentID:    "main",
	})
}

// promptChoices is the shared Allow/Deny shape for the prompt tests.
func promptChoices() []delegator.PromptChoice {
	return []delegator.PromptChoice{{Label: "Allow", Data: "allow"}, {Label: "Deny", Data: "deny"}}
}

// telegramLikeConn builds a Telegram-like fake with default chat 999.
func telegramLikeConn() *docRoutingConn {
	c := &docRoutingConn{}
	c.stubConn = stubConn{sessionKey: "main/c999"}
	c.defaultSessionKey = "main/c999"
	return c
}

// TestQuestionPrompt_PostsToAskingSessionChat proves #2275: the question
// prompt kind posts through the session-addressed surface — a backend question
// from a non-default chat reaches that chat, not the default one.
func TestQuestionPrompt_PostsToAskingSessionChat(t *testing.T) {
	conn := telegramLikeConn()

	_, question := promptClosures(conn, nil)
	question("main/c222", "req-q-2275", "Which colour?", "Question", "", promptChoices())

	sends, _, _, dSends, _, _ := conn.counts()
	if sends != 1 || dSends != 0 {
		t.Errorf("session sends = %d, default sends = %d, want 1/0 (a question goes to the asking session's chat)", sends, dSends)
	}
	if conn.lastSessionKey != "main/c222" {
		t.Errorf("send addressed session %q, want main/c222", conn.lastSessionKey)
	}
	platform.CancelInteractiveMessage("req-q-2275", "done") // keep the global store clean for the sweep tests
}

// TestPermissionPrompt_UsesDefaultChatButtonSender pins Dick's 2026-10-09
// ruling (#2275 characterisation): an approval prompt from a non-default chat
// still posts to the DEFAULT chat via the plain ButtonSender surface.
func TestPermissionPrompt_UsesDefaultChatButtonSender(t *testing.T) {
	conn := telegramLikeConn()

	approval, _ := promptClosures(conn, nil)
	approval("main/c222", "req-p-2275", "Allow Bash?", "Bash", "", promptChoices())

	sends, _, _, dSends, _, _ := conn.counts()
	if dSends != 1 || sends != 0 {
		t.Errorf("default sends = %d, session sends = %d, want 1/0 (approvals stay in the default chat)", dSends, sends)
	}
	platform.CancelInteractiveMessage("req-p-2275", "done") // keep the global store clean for the sweep tests
}

// newOriginIndex seeds a session index with chat 222's username for the
// origin-line tests.
func newOriginIndex(t *testing.T, username string) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	if username != "" {
		if err := idx.SetChatMetadata("main", "telegram", 222, "username", username); err != nil {
			t.Fatal(err)
		}
	}
	return idx
}

// TestPermissionPrompt_OriginLine proves #2275: an approval prompt shown in a
// chat that is not the requesting session's own is prefixed with one origin
// line naming the session key, the platform, the chat ID and the recorded
// username (each segment omitted when unknown).
func TestPermissionPrompt_OriginLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		session  string
		username string
		want     string
	}{
		{"other chat with username", "main/c222", "bob", "🔐 Request from session main/c222 (telegram chat 222, @bob)"},
		{"other chat without username", "main/c222", "", "🔐 Request from session main/c222 (telegram chat 222)"},
		{"chatless key", "main/iwork", "bob", "🔐 Request from session main/iwork (telegram)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := telegramLikeConn()
			idx := newOriginIndex(t, tc.username)

			approval, _ := promptClosures(conn, idx)
			approval(tc.session, "req-origin", "Allow Bash?", "Bash", "", promptChoices())

			_, _, _, dSends, _, _ := conn.counts()
			if dSends != 1 {
				t.Fatalf("default sends = %d, want 1", dSends)
			}
			if !strings.HasPrefix(conn.lastSendText, tc.want) {
				t.Errorf("prompt text = %q, want it to start with the origin line %q", conn.lastSendText, tc.want)
			}
			if !strings.Contains(conn.lastSendText, "Allow Bash?") {
				t.Errorf("prompt text = %q, want the original body preserved after the origin line", conn.lastSendText)
			}
			platform.CancelInteractiveMessage("req-origin", "done") // keep the global store clean for the sweep tests
		})
	}
}

// TestPermissionPrompt_NoOriginForOwnSessions pins #2275: prompts for the
// default chat's own sessions — including branch keys — and prompts on the app
// (already session-bound) carry NO origin line, so the owner's own prompts look
// exactly as before.
func TestPermissionPrompt_NoOriginForOwnSessions(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn *docRoutingConn
		sk   string
	}{
		{"default chat root", telegramLikeConn(), "main/c999"},
		{"default chat branch", telegramLikeConn(), "main/c999/b1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newOriginIndex(t, "bob")

			approval, _ := promptClosures(tc.conn, idx)
			approval(tc.sk, "req-no-origin", "Allow Bash?", "Bash", "", promptChoices())

			if strings.Contains(tc.conn.lastSendText, "🔐") {
				t.Errorf("prompt text = %q, want no origin line for the default chat's own session", tc.conn.lastSendText)
			}
			platform.CancelInteractiveMessage("req-no-origin", "done") // keep the global store clean for the sweep tests
		})
	}

	t.Run("app connection", func(t *testing.T) {
		conn := &appishConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c222"}}}
		idx := newOriginIndex(t, "bob")

		approval, _ := promptClosures(conn, idx)
		approval("main/c222", "req-no-origin-app", "Allow Bash?", "Bash", "", promptChoices())

		if strings.Contains(conn.lastText, "🔐") {
			t.Errorf("prompt text = %q, want no origin line on the app (already session-bound)", conn.lastText)
		}
		platform.CancelInteractiveMessage("req-no-origin-app", "done") // keep the global store clean for the sweep tests
	})
}

// TestPermissionPrompt_AttachmentGoesToDefaultChat proves #2275: the plan
// attachment is sent to the chat the buttons go to — the DEFAULT chat, not the
// LAST chat (which differs when two users share the bot).
func TestPermissionPrompt_AttachmentGoesToDefaultChat(t *testing.T) {
	conn := telegramLikeConn() // DefaultSessionKey main/c999; ChatID() (last chat) = 222

	approval, _ := promptClosures(conn, nil)
	approval("main/c222", "req-att", "Plan ready?", "Plan", "/tmp/plan.md", promptChoices())

	if conn.docToChat != 1 || conn.lastChat != 999 {
		t.Errorf("SendDocumentToChat calls = %d (chat %d), want 1 to chat 999 (the chat the buttons go to)", conn.docToChat, conn.lastChat)
	}
	if conn.docs != 0 {
		t.Errorf("plain SendDocument calls = %d, want 0 (the last-chat send is the wrong chat here)", conn.docs)
	}
	if conn.lastPath != "/tmp/plan.md" {
		t.Errorf("attachment path = %q, want /tmp/plan.md", conn.lastPath)
	}
	platform.CancelInteractiveMessage("req-att", "done") // keep the global store clean for the sweep tests
}

// TestPermissionPrompt_AttachmentAppConnUnchanged pins the app path: the
// attachment goes through the connection's own SendDocument — the app is
// session-bound and needs no chat resolution (#2275 characterisation).
func TestPermissionPrompt_AttachmentAppConnUnchanged(t *testing.T) {
	conn := &appDocConn{appishConn: appishConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c222"}}}}

	approval, _ := promptClosures(conn, nil)
	approval("main/c222", "req-att-app", "Plan ready?", "Plan", "/tmp/plan.md", promptChoices())

	if conn.docs != 1 || conn.chatDocs != 0 {
		t.Errorf("SendDocument = %d, SendDocumentToChat = %d, want 1/0 (app connection unchanged)", conn.docs, conn.chatDocs)
	}
	platform.CancelInteractiveMessage("req-att-app", "done") // keep the global store clean for the sweep tests
}

// appDocConn records document sends on the app-like fake.
type appDocConn struct {
	appishConn
	docs     int
	chatDocs int
}

func (c *appDocConn) SendDocument(string, string) error {
	c.docs++
	return nil
}

func (c *appDocConn) SendDocumentToChat(int64, string, string) error {
	c.chatDocs++
	return nil
}

// TestBackendPrompt_NilConnectionDropped proves both prompt closures survive a
// resolver with no live connection (platform down, prompt arriving during a
// restart): each prompt is dropped with a WARN naming the session, not a
// panic — the body dereferences the connection right after the nil check.
// The warn hook is collected (after draining any startup-buffered entries) so
// the WARNs do not linger in the global buffer for the next hook to replay.
func TestBackendPrompt_NilConnectionDropped(t *testing.T) {
	log.SetWarnHook(func(log.Level, string, string) {}) // drain buffered entries
	var mu sync.Mutex
	var warns []string
	log.SetWarnHook(func(level log.Level, _, msg string) {
		if level != log.WARN {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		warns = append(warns, msg)
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	approval, question := promptClosures(nil, nil)
	approval("main/c222", "req-nil-1", "Allow Bash?", "Bash", "", promptChoices())
	question("main/c222", "req-nil-2", "Which colour?", "Question", "", promptChoices())

	mu.Lock()
	defer mu.Unlock()
	if len(warns) != 2 {
		t.Fatalf("drop WARNs = %d (%v), want 2 (one per kind, no panic)", len(warns), warns)
	}
	for _, msg := range warns {
		if !strings.Contains(msg, "prompt dropped") || !strings.Contains(msg, "main/c222") {
			t.Errorf("drop WARN %q, want it to name the session and the drop", msg)
		}
	}
}

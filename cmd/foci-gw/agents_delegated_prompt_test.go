package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/askgw"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/question"
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
// real session index for the origin line. The refuserConn fake lives in
// agents_delegated_refuser_test.go (a base-compilable file), shared with the
// question-boundary characterisation there.
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestBackendPrompt_NilConnectionRefused proves both prompt closures survive a
// resolver with no live connection (platform down, prompt arriving during a
// restart): each prompt is refused back to the backend with a WARN naming the
// session (#2608: undeliverable must fail, never wait), not a panic and not a
// silent drop. The WARNs are read through a log tap and tied to this run by its
// own session key, so the test needs no process-wide hook and runs in parallel.
func TestBackendPrompt_NilConnectionRefused(t *testing.T) {
	t.Parallel()
	logs := captureLog(t, log.WARN)
	session := "main/" + runToken("nilconn")

	rec := &failRecorder{}
	approval, question := backendPromptFuncs(backendPromptDeps{
		ag:         &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
		connMgr:    oneConnMgr{},
		agentID:    "main",
		failPrompt: rec.fail,
	})
	approval(session, "req-nil-1", "Allow Bash?", "Bash", "", promptChoices())
	question(session, "req-nil-2", "Which colour?", "Question", "", promptChoices())

	if rec.calls != 2 {
		t.Errorf("refusals = %d, want 2 (one per kind)", rec.calls)
	}
	warns := logs.Lines("WARN", session)
	if len(warns) != 2 {
		t.Fatalf("refusal WARNs = %d (%v), want 2 (one per kind, no panic)", len(warns), warns)
	}
	for _, l := range warns {
		if !strings.Contains(l.Msg, "undeliverable, refused") || !strings.Contains(l.Msg, "sk="+session+" ") {
			t.Errorf("refusal WARN %q, want it to name the session and the refusal", l.Msg)
		}
	}
}

// refuserConn is defined in agents_delegated_refuser_test.go; the
// fail-closed tests below drive it through the respond seam.

// promptClosuresWithRespond is promptClosures with a respond recorder, so a
// fail-closed denial is asserted without a live backend.
func promptClosuresWithRespond(conn platform.Connection, respond func(ctx context.Context, sessionKey, requestID, choice string) error) (approval, question func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)) {
	return backendPromptFuncs(backendPromptDeps{
		ag:      &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
		connMgr: oneConnMgr{conn: conn},
		agentID: "main",
		respond: respond,
	})
}

// TestPermissionPrompt_RefusedConnDeniesNotifiesAndPostsNothing proves the
// interim approval rule (#2459 A1, req 7): an approval raised on a
// connection whose platform cannot show one — the plain permission prompt
// AND the ExitPlanMode plan attachment — is denied at once, the requesting
// conversation gets a short notice, nothing is posted, nothing is attached,
// and nothing is left registered in the interactive store.
func TestPermissionPrompt_RefusedConnDeniesNotifiesAndPostsNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attachment string
	}{
		{"permission prompt", ""},
		{"ExitPlanMode plan attachment", "/tmp/plan.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &refuserConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c77"}}}
			logs := captureLog(t, log.WARN)

			var denials []string
			approval, _ := promptClosuresWithRespond(conn, func(_ context.Context, _, requestID, choice string) error {
				denials = append(denials, requestID+"="+choice)
				return nil
			})
			approval("main/c77", "req-refusal", "Allow Bash?", "Bash", tc.attachment, promptChoices())

			if len(denials) != 1 || denials[0] != "req-refusal=deny" {
				t.Errorf("denials = %v, want exactly one deny for req-refusal", denials)
			}
			if conn.buttonSend != 0 {
				t.Errorf("button sends = %d, want 0 (the prompt must never be shown)", conn.buttonSend)
			}
			if conn.documents != 0 {
				t.Errorf("documents = %d, want 0 (the plan attachment is not sent)", conn.documents)
			}
			if len(conn.notices) != 1 || !strings.Contains(conn.notices[0], "Bash") {
				t.Errorf("notices = %v, want one short notice naming the request", conn.notices)
			}
			if warns := logs.Lines("denied"); len(warns) != 1 {
				t.Errorf("denial WARNs = %d, want 1 (%v)", len(warns), warns)
			}
			// Nothing left pending: the interactive store holds no entry.
			if err := platform.CancelInteractiveMessage("req-refusal", "x"); err != nil {
				t.Errorf("cancel: %v", err)
			}
		})
	}
}

// TestPermissionPrompt_RefusalNoticeHasSubjectWithoutSummary proves the
// denial notice keeps a subject when the prompt carried no summary: an empty
// summary must not collapse the notice to "🔒  needs an approval" (no
// subject, a double space).
func TestPermissionPrompt_RefusalNoticeHasSubjectWithoutSummary(t *testing.T) {
	conn := &refuserConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c77"}}}
	approval, _ := promptClosuresWithRespond(conn, func(_ context.Context, _, _, _ string) error { return nil })
	approval("main/c77", "req-no-summary", "Allow Bash?", "", "", promptChoices())

	if len(conn.notices) != 1 {
		t.Fatalf("notices = %v, want one", conn.notices)
	}
	if strings.Contains(conn.notices[0], "  needs") {
		t.Errorf("notice has no subject: %q", conn.notices[0])
	}
	if !strings.Contains(conn.notices[0], "needs an approval") {
		t.Errorf("notice lost its sentence: %q", conn.notices[0])
	}
}

// TestPermissionPrompt_ButtonPressDeliversChoiceToBackend proves the press
// path of a posted permission prompt: the button callback resolves the
// backend's permission waiter with the pressed choice through
// backendPromptDeps' response seam — the same one a fail-closed denial uses —
// and not only the ✅ UI edit. A press that edited the message but never
// reached the backend would leave the turn blocked forever.
func TestPermissionPrompt_ButtonPressDeliversChoiceToBackend(t *testing.T) {
	t.Parallel()
	conn := telegramLikeConn()

	var decisions []string
	approval, _ := promptClosuresWithRespond(conn, func(_ context.Context, _, requestID, choice string) error {
		decisions = append(decisions, requestID+"="+choice)
		return nil
	})
	approval("main/c999", "req-press", "Allow Bash?", "Bash", "", promptChoices())

	edit, _, ok := platform.HandleInteractiveCallback("req-press:0") // the Allow button
	if !ok {
		t.Fatal("button press did not find the registered prompt")
	}
	if len(decisions) != 1 || decisions[0] != "req-press=allow" {
		t.Errorf("backend decisions = %v, want [req-press=allow] (the press reaches the permission waiter)", decisions)
	}
	if edit != "✅ Bash" {
		t.Errorf("press edit = %q, want %q", edit, "✅ Bash")
	}
}

// TestPermissionPrompt_ExpiryDeniesToBackend proves the expiry path: when the
// interactive sweep retires an unanswered permission prompt, the backend's
// waiter is resolved as a denial through the same response seam as a press,
// so a turn blocked in WaitForPermission unblocks instead of orphaning.
// Not parallel: CleanupExpiredInteractive sweeps the process-global prompt store.
func TestPermissionPrompt_ExpiryDeniesToBackend(t *testing.T) {
	conn := telegramLikeConn()

	var decisions []string
	approval, _ := promptClosuresWithRespond(conn, func(_ context.Context, _, requestID, choice string) error {
		decisions = append(decisions, requestID+"="+choice)
		return nil
	})
	approval("main/c999", "req-expire", "Allow Bash?", "Bash", "", promptChoices())

	platform.CleanupExpiredInteractive(-time.Hour) // every entry is older than a cutoff in the future

	if len(decisions) != 1 || decisions[0] != "req-expire=deny" {
		t.Errorf("backend decisions = %v, want [req-expire=deny] (expiry denies so the turn unblocks)", decisions)
	}
}

// TestAskgwPresent_RefusedConnFailsClosed proves the askgw entry point of
// the interim rule: an ask presented on a refusing connection posts nothing
// and reports PresentRefused, so the askgw server DENIES the ask (the
// requester receives the dismissal frame — no answer, nothing approved,
// nothing pending; askgw server_test.go pins the frame). A connection that
// merely has no session stays PresentFailed, and a capable connection
// presents normally.
func TestAskgwPresent_RefusedConnFailsClosed(t *testing.T) {
	t.Parallel()
	conn := &refuserConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c77"}}}
	logs := captureLog(t, log.WARN)

	id, outcome := askgwPresentFn(oneConnMgr{conn: conn})("main", "main/c77", "ask-req", "Run sudo?", "Sudo", "",
		[]question.Choice{{Label: "Yes", Data: "qa:0"}}, func(string) {})

	if outcome != askgw.PresentRefused || id != "" {
		t.Errorf("present = (%q, %v); want (\"\", PresentRefused) — denied, not merely failed", id, outcome)
	}
	if conn.buttonSend != 0 {
		t.Errorf("button sends = %d, want 0", conn.buttonSend)
	}
	if warns := logs.Lines("denied"); len(warns) != 1 {
		t.Errorf("denial WARNs = %d, want 1 (%v)", len(warns), warns)
	}

	// A connection with no session at all is a failure, not a refusal: the
	// askgw server resolves it unavailable (the honest status for "no
	// session"), so the two must stay distinguishable.
	if _, outcome := askgwPresentFn(stubConnMgr{})("main", "main/c404", "ask-404", "Run sudo?", "Sudo", "",
		[]question.Choice{{Label: "Yes", Data: "qa:0"}}, func(string) {}); outcome != askgw.PresentFailed {
		t.Errorf("present with no connection = %v, want PresentFailed (unavailable, not denied)", outcome)
	}

	// Positive control: a capable connection presents and reports its
	// platform message id.
	capable := telegramLikeConn()
	if id, outcome := askgwPresentFn(oneConnMgr{conn: capable})("main", "main/c999", "ask-ok", "Run sudo?", "Sudo", "",
		[]question.Choice{{Label: "Yes", Data: "qa:0"}}, func(string) {}); outcome != askgw.Presented || id == "" {
		t.Errorf("present on a capable conn = (%q, %v); want (id, Presented)", id, outcome)
	}
}

// twoPlatformConnMgr routes like an agent with a Telegram primary and a
// Slack connection: the Slack session resolves to a refusing connection,
// everything else to the Telegram one — the shape of requirement 6.8.
type twoPlatformConnMgr struct {
	stubConnMgr
	telegram platform.Connection
	slack    platform.Connection
	slackKey string
}

func (m twoPlatformConnMgr) Primary(string) platform.Connection { return m.telegram }
func (m twoPlatformConnMgr) ForSessionOrPrimary(sessionKey, _ string) platform.Connection {
	if sessionKey == m.slackKey {
		return m.slack
	}
	return m.telegram
}

// TestOtherPlatformApprovalsUnaffectedBySlackSession proves requirement 6.8:
// with a Slack session the agent's most recently active one, every approval
// shape raised by a NON-Slack session — a Telegram session's permission
// prompt, a chatless session's prompt, and an askgw ask — still reaches
// Telegram with its buttons, and the Slack connection records zero sends of
// any kind. (The Slack-side refusal itself is pinned by the H8 tests; this
// one is about the routing around it.) The prompt closures carry a respond
// recorder so a leak that fail-closes an approval surfaces as a recorded
// denial and failed assertions here, not as a panic on the bare test agent.
func TestOtherPlatformApprovalsUnaffectedBySlackSession(t *testing.T) {
	t.Parallel()
	telegram := telegramLikeConn() // default chat 999, records sends
	slackConn := &refuserConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: "main/c555"}}}
	slackKey := "main/c555"
	mgr := twoPlatformConnMgr{telegram: telegram, slack: slackConn, slackKey: slackKey}

	// A backend permission prompt from the telegram session. The respond
	// seam must stay silent: an approval that reached Telegram is waiting
	// for a human, not denied.
	var denials []string
	approval, _ := promptClosuresWithRespond(telegram, func(_ context.Context, _, requestID, choice string) error {
		denials = append(denials, requestID+"="+choice)
		return nil
	})
	approval("main/c222", "req-6-8-tg", "Allow Bash?", "Bash", "", promptChoices())
	if len(denials) != 0 {
		t.Errorf("backend denials = %v, want none (a Telegram approval must wait for a human, not be denied)", denials)
	}
	if sends, _, _, dSends, _, _ := telegram.counts(); sends+dSends != 1 {
		t.Errorf("telegram prompt sends = %d, want 1 (the prompt reaches telegram)", sends+dSends)
	}
	platform.CancelInteractiveMessage("req-6-8-tg", "done")

	// A prompt from a chatless session resolves to the primary — telegram.
	approval("main/iwork", "req-6-8-chatless", "Allow Bash?", "Bash", "", promptChoices())
	if len(denials) != 0 {
		t.Errorf("backend denials after chatless = %v, want none", denials)
	}
	if sends, _, _, dSends, _, _ := telegram.counts(); sends+dSends != 2 {
		t.Errorf("telegram prompt sends after chatless = %d, want 2", sends+dSends)
	}
	platform.CancelInteractiveMessage("req-6-8-chatless", "done")

	// An askgw ask resolves the same way.
	if _, outcome := askgwPresentFn(mgr)("main", "main/c222", "ask-6-8", "Q?", "Q", "",
		[]question.Choice{{Label: "Yes", Data: "qa:0"}}, func(string) {}); outcome != askgw.Presented {
		t.Error("askgw ask failed on a telegram connection; only refusing connections fail closed")
	}

	// The slack connection — the most recently active session's platform —
	// saw nothing of any of it.
	if slackConn.buttonSend != 0 || len(slackConn.notices) != 0 || slackConn.documents != 0 {
		t.Errorf("slack connection recorded sends: buttons=%d notices=%v docs=%d",
			slackConn.buttonSend, slackConn.notices, slackConn.documents)
	}
}

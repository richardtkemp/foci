package telegram

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// scopeWizard is a wizard double that records every Handle call so tests can
// prove which chat's text actually reached the wizard.
type scopeWizard struct {
	handles  []string
	response string
}

func (w *scopeWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	return w.response, false // never done — keeps the wizard active for further asserts
}

// makeMsgInChat builds a fresh Telegram message in an arbitrary chat (makeMsg
// hardcodes chat 12345; wizard scoping needs two distinct chats).
func makeMsgInChat(chatID, userID int64, username, text string) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: chatID},
		Text: text,
		Date: int64(time.Now().Unix()),
	}
}

// newWizardScopeBot builds a primary-bot testBot for wizard-scoping tests: two
// allowed users (111=A, 222=B), no override session key, a real session index
// (so the first message in chat 12345 sets the default chat), and the
// production dispatcher wiring (SetCommandContext → dispatchSessionKey). The
// registry's /wizstart command activates the given wizard under
// req.SessionKey — the key the real wizard commands (/secrets set, /config
// set, …) use.
func newWizardScopeBot(t *testing.T, w *scopeWizard) (*Bot, *mockClient, *command.Registry) {
	t.Helper()
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name:        "wizstart",
		Description: "activate the test wizard",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			cmds.SetWizard(req.SessionKey, w)
			return command.Response{Text: "wizard started"}, nil
		},
	})
	b, mock := testBot([]string{"111", "222"}, cmds)
	b.agentID = "test-agent"
	b.chatmeta.AgentID = "test-agent"
	b.sessionKey = "" // primary bot: no facet override key
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	b.SetSessionIndex(idx)
	b.SetCommandContext(command.CommandContext{})
	return b, mock, cmds
}

// startWizardInChat sends /wizstart from userID in chatID and simulates the
// command worker dispatching it, activating the wizard under that chat's key.
func startWizardInChat(t *testing.T, b *Bot, chatID, userID int64) {
	t.Helper()
	b.receiveMessage(context.Background(), makeMsgInChat(chatID, userID, "user", "/wizstart"))
	select {
	case cmd := <-b.mq.CmdChan():
		b.processQueuedCommand(context.Background(), cmd)
	default:
		t.Fatal("expected /wizstart to be routed to the command channel")
	}
}

// TestReceiveMessage_WizardInDefaultChatDoesNotConsumeOtherChat proves
// Problem case 1 end-to-end on the receive path: a wizard started by user A
// in the default chat never consumes user B's ordinary text in another chat —
// B's text continues to the agent queue and /repeat recording exactly as if
// no wizard existed, and A's wizard stays active and untouched.
func TestReceiveMessage_WizardInDefaultChatDoesNotConsumeOtherChat(t *testing.T) {
	w := &scopeWizard{response: "A's wizard step"}
	b, mock, cmds := newWizardScopeBot(t, w)

	// A starts the wizard in chat 12345; this first message also makes 12345
	// the default chat.
	startWizardInChat(t, b, 12345, 111)
	if !cmds.WizardActive("test-agent/c12345") {
		t.Fatal("wizard must be active under the default chat's key")
	}

	// B's ordinary text in chat 67890 must not feed A's wizard.
	b.receiveMessage(context.Background(), makeMsgInChat(67890, 222, "bob", "my secret value"))

	if len(b.mq.Chan()) != 1 {
		t.Fatalf("B's text must reach the agent queue, got %d queued", len(b.mq.Chan()))
	}
	if got := b.lastMsgStore.Get("222"); got != "my secret value" {
		t.Errorf("B's text must be recorded for /repeat, got %q", got)
	}
	if len(w.handles) != 0 {
		t.Errorf("A's wizard must not see B's text, handled: %v", w.handles)
	}
	if !cmds.WizardActive("test-agent/c12345") {
		t.Error("A's wizard must stay active")
	}
	if mock.sentCount() != 1 { // only the /wizstart ack; no wizard reply to B
		t.Errorf("sent %d messages, want 1 (no wizard reply into B's chat)", mock.sentCount())
	}
}

// TestReceiveMessage_WizardInOtherChatConsumesItsOwnText proves Problem
// case 2 end-to-end: a wizard started by user B in a non-default chat is fed
// by B's next message in that chat — consumed with the wizard's reply sent to
// B's chat, never queued to the agent or recorded for /repeat (where the
// secret value would leak into the transcript).
func TestReceiveMessage_WizardInOtherChatConsumesItsOwnText(t *testing.T) {
	w := &scopeWizard{response: "value received"}
	b, mock, cmds := newWizardScopeBot(t, w)

	// First message in chat 12345 sets the default chat for user A; B's chat
	// 67890 is deliberately NOT the default.
	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "hello"))
	if len(b.mq.Chan()) != 1 {
		t.Fatal("A's message should be queued")
	}
	<-b.mq.Chan() // drain

	// B starts the wizard in chat 67890.
	startWizardInChat(t, b, 67890, 222)
	if !cmds.WizardActive("test-agent/c67890") {
		t.Fatal("wizard must be active under B's chat key")
	}

	// B's value text must be consumed by B's own wizard.
	b.receiveMessage(context.Background(), makeMsgInChat(67890, 222, "bob", "the secret value"))

	if len(b.mq.Chan()) != 0 {
		t.Fatalf("B's value must not reach the agent queue, got %d queued", len(b.mq.Chan()))
	}
	if got := b.lastMsgStore.Get("222"); got != "" {
		t.Errorf("B's value must not be recorded for /repeat, got %q", got)
	}
	if mock.lastSendChatID != 67890 || mock.lastSendInjected != "value received" {
		t.Errorf("reply went to chat %d with %q, want chat 67890 with the wizard reply", mock.lastSendChatID, mock.lastSendInjected)
	}
	if len(w.handles) != 1 || w.handles[0] != "the secret value" {
		t.Errorf("wizard handled %v, want [the secret value]", w.handles)
	}
}

// TestReceiveMessage_FacetWizardStillFedByOwnChat characterises the facet
// (secondary-bot) path, which the fix must not change: a wizard started in a
// secondary bot's chat is stored under the bot's override key (what
// dispatchSessionKey returns) and that chat's text still advances it.
func TestReceiveMessage_FacetWizardStillFedByOwnChat(t *testing.T) {
	w := &scopeWizard{response: "facet wizard step"}
	b, mock, cmds := newWizardScopeBot(t, w)
	b.isSecondary = true
	override := "test-agent/c12345/b777"
	b.SetSessionKey(override)

	startWizardInChat(t, b, 12345, 111)
	if !cmds.WizardActive(override) {
		t.Fatalf("facet wizard must be stored under the override key %q", override)
	}

	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "facet input"))

	if len(b.mq.Chan()) != 0 {
		t.Fatalf("facet text must be consumed by the facet wizard, got %d queued", len(b.mq.Chan()))
	}
	if mock.lastSendChatID != 12345 || mock.lastSendInjected != "facet wizard step" {
		t.Errorf("reply went to chat %d with %q, want chat 12345 with the wizard reply", mock.lastSendChatID, mock.lastSendInjected)
	}
}

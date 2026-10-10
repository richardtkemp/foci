package discord

import (
	"context"
	"testing"
	"time"

	"foci/internal/command"
)

// scopeWizard is a wizard double that records every Handle call so tests can
// prove which channel's text actually reached the wizard.
type scopeWizard struct {
	handles  []string
	response string
}

func (w *scopeWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	return w.response, false // never done — keeps the wizard active for further asserts
}

// newWizardScopeBot builds a primary-bot test bot for wizard-scoping tests:
// two allowed users (u1=A, u2=B), a real session index (so the first message
// in channel 100 sets the default channel), and the production dispatcher
// wiring (SetCommandContext → dispatchSessionKey). The registry's /wizstart
// command activates the given wizard under req.SessionKey — the key the real
// wizard commands (/secrets set, /config set, …) use.
func newWizardScopeBot(t *testing.T, w *scopeWizard) (*Bot, *fakeSession) {
	t.Helper()
	b, fs, _ := newTestBot(t, "a")
	b.allowedUsers = map[string]bool{"u1": true, "u2": true}
	b.commands.Register(&command.Command{
		Name:        "wizstart",
		Description: "activate the test wizard",
		Execute: func(_ context.Context, req command.Request, _ command.CommandContext) (command.Response, error) {
			b.commands.SetWizard(req.SessionKey, w)
			return command.Response{Text: "wizard started"}, nil
		},
	})
	b.SetCommandContext(commandTestContext())
	return b, fs
}

// startWizardInChannel sends /wizstart from userID in channelID and simulates
// the command worker dispatching it, activating the wizard under that
// channel's key.
func startWizardInChannel(t *testing.T, b *Bot, channelID, userID string) {
	t.Helper()
	msg := testDiscordMessage(channelID, userID, "/wizstart")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)
	select {
	case cmd := <-b.mq.CmdChan():
		b.processQueuedCommand(context.Background(), cmd)
	default:
		t.Fatal("expected /wizstart to be routed to the command channel")
	}
}

// TestReceiveMessage_WizardInDefaultChannelDoesNotConsumeOtherChannel proves
// Problem case 1 end-to-end on the receive path: a wizard started by user A
// in the default channel never consumes user B's ordinary text in another
// channel — B's text continues to the agent queue and /repeat recording
// exactly as if no wizard existed, and A's wizard stays active and untouched.
func TestReceiveMessage_WizardInDefaultChannelDoesNotConsumeOtherChannel(t *testing.T) {
	w := &scopeWizard{response: "A's wizard step"}
	b, fs := newWizardScopeBot(t, w)

	// A starts the wizard in channel 100; this first message also makes 100
	// the default channel.
	startWizardInChannel(t, b, "100", "u1")
	if !b.commands.WizardActive("a/c100") {
		t.Fatal("wizard must be active under the default channel's key")
	}

	// B's ordinary text in channel 200 must not feed A's wizard.
	msg := testDiscordMessage("200", "u2", "my secret value")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 1 {
		t.Fatalf("B's text must reach the agent queue, got %d queued", len(b.mq.Chan()))
	}
	if got := b.lastMsgStore.Get("u2"); got != "my secret value" {
		t.Errorf("B's text must be recorded for /repeat, got %q", got)
	}
	if len(w.handles) != 0 {
		t.Errorf("A's wizard must not see B's text, handled: %v", w.handles)
	}
	if !b.commands.WizardActive("a/c100") {
		t.Error("A's wizard must stay active")
	}
	if fs.sendCount() != 1 { // only the /wizstart ack; no wizard reply into B's channel
		t.Errorf("sent %d messages, want 1 (no wizard reply into B's channel)", fs.sendCount())
	}
}

// TestReceiveMessage_WizardInOtherChannelConsumesItsOwnText proves Problem
// case 2 end-to-end: a wizard started by user B in a non-default channel is
// fed by B's next message in that channel — consumed with the wizard's reply
// sent to B's channel, never queued to the agent or recorded for /repeat
// (where the secret value would leak into the transcript).
func TestReceiveMessage_WizardInOtherChannelConsumesItsOwnText(t *testing.T) {
	w := &scopeWizard{response: "value received"}
	b, fs := newWizardScopeBot(t, w)

	// First message in channel 100 sets the default channel for user A; B's
	// channel 200 is deliberately NOT the default.
	msg := testDiscordMessage("100", "u1", "hello")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)
	if len(b.mq.Chan()) != 1 {
		t.Fatal("A's message should be queued")
	}
	<-b.mq.Chan() // drain

	// B starts the wizard in channel 200.
	startWizardInChannel(t, b, "200", "u2")
	if !b.commands.WizardActive("a/c200") {
		t.Fatal("wizard must be active under B's channel key")
	}

	// B's value text must be consumed by B's own wizard.
	msg = testDiscordMessage("200", "u2", "the secret value")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 0 {
		t.Fatalf("B's value must not reach the agent queue, got %d queued", len(b.mq.Chan()))
	}
	if got := b.lastMsgStore.Get("u2"); got != "" {
		t.Errorf("B's value must not be recorded for /repeat, got %q", got)
	}
	if got := fs.lastSend(t); got.channelID != "200" || got.content != "value received" {
		t.Errorf("reply went to channel %q with %q, want channel 200 with the wizard reply", got.channelID, got.content)
	}
	if len(w.handles) != 1 || w.handles[0] != "the secret value" {
		t.Errorf("wizard handled %v, want [the secret value]", w.handles)
	}
}

// TestReceiveMessage_FacetWizardStillFedByOwnChannel characterises the facet
// (secondary-bot) path, which the fix must not change: a wizard started in a
// secondary bot's channel is stored under the bot's override key (what
// dispatchSessionKey returns) and that channel's text still advances it.
func TestReceiveMessage_FacetWizardStillFedByOwnChannel(t *testing.T) {
	w := &scopeWizard{response: "facet wizard step"}
	b, fs := newWizardScopeBot(t, w)
	b.isSecondary = true
	override := "a/c100/b777"
	b.SetSessionKey(override)

	startWizardInChannel(t, b, "100", "u1")
	if !b.commands.WizardActive(override) {
		t.Fatalf("facet wizard must be stored under the override key %q", override)
	}

	msg := testDiscordMessage("100", "u1", "facet input")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	if len(b.mq.Chan()) != 0 {
		t.Fatalf("facet text must be consumed by the facet wizard, got %d queued", len(b.mq.Chan()))
	}
	if got := fs.lastSend(t); got.channelID != "100" || got.content != "facet wizard step" {
		t.Errorf("reply went to channel %q with %q, want channel 100 with the wizard reply", got.channelID, got.content)
	}
}

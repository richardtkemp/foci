package discord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/question"

	"github.com/bwmarrin/discordgo"
)

// buttonWizard is a wizard double modelling an android-like flow with two
// structured steps around one free-text step:
//
//	step 0: structured yes/no confirm — any answer but "no" advances
//	step 1: free text (host)
//	step 2: structured yes/no restart — any answer finishes
//
// PendingStep is structured on 0 and 2, nil on 1, so tests can drive both
// buttoned and plain replies.
type buttonWizard struct {
	handles []string
	step    int
}

func (w *buttonWizard) Handle(text string) (string, bool) {
	w.handles = append(w.handles, text)
	switch w.step {
	case 0:
		if strings.EqualFold(strings.TrimSpace(text), "no") {
			return "Cancelled.", true
		}
		w.step = 1
		return "Enter the host:", false
	case 1:
		w.step = 2
		return "Okay to restart foci now? (`yes`/`no`)", false
	default:
		return "All done.", true
	}
}

func (w *buttonWizard) PendingStep() *question.Question {
	switch w.step {
	case 0:
		return &question.Question{
			Header:   "Confirm",
			Question: "Proceed? (`yes`/`no`)",
			Options:  []question.Option{{Label: "yes"}, {Label: "no"}},
		}
	case 2:
		return &question.Question{
			Header:   "Restart",
			Question: "Okay to restart foci now? (`yes`/`no`)",
			Options:  []question.Option{{Label: "yes"}, {Label: "no"}},
		}
	}
	return nil
}

// pressData builds the wz: callback data for the wizard's CURRENT structured
// step: the step's token plus an option index, or "cancel". Fails the test if
// the wizard is not on a structured step.
func pressData(t *testing.T, w *buttonWizard, suffix string) string {
	t.Helper()
	q := w.PendingStep()
	if q == nil {
		t.Fatal("pressData: wizard is not on a structured step")
	}
	return "wz:" + command.WizardStepToken(q) + ":" + suffix
}

// TestTryIntercept_WizardStepReplyHasButtons proves a wizard reply for a
// structured step is sent WITH wz: buttons to the asking channel (not the
// default): one per option plus Cancel, carrying the step's token.
func TestTryIntercept_WizardStepReplyHasButtons(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)

	// First message in channel 100 sets the default; the wizard runs in 200.
	msg := testDiscordMessage("100", "u1", "hello")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)
	<-b.mq.Chan() // drain
	startWizardInChannel(t, b, "200", "u2")

	// "yes" answers the confirm step (free-text host prompt follows)…
	m := testDiscordMessage("200", "u2", "yes")
	m.Timestamp = time.Now()
	b.receiveMessage(context.Background(), m)
	// …and the host answer lands on the structured restart step: the reply
	// must go out with buttons.
	m = testDiscordMessage("200", "u2", "app.example.com")
	m.Timestamp = time.Now()
	b.receiveMessage(context.Background(), m)

	if !b.commands.WizardActive("a/c200") {
		t.Fatal("wizard must stay active")
	}
	got := fs.lastSend(t)
	if got.channelID != "200" {
		t.Fatalf("reply went to channel %q, want 200 (the asking channel, not the default)", got.channelID)
	}
	if !strings.Contains(got.content, "Okay to restart") {
		t.Fatalf("reply text = %q, want the restart prompt", got.content)
	}
	want := "wz:" + command.WizardStepToken(w.PendingStep())
	var customIDs []string
	for _, comp := range got.components {
		if row, ok := comp.(discordgo.ActionsRow); ok {
			for _, c := range row.Components {
				if btn, ok := c.(discordgo.Button); ok {
					customIDs = append(customIDs, btn.CustomID)
				}
			}
		}
	}
	joined := strings.Join(customIDs, ",")
	for _, wantID := range []string{want + ":0", want + ":1", want + ":cancel"} {
		if !strings.Contains(joined, wantID) {
			t.Errorf("buttons %v missing %q", customIDs, wantID)
		}
	}
}

// TestWizardStepReply_ButtonSendFallsBackToPlain proves that when the button
// send fails (e.g. Discord's 25-button limit on a long option list), the
// wizard's prompt is still delivered as plain text — the prompt is never
// lost.
func TestWizardStepReply_ButtonSendFallsBackToPlain(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")

	// Advance past the first structured step (its reply is plain text).
	m := testDiscordMessage("100", "u1", "yes")
	m.Timestamp = time.Now()
	b.receiveMessage(context.Background(), m)

	// The NEXT reply is buttoned: make the complex (component) send fail; the
	// plain fallback must then succeed.
	fs.mu.Lock()
	fs.complexSendErr = errors.New("50035 Invalid Form Body")
	fs.mu.Unlock()
	m = testDiscordMessage("100", "u1", "app.example.com")
	m.Timestamp = time.Now()
	b.receiveMessage(context.Background(), m)

	if !b.commands.WizardActive("a/c100") {
		t.Fatal("wizard must stay active")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sends) < 2 {
		t.Fatalf("sends = %d, want >= 2 (failed button send + plain fallback)", len(fs.sends))
	}
	last := fs.sends[len(fs.sends)-1]
	if len(last.components) != 0 {
		t.Errorf("fallback send still carries components (%d)", len(last.components))
	}
	if !strings.Contains(last.content, "Okay to restart") {
		t.Errorf("fallback text = %q, want the wizard's prompt", last.content)
	}
}

// TestWizardButtonPress_AdvancesAndStrips proves a valid wz: press answers
// the wizard: the pressed message's buttons are stripped (edit with empty
// components), the picked label reaches Handle, and the wizard's next prompt
// is sent as a new message.
func TestWizardButtonPress_AdvancesAndStrips(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")

	b.handleComponentInteraction(context.Background(), componentInteraction("100", "42", pressData(t, w, "0")))

	if len(w.handles) != 1 || w.handles[0] != "yes" {
		t.Fatalf("wizard handled %v, want [yes] (the pressed option's label)", w.handles)
	}
	if !b.commands.WizardActive("a/c100") {
		t.Fatal("wizard must stay active on its free-text step")
	}

	// The pressed message's buttons came off.
	strip := fs.lastEdit(t)
	if strip.msgID != "42" || strip.channelID != "100" {
		t.Fatalf("strip edit = (%q, %q), want message 42 in channel 100", strip.channelID, strip.msgID)
	}
	if len(strip.components) != 0 {
		t.Errorf("strip edit left %d components, want none", len(strip.components))
	}

	// The wizard's next prompt went out as a new message.
	got := fs.lastSend(t)
	if !strings.Contains(got.content, "Enter the host:") {
		t.Errorf("next prompt = %q, want the free-text host prompt", got.content)
	}
	if len(got.components) != 0 {
		t.Errorf("free-text next prompt carries %d components", len(got.components))
	}
}

// TestWizardButtonPress_CancelButtonCancels proves the Cancel button (payload
// "<token>:cancel") takes the wizard's cancel path and the pressed message's
// buttons are stripped.
func TestWizardButtonPress_CancelButtonCancels(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")

	b.handleComponentInteraction(context.Background(), componentInteraction("100", "42", pressData(t, w, "cancel")))

	if b.commands.WizardActive("a/c100") {
		t.Error("Cancel press must clear the wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; Cancel must not reach Handle", w.handles)
	}
	if got := fs.lastEdit(t); len(got.components) != 0 {
		t.Errorf("cancel press left %d components on the message", len(got.components))
	}
	if got := fs.lastSend(t); !strings.Contains(got.content, "Wizard cancelled.") {
		t.Errorf("reply = %q, want the cancel reply", got.content)
	}
}

// TestWizardButtonPress_StaleTokenDoesNotReachWizard proves a press whose
// token doesn't match the wizard's current step is answered with the stale
// notice and never reaches the wizard (or the agent queue).
func TestWizardButtonPress_StaleTokenDoesNotReachWizard(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")

	for _, data := range []string{
		"wz:0000000000000000:0", // well-formed but wrong token
		"wz:deadbeefdeadbeef",   // malformed payload
		"wz::0",                 // malformed token
	} {
		b.handleComponentInteraction(context.Background(), componentInteraction("100", "42", data))
	}

	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; stale presses must not reach it", w.handles)
	}
	if !b.commands.WizardActive("a/c100") {
		t.Error("stale press must leave the wizard active")
	}
	fs.mu.Lock()
	edits, sends := len(fs.edits), len(fs.sends)
	last := sends > 0 && strings.Contains(fs.sends[len(fs.sends)-1].content, "This step is no longer active.")
	fs.mu.Unlock()
	if edits != 3 {
		t.Errorf("edits = %d, want 3 (each stale press strips the buttons)", edits)
	}
	if !last {
		t.Error("last reply must be the stale notice")
	}
}

// TestWizardButtonPress_SecondPressRejected proves a double press of an
// already-answered step is rejected: the second press strips the buttons and
// gets the stale notice, and the wizard saw exactly one answer.
func TestWizardButtonPress_SecondPressRejected(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")
	data := pressData(t, w, "0")

	b.handleComponentInteraction(context.Background(), componentInteraction("100", "42", data))
	b.handleComponentInteraction(context.Background(), componentInteraction("100", "42", data))

	if len(w.handles) != 1 {
		t.Errorf("wizard handled %v, want exactly one answer", w.handles)
	}
	if !b.commands.WizardActive("a/c100") {
		t.Error("wizard must stay active on its free-text step")
	}
	if got := fs.lastSend(t); !strings.Contains(got.content, "This step is no longer active.") {
		t.Errorf("second press reply = %q, want the stale notice", got.content)
	}
}

// TestWizardButtonPress_OtherChannelScopeIsolated proves a press is resolved
// against the PRESSED channel's scope: a button pressed in channel B never
// advances channel A's wizard.
func TestWizardButtonPress_OtherChannelScopeIsolated(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1") // wizard lives under a/c100

	// The same button data, pressed in another channel.
	b.handleComponentInteraction(context.Background(), componentInteraction("200", "42", pressData(t, w, "0")))

	if len(w.handles) != 0 {
		t.Errorf("channel A's wizard handled %v; channel B's press must not reach it", w.handles)
	}
	if !b.commands.WizardActive("a/c100") {
		t.Error("channel A's wizard must stay active")
	}
	if got := fs.lastSend(t); got.channelID != "200" || !strings.Contains(got.content, "This step is no longer active.") {
		t.Errorf("reply = (%q, %q), want the stale notice in channel 200", got.channelID, got.content)
	}
}

// TestReceiveMessage_CancelCancelsActiveWizard proves a typed /cancel in a
// channel whose scope has an active wizard is routed to the wizard intercept
// (not the command channel): it replies "Wizard cancelled." and clears the
// wizard.
func TestReceiveMessage_CancelCancelsActiveWizard(t *testing.T) {
	w := &buttonWizard{}
	b, fs := newWizardScopeBot(t, w)
	startWizardInChannel(t, b, "100", "u1")

	msg := testDiscordMessage("100", "u1", "/CANCEL  ")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	select {
	case <-b.mq.CmdChan():
		t.Fatal("/cancel must be routed to the wizard, not the command channel")
	default:
	}
	if b.commands.WizardActive("a/c100") {
		t.Error("/cancel must clear the wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; /cancel must not reach Handle", w.handles)
	}
	if got := fs.lastSend(t); !strings.Contains(got.content, "Wizard cancelled.") {
		t.Errorf("reply = %q, want the cancel reply", got.content)
	}
}

// Characterises /cancel with NO active wizard: it keeps today's routing — the
// command channel, then the registry's unknown-command reply. Nothing changes
// for it.
func TestDiscordCancelWithoutWizardUnchanged(t *testing.T) {
	b, fs := newWizardScopeBot(t, &buttonWizard{})

	msg := testDiscordMessage("100", "u1", "/cancel")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)

	select {
	case cmd := <-b.mq.CmdChan():
		b.processQueuedCommand(context.Background(), cmd)
	default:
		t.Fatal("/cancel without a wizard must route to the command channel, as before")
	}
	if got := fs.lastSend(t); !strings.Contains(got.content, "Unknown command /cancel") {
		t.Errorf("reply = %q, want the registry's unknown-command reply", got.content)
	}
}

// Characterises free-text wizard replies: a wizard step without options is
// sent as plain text (no components), to the asking channel, exactly as
// before this change.
func TestDiscordWizardFreeTextStepPlain(t *testing.T) {
	w := &scopeWizard{response: "type the value"}
	b, fs := newWizardScopeBot(t, w)

	// First message in channel 100 sets the default; the wizard runs in 200.
	msg := testDiscordMessage("100", "u1", "hello")
	msg.Timestamp = time.Now()
	b.receiveMessage(context.Background(), msg)
	<-b.mq.Chan() // drain
	startWizardInChannel(t, b, "200", "u2")

	m := testDiscordMessage("200", "u2", "the value")
	m.Timestamp = time.Now()
	b.receiveMessage(context.Background(), m)

	if !b.commands.WizardActive("a/c200") {
		t.Fatal("wizard must stay active")
	}
	got := fs.lastSend(t)
	if got.channelID != "200" || !strings.Contains(got.content, "type the value") {
		t.Errorf("reply = (%q, %q), want the wizard reply in channel 200", got.channelID, got.content)
	}
	if len(got.components) != 0 {
		t.Errorf("free-text reply carries %d components; must stay plain", len(got.components))
	}
}

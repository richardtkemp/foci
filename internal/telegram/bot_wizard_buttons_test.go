package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/question"

	"github.com/PaulSonOfLars/gotgbot/v2"
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

// callbackDataFromMarkup flattens an inline keyboard's callback data in row
// order, for asserting which wz:/cmd: buttons a sent keyboard carries.
func callbackDataFromMarkup(kb gotgbot.InlineKeyboardMarkup) []string {
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			data = append(data, btn.CallbackData)
		}
	}
	return data
}

// assertWizardKeyboard fails the test unless kb carries one wz: button per
// option of the wizard's current structured step, plus Cancel — each carrying
// the step's token.
func assertWizardKeyboard(t *testing.T, kb gotgbot.InlineKeyboardMarkup, w *buttonWizard) {
	t.Helper()
	want := "wz:" + command.WizardStepToken(w.PendingStep())
	joined := strings.Join(callbackDataFromMarkup(kb), ",")
	for _, wantBtn := range []string{want + ":0", want + ":1", want + ":cancel"} {
		if !strings.Contains(joined, wantBtn) {
			t.Errorf("keyboard %v missing %q", joined, wantBtn)
		}
	}
}

// TestTryIntercept_WizardStepReplyHasButtons proves a wizard reply for a
// structured step is sent WITH wz: buttons to the asking chat (not the
// default): one per option plus Cancel, carrying the step's token.
func TestTryIntercept_WizardStepReplyHasButtons(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)

	// First message in 12345 sets the default chat; the wizard runs in 67890.
	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "hello"))
	<-b.mq.Chan() // drain
	startWizardInChat(t, b, 67890, 222)

	// "yes" answers the confirm step (free-text host prompt follows)…
	b.receiveMessage(context.Background(), makeMsgInChat(67890, 222, "bob", "yes"))
	// …and the host answer lands on the structured restart step: the reply
	// must go out with buttons.
	b.receiveMessage(context.Background(), makeMsgInChat(67890, 222, "bob", "app.example.com"))

	if !cmds.WizardActive("test-agent/c67890") {
		t.Fatal("wizard must stay active")
	}
	mock.mu.Lock()
	chatID, opts := mock.lastSendChatID, mock.lastSendOpts
	mock.mu.Unlock()
	if chatID != 67890 {
		t.Fatalf("reply went to chat %d, want 67890 (the asking chat, not the default)", chatID)
	}
	if opts == nil {
		t.Fatal("reply send had no opts")
	}
	kb, ok := opts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("reply sent without an inline keyboard (markup=%T)", opts.ReplyMarkup)
	}
	assertWizardKeyboard(t, kb, w)
}

// TestWizardStepReply_ButtonSendFallsBackToPlain proves that when the button
// send fails, the wizard's prompt is still delivered as plain text — the
// prompt is never lost.
func TestWizardStepReply_ButtonSendFallsBackToPlain(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)

	// Advance past the first structured step (its reply is plain text).
	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "yes"))

	// The NEXT reply is buttoned: make its button send fail once; the plain
	// fallback must then succeed.
	mock.mu.Lock()
	mock.sendErr = errors.New("send failed")
	mock.sendErrOnce = true
	mock.mu.Unlock()
	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "app.example.com"))

	if !cmds.WizardActive("test-agent/c12345") {
		t.Fatal("wizard must stay active")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.sends < 2 {
		t.Fatalf("sends = %d, want >= 2 (failed button send + plain fallback)", mock.sends)
	}
	if mock.lastSendOpts != nil && mock.lastSendOpts.ReplyMarkup != nil {
		t.Errorf("fallback send still carries markup (%T)", mock.lastSendOpts.ReplyMarkup)
	}
	if !strings.Contains(mock.lastSendInjected, "Okay to restart") {
		t.Errorf("fallback text = %q, want the wizard's prompt", mock.lastSendInjected)
	}
}

// TestCommandActivatedWizard_PromptHasButtons proves a typed command that
// ACTIVATES a wizard with a structured first step (as /config set and /android
// do) renders its response — the carrier of the first step's prompt — with wz:
// buttons, not as plain text.
func TestCommandActivatedWizard_PromptHasButtons(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)

	// /wizstart's dispatch activates the wizard (step 0 is structured), so
	// the command's own reply is the first buttoned prompt.
	startWizardInChat(t, b, 12345, 111)

	if !cmds.WizardActive("test-agent/c12345") {
		t.Fatal("wizard must be active after the command")
	}
	mock.mu.Lock()
	opts := mock.lastSendOpts
	mock.mu.Unlock()
	if opts == nil {
		t.Fatal("no send recorded for the command's reply")
	}
	kb, ok := opts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("command reply sent without an inline keyboard (markup=%T)", opts.ReplyMarkup)
	}
	assertWizardKeyboard(t, kb, w)
}

// TestCommandCallbackPress_ActivatesWizardWithButtons proves a cmd: button
// press whose command ACTIVATES a wizard with a structured first step (e.g. a
// /config chain-keyboard pick of "set" landing on /config set) strips the
// pressed message's keyboard and sends the wizard's prompt as a NEW buttoned
// message, instead of editing the pressed message into the prompt.
func TestCommandCallbackPress_ActivatesWizardWithButtons(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)

	b.handleCallbackQuery(context.Background(), makeCallbackQuery(55, "cmd:/wizstart"))

	if !cmds.WizardActive("test-agent/c12345") {
		t.Fatal("command press must activate the wizard")
	}
	if mock.markupEdits != 1 {
		t.Fatalf("markup edits = %d, want 1 (strip the pressed message's keyboard)", mock.markupEdits)
	}
	mock.mu.Lock()
	markupOpts, sendOpts := mock.lastMarkupOpts, mock.lastSendOpts
	mock.mu.Unlock()
	if markupOpts == nil || markupOpts.MessageId != 55 {
		t.Fatalf("markup edit opts = %+v, want the pressed message 55", markupOpts)
	}
	if len(markupOpts.ReplyMarkup.InlineKeyboard) != 0 {
		t.Errorf("markup edit left %d keyboard rows on the pressed message", len(markupOpts.ReplyMarkup.InlineKeyboard))
	}
	if sendOpts == nil {
		t.Fatal("the wizard's prompt was not sent as a new message")
	}
	kb, ok := sendOpts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("prompt sent without an inline keyboard (markup=%T)", sendOpts.ReplyMarkup)
	}
	assertWizardKeyboard(t, kb, w)
}

// TestWizardButtonPress_AdvancesAndStrips proves a valid wz: press answers the
// wizard: the pressed message's keyboard is stripped (empty markup edit), the
// picked label reaches Handle, and the wizard's next prompt is sent (plain for
// a free-text step).
func TestWizardButtonPress_AdvancesAndStrips(t *testing.T) {
	w := &buttonWizard{}
	b, mock, _ := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)

	sendsBefore := mock.sentCount()
	b.handleCallbackQuery(context.Background(), makeCallbackQuery(42, pressData(t, w, "0")))

	if len(w.handles) != 1 || w.handles[0] != "yes" {
		t.Fatalf("wizard handled %v, want [yes] (the pressed option's label)", w.handles)
	}
	if mock.markupEdits != 1 {
		t.Fatalf("markup edits = %d, want 1 (strip the pressed message's buttons)", mock.markupEdits)
	}
	mock.mu.Lock()
	markupOpts := mock.lastMarkupOpts
	mock.mu.Unlock()
	if markupOpts == nil || markupOpts.MessageId != 42 {
		t.Fatalf("markup edit opts = %+v, want the pressed message 42", markupOpts)
	}
	if len(markupOpts.ReplyMarkup.InlineKeyboard) != 0 {
		t.Errorf("markup edit left %d keyboard rows on the message", len(markupOpts.ReplyMarkup.InlineKeyboard))
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.sends != sendsBefore+1 {
		t.Fatalf("sends = %d, want %d (the next prompt as a new message)", mock.sends, sendsBefore+1)
	}
	if !strings.Contains(mock.lastSendInjected, "Enter the host:") {
		t.Errorf("next prompt = %q, want the free-text host prompt", mock.lastSendInjected)
	}
	if mock.lastSendOpts != nil && mock.lastSendOpts.ReplyMarkup != nil {
		t.Errorf("free-text next prompt carries markup (%T)", mock.lastSendOpts.ReplyMarkup)
	}
	if mock.answerCBCalls != 1 {
		t.Errorf("callback acks = %d, want 1", mock.answerCBCalls)
	}
}

// TestWizardButtonPress_CancelButtonCancels proves the Cancel button (payload
// "<token>:cancel") takes the wizard's cancel path and the pressed message's
// buttons are stripped.
func TestWizardButtonPress_CancelButtonCancels(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)

	b.handleCallbackQuery(context.Background(), makeCallbackQuery(42, pressData(t, w, "cancel")))

	if cmds.WizardActive("test-agent/c12345") {
		t.Error("Cancel press must clear the wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; Cancel must not reach Handle", w.handles)
	}
	if mock.markupEdits != 1 {
		t.Errorf("markup edits = %d, want 1", mock.markupEdits)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "Wizard cancelled.") {
		t.Errorf("reply = %q, want the cancel reply", mock.lastSendInjected)
	}
}

// TestWizardButtonPress_StaleTokenDoesNotReachWizard proves a press whose
// token doesn't match the wizard's current step is answered with the stale
// notice and never reaches the wizard (or the agent queue).
func TestWizardButtonPress_StaleTokenDoesNotReachWizard(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)
	sendsBefore := mock.sentCount()

	for _, data := range []string{
		"wz:0000000000000000:0", // well-formed but wrong token
		"wz:deadbeefdeadbeef",   // malformed payload
		"wz::0",                 // malformed token
	} {
		b.handleCallbackQuery(context.Background(), makeCallbackQuery(42, data))
	}

	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; stale presses must not reach it", w.handles)
	}
	if !cmds.WizardActive("test-agent/c12345") {
		t.Error("stale press must leave the wizard active")
	}
	if mock.markupEdits != 3 {
		t.Errorf("markup edits = %d, want 3 (each stale press strips the buttons)", mock.markupEdits)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "This step is no longer active.") {
		t.Errorf("reply = %q, want the stale notice", mock.lastSendInjected)
	}
	if mock.sends != sendsBefore+3 {
		t.Errorf("sends = %d, want %d (one stale notice per press)", mock.sends, sendsBefore+3)
	}
}

// TestWizardButtonPress_SecondPressRejected proves a double press of an
// already-answered step is rejected: the second press strips the buttons and
// gets the stale notice, and the wizard saw exactly one answer.
func TestWizardButtonPress_SecondPressRejected(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)
	data := pressData(t, w, "0")

	b.handleCallbackQuery(context.Background(), makeCallbackQuery(42, data))
	b.handleCallbackQuery(context.Background(), makeCallbackQuery(42, data))

	if len(w.handles) != 1 {
		t.Errorf("wizard handled %v, want exactly one answer", w.handles)
	}
	if !cmds.WizardActive("test-agent/c12345") {
		t.Error("wizard must stay active on its free-text step")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "This step is no longer active.") {
		t.Errorf("second press reply = %q, want the stale notice", mock.lastSendInjected)
	}
}

// TestWizardButtonPress_OtherChatScopeIsolated proves a press is resolved
// against the PRESSED chat's scope: a button pressed in chat B never advances
// chat A's wizard.
func TestWizardButtonPress_OtherChatScopeIsolated(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111) // wizard lives under test-agent/c12345

	// The same button data, pressed in another chat.
	b.handleCallbackQuery(context.Background(), makeCallbackQueryInChat(67890, 42, pressData(t, w, "0")))

	if len(w.handles) != 0 {
		t.Errorf("chat A's wizard handled %v; chat B's press must not reach it", w.handles)
	}
	if !cmds.WizardActive("test-agent/c12345") {
		t.Error("chat A's wizard must stay active")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "This step is no longer active.") {
		t.Errorf("reply = %q, want the stale notice in chat B", mock.lastSendInjected)
	}
	if mock.lastSendChatID != 67890 {
		t.Errorf("stale notice went to chat %d, want 67890", mock.lastSendChatID)
	}
}

// TestReceiveMessage_CancelCancelsActiveWizard proves a typed /cancel in a
// chat whose scope has an active wizard is routed to the wizard intercept
// (not the command channel): it replies "Wizard cancelled." and clears the
// wizard.
func TestReceiveMessage_CancelCancelsActiveWizard(t *testing.T) {
	w := &buttonWizard{}
	b, mock, cmds := newWizardScopeBot(t, w)
	startWizardInChat(t, b, 12345, 111)

	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "/CANCEL  "))

	select {
	case <-b.mq.CmdChan():
		t.Fatal("/cancel must be routed to the wizard, not the command channel")
	default:
	}
	if cmds.WizardActive("test-agent/c12345") {
		t.Error("/cancel must clear the wizard")
	}
	if len(w.handles) != 0 {
		t.Errorf("wizard handled %v; /cancel must not reach Handle", w.handles)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "Wizard cancelled.") {
		t.Errorf("reply = %q, want the cancel reply", mock.lastSendInjected)
	}
}

// Characterises /cancel with NO active wizard: it keeps today's routing — the
// command channel, then the registry's unknown-command reply. Nothing changes
// for it.
func TestTelegramCancelWithoutWizardUnchanged(t *testing.T) {
	b, mock, _ := newWizardScopeBot(t, &buttonWizard{})

	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "/cancel"))

	select {
	case cmd := <-b.mq.CmdChan():
		b.processQueuedCommand(context.Background(), cmd)
	default:
		t.Fatal("/cancel without a wizard must route to the command channel, as before")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !strings.Contains(mock.lastSendInjected, "Unknown command /cancel") {
		t.Errorf("reply = %q, want the registry's unknown-command reply", mock.lastSendInjected)
	}
}

// Characterises free-text wizard replies: a wizard step without options is
// sent as plain text (no keyboard), to the asking chat, exactly as before
// this change.
func TestTelegramWizardFreeTextStepPlain(t *testing.T) {
	w := &scopeWizard{response: "type the value"}
	b, mock, cmds := newWizardScopeBot(t, w)

	// First message in 12345 sets the default chat; the wizard runs in 67890.
	b.receiveMessage(context.Background(), makeMsgInChat(12345, 111, "alice", "hello"))
	<-b.mq.Chan() // drain
	startWizardInChat(t, b, 67890, 222)

	b.receiveMessage(context.Background(), makeMsgInChat(67890, 222, "bob", "the value"))

	if !cmds.WizardActive("test-agent/c67890") {
		t.Fatal("wizard must stay active")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.lastSendChatID != 67890 || !strings.Contains(mock.lastSendInjected, "type the value") {
		t.Errorf("reply = (%d, %q), want the wizard reply in chat 67890", mock.lastSendChatID, mock.lastSendInjected)
	}
	if mock.lastSendOpts != nil && mock.lastSendOpts.ReplyMarkup != nil {
		t.Errorf("free-text reply carries markup (%T); must stay plain", mock.lastSendOpts.ReplyMarkup)
	}
}

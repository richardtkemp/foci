package suggestion

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"foci/internal/platform"
	"foci/internal/session"
)

// --- fakes -------------------------------------------------------------

// fakeButtonConn is a connection that renders buttons: it records every
// button send (text, labels, per-button data — which carries the
// interactive-message id) and every edit. Unimplemented Connection methods
// panic via the embedded nil interface, so a test only reaches what the
// suggestion path actually calls.
type fakeButtonConn struct {
	platform.Connection // embedded nil: satisfies the rest of the surface

	mu     sync.Mutex
	sends  []buttonSend
	edits  []msgEdit
	nextID int
}

type buttonSend struct {
	msgID  string
	text   string
	labels []string
	datas  []string // per-button Data, in order
}

type msgEdit struct {
	msgID string
	text  string
}

func (c *fakeButtonConn) SendTextWithButtons(text string, buttons []platform.ButtonChoice, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	send := buttonSend{
		msgID:  "msg-" + strconv.Itoa(c.nextID),
		text:   text,
		labels: make([]string, len(buttons)),
		datas:  make([]string, len(buttons)),
	}
	for i, b := range buttons {
		send.labels[i] = b.Label
		send.datas[i] = b.Data
	}
	c.sends = append(c.sends, send)
	return send.msgID, nil
}

func (c *fakeButtonConn) EditMessageText(msgID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, msgEdit{msgID: msgID, text: text})
	return nil
}

func (c *fakeButtonConn) EditMessageWithButtons(msgID, text string, _ []platform.ButtonChoice, _ string) error {
	return c.EditMessageText(msgID, text)
}

func (c *fakeButtonConn) sendCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sends)
}

func (c *fakeButtonConn) sendAt(i int) buttonSend {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sends[i]
	out.labels = append([]string(nil), c.sends[i].labels...)
	out.datas = append([]string(nil), c.sends[i].datas...)
	return out
}

func (c *fakeButtonConn) editsSnapshot() []msgEdit {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]msgEdit, len(c.edits))
	copy(out, c.edits)
	return out
}

// fakeTextConn is a connection WITHOUT the ButtonSender surface: the suggestion
// must degrade to the numbered plain-text fallback.
type fakeTextConn struct {
	platform.Connection

	mu    sync.Mutex
	texts []string
}

func (c *fakeTextConn) SendText(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return nil
}

func (c *fakeTextConn) textsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.texts))
	copy(out, c.texts)
	return out
}

// fakeDispatch records each dispatched (sessionKey, command) and answers with
// the configured result; allowRun=false answers ok=false for every command.
type fakeDispatch struct {
	mu       sync.Mutex
	calls    [][2]string
	text     string
	ok       bool
	allowRun bool
}

func (d *fakeDispatch) dispatch(_ context.Context, sessionKey, commandLine string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, [2]string{sessionKey, commandLine})
	if !d.allowRun {
		return "", false
	}
	return d.text, d.ok
}

func (d *fakeDispatch) callsSnapshot() [][2]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][2]string, len(d.calls))
	copy(out, d.calls)
	return out
}

// senderFixture wires a Sender against fakes: a registry with one registered
// spec (validSpec, two actions), a real SessionIndex on a temp file, a
// swappable live connection, and a fake dispatcher. connLive=false makes the
// resolver return nil (no connection).
type senderFixture struct {
	reg      *Registry
	idx      *session.SessionIndex
	conn     *fakeButtonConn
	textConn *fakeTextConn // set to run the fixture against a button-less conn
	disp     *fakeDispatch
	sender   *Sender
	chatKey  string
	connLive bool
}

func newSenderFixture(t *testing.T) *senderFixture {
	t.Helper()
	f := &senderFixture{
		reg:      NewRegistry(),
		idx:      tempMuteIndex(t),
		conn:     &fakeButtonConn{},
		disp:     &fakeDispatch{allowRun: true, ok: true},
		connLive: true,
	}
	f.chatKey = session.NewChatSessionKey("clutch", 42)
	if err := f.reg.Register(validSpec()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sender, err := NewSender(SenderDeps{
		Registry: f.reg,
		Index:    f.idx,
		ConnFor: func(agentID, sessionKey string) platform.ConnResolver {
			return func() platform.Connection {
				if !f.connLive {
					return nil
				}
				if f.textConn != nil {
					return f.textConn
				}
				return f.conn
			}
		},
		Dispatch: f.disp.dispatch,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	f.sender = sender
	return f
}

// pressButton drives one press of send N's button buttonIdx through the REAL
// router, on the recorded button data ("<interactiveID>:<idx>"), applying the
// returned edit exactly as the platform layers do (telegram bot_callbacks.go,
// app dispatch.go). Returns whether the router knew the prompt.
func pressButton(t *testing.T, conn *fakeButtonConn, sendIdx, buttonIdx int) bool {
	t.Helper()
	send := conn.sendAt(sendIdx)
	edit, _, ok := platform.HandleInteractiveCallback(send.datas[buttonIdx])
	if ok && edit != "" {
		if err := conn.EditMessageText(send.msgID, edit); err != nil {
			t.Fatalf("edit after press: %v", err)
		}
	}
	return ok
}

// interactiveIDOf extracts the interactive-message id from a recorded button
// data string ("<id>:<idx>").
func interactiveIDOf(t *testing.T, data string) string {
	t.Helper()
	id, _, found := strings.Cut(data, ":")
	if !found || id == "" {
		t.Fatalf("button data %q has no <id>:<index> shape", data)
	}
	return id
}

// --- Send ---------------------------------------------------------------

// TestNewSenderRequiresEveryDep proves NewSender's construction contract:
// every dep is required, and a nil one is rejected at construction — the
// wiring is gateway startup code, so a missing collaborator must fail there
// rather than surface as a nil dereference inside a platform button-press
// callback (Send would panic on a nil Registry/Index/ConnFor, a press on a
// nil Dispatch).
func TestNewSenderRequiresEveryDep(t *testing.T) {
	base := SenderDeps{
		Registry: NewRegistry(),
		Index:    tempMuteIndex(t),
		ConnFor: func(_, _ string) platform.ConnResolver {
			return func() platform.Connection { return nil }
		},
		Dispatch: func(context.Context, string, string) (string, bool) { return "", false },
	}
	for _, dep := range []struct {
		name string
		zap  func(*SenderDeps)
	}{
		{"Registry", func(d *SenderDeps) { d.Registry = nil }},
		{"Index", func(d *SenderDeps) { d.Index = nil }},
		{"ConnFor", func(d *SenderDeps) { d.ConnFor = nil }},
		{"Dispatch", func(d *SenderDeps) { d.Dispatch = nil }},
	} {
		deps := base
		dep.zap(&deps)
		s, err := NewSender(deps)
		if err == nil {
			t.Errorf("NewSender without %s = nil error, want rejection", dep.name)
		}
		if s != nil {
			t.Errorf("NewSender without %s returned a Sender (%v), want nil", dep.name, s)
		}
	}
	if s, err := NewSender(base); err != nil || s == nil {
		t.Errorf("NewSender with every dep = (%v, %v), want a Sender and no error", s, err)
	}
}

// TestSendPostsSpecTextAndButtons proves the posted message: exactly one
// interactive send carrying the spec's text, one button per action in order,
// then the two built-ins — and each button's data is ONLY its index, so no
// command ever travels in the press data.
func TestSendPostsSpecTextAndButtons(t *testing.T) {
	f := newSenderFixture(t)

	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res != ResultSent {
		t.Errorf("Send = %q, want %q", res, ResultSent)
	}
	if got := f.conn.sendCount(); got != 1 {
		t.Fatalf("button sends = %d, want 1", got)
	}
	send := f.conn.sendAt(0)
	if send.text != validSpec().Text {
		t.Errorf("posted text = %q, want spec text %q", send.text, validSpec().Text)
	}
	wantLabels := []string{"Yes", "Not yet", NotNowLabel, MuteLabel}
	if len(send.labels) != len(wantLabels) {
		t.Fatalf("buttons = %v, want %v", send.labels, wantLabels)
	}
	for i, want := range wantLabels {
		if send.labels[i] != want {
			t.Errorf("button %d label = %q, want %q", i, send.labels[i], want)
		}
	}
	for i, data := range send.datas {
		id := interactiveIDOf(t, data)
		if !strings.HasPrefix(id, "suggestion-"+validSpec().ID+"-") {
			t.Errorf("interactive id %q lacks the suggestion-<suggestionID>- prefix", id)
		}
		if idx := data[len(id)+1:]; idx != strconv.Itoa(i) {
			t.Errorf("button %d carries press data %q, want index %d only", i, idx, i)
		}
		if strings.Contains(data, "/sr") {
			t.Errorf("button data leaks a command: %q", data)
		}
	}
}

// TestSendUniqueInteractiveIDPerSend proves each Send gets its own
// interactive-message id: two sends produce two distinct ids, so the second
// never overwrites the first's callback registration.
func TestSendUniqueInteractiveIDPerSend(t *testing.T) {
	f := newSenderFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	first := interactiveIDOf(t, f.conn.sendAt(0).datas[0])
	second := interactiveIDOf(t, f.conn.sendAt(1).datas[0])
	if first == second {
		t.Errorf("two sends share interactive id %q", first)
	}
}

// TestSendUnknownSuggestionIDErrors: an unregistered id is an error, not a send.
func TestSendUnknownSuggestionIDErrors(t *testing.T) {
	f := newSenderFixture(t)
	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, "nope.suggestion"); err == nil {
		t.Error("Send(unknown id) = nil error, want error")
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages for an unknown id, want 0", got)
	}
}

// TestSendNoConnectionReturnsNoConnection: nothing resolvable to send
// through is reported as ResultNoConnection, not an error.
func TestSendNoConnectionReturnsNoConnection(t *testing.T) {
	f := newSenderFixture(t)
	f.connLive = false
	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res != ResultNoConnection {
		t.Errorf("Send = %q, want %q", res, ResultNoConnection)
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages with no connection, want 0", got)
	}
}

// TestSendNonChatSessionKeyErrors: the per-chat mute unit does not exist for
// a non-chat session, so Send refuses rather than keying a mute at chat 0.
// The independent key's id is deliberately NUMERIC ("i42"): a named id
// (like "inosend") would also be rejected later by the chat-id parse, but
// "42" parses — so ONLY the chat-type guard rejects this key. Without that
// guard, an independent session whose id parses would post into, and mute,
// that chat's bucket.
func TestSendNonChatSessionKeyErrors(t *testing.T) {
	f := newSenderFixture(t)
	numKey, err := session.NamedIndependentSessionKey("clutch", "42")
	if err != nil {
		t.Fatalf("NamedIndependentSessionKey: %v", err)
	}
	if _, err := f.sender.Send(context.Background(), "clutch", numKey, validSpec().ID); err == nil {
		t.Error("Send(numeric independent session) = nil error, want error")
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages for a non-chat session, want 0", got)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 42, validSpec().ID)
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("an independent session keyed a mute in chat 42's bucket")
	}
}

// TestSendChildSessionKeyErrors: a child key (branch or spawn off a chat)
// still names the chat, but it is a transient work session — a suggestion's
// buttons must neither post there nor run their commands there. Send
// accepts only the chat's ROOT session key.
func TestSendChildSessionKeyErrors(t *testing.T) {
	f := newSenderFixture(t)
	root := session.NewChatSession("clutch", 42)
	children := map[string]string{
		"branch": root.Branch().String(),
		"spawn":  session.SessionKey{AgentID: "clutch", Type: 'c', ID: "42", ChildType: 'i', ChildTS: 1700000000}.String(),
	}
	for name, key := range children {
		if _, err := f.sender.Send(context.Background(), "clutch", key, validSpec().ID); err == nil {
			t.Errorf("Send(%s key %q) = nil error, want error", name, key)
		}
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages for child session keys, want 0", got)
	}
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("child-key Send dispatched commands: %v", calls)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 42, validSpec().ID)
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("a child session key keyed a mute in chat 42's bucket")
	}
}

// TestSendUnusableChatKeyErrors: a chat key whose id does not parse (or is
// 0) must fail closed. SessionKey.ChatID silently maps every such key to
// chat 0, so accepting one would key the mute on the bucket ALL malformed
// keys share (review round 4, finding 1: cabc, cxyz and c0 muted each
// other). A negative id is a real chat (Telegram groups) and still sends.
func TestSendUnusableChatKeyErrors(t *testing.T) {
	f := newSenderFixture(t)

	for _, key := range []string{"clutch/cabc", "clutch/c0"} {
		if _, err := f.sender.Send(context.Background(), "clutch", key, validSpec().ID); err == nil {
			t.Errorf("Send(%q) = nil error, want error", key)
		}
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages for unusable chat keys, want 0", got)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 0, validSpec().ID)
	if err != nil {
		t.Fatalf("muted(chat 0): %v", err)
	}
	if muted {
		t.Error("an unusable chat key keyed a mute in the shared 0 bucket")
	}

	// A negative chat id is a real chat (Telegram groups): it still sends.
	negKey := session.NewChatSessionKey("clutch", -77)
	if res, err := f.sender.Send(context.Background(), "clutch", negKey, validSpec().ID); err != nil || res != ResultSent {
		t.Errorf("Send(negative chat id) = %q, %v; want %q, nil", res, err, ResultSent)
	}
}

// TestSendAgentKeyMismatchErrors: the mute is keyed by (agent, chatID), so
// a Send whose agentID disagrees with the session key's agent must fail
// closed — accepting it would store the mute under the wrong agent's bucket
// and the chat would keep seeing the suggestion (review round 4, finding 2).
func TestSendAgentKeyMismatchErrors(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "other", f.chatKey, validSpec().ID); err == nil {
		t.Error("Send(agentID=other, clutch's key) = nil error, want error")
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages under a mismatched agentID, want 0", got)
	}
	for _, agent := range []string{"clutch", "other"} {
		muted, err := newMuteStore(f.idx).muted(agent, 42, validSpec().ID)
		if err != nil {
			t.Fatalf("muted(%s, 42): %v", agent, err)
		}
		if muted {
			t.Errorf("mismatched Send keyed a mute under agent %q", agent)
		}
	}
}

// TestSendTextOnlyConnectionGetsNumberedFallback proves the degradation on a
// connection without ButtonSender: one numbered plain-text message (the
// built-ins included), no buttons, and no execution path — the fallback
// registers no callback, so nothing can dispatch a command.
func TestSendTextOnlyConnectionGetsNumberedFallback(t *testing.T) {
	f := newSenderFixture(t)
	f.textConn = &fakeTextConn{}

	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res != ResultSent {
		t.Errorf("Send = %q, want %q", res, ResultSent)
	}
	texts := f.textConn.textsSnapshot()
	if len(texts) != 1 {
		t.Fatalf("text sends = %d, want 1", len(texts))
	}
	for _, want := range []string{
		validSpec().Text,
		"1. Yes",
		"2. Not yet",
		"3. " + NotNowLabel,
		"4. " + MuteLabel,
	} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("fallback text missing %q:\n%s", want, texts[0])
		}
	}
	// No callback was registered, so no typed reply can run a command: the
	// dispatcher stays silent and the router knows no suggestion prompt.
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("dispatcher ran with a text-only connection: %v", calls)
	}
	if edit, _, ok := platform.HandleInteractiveCallback("suggestion-" + validSpec().ID + "-forged:0"); ok || edit != "" {
		t.Error("a suggestion press routed without a ButtonSender connection")
	}
}

// --- presses ------------------------------------------------------------

// TestPressRunsSpecCommandOnSessionKey proves the press path: pressing action
// i dispatches exactly Actions[i].Command — the command from the REGISTERED
// SPEC, not from the press data — through the dispatcher on the suggestion's
// session key, and the message is edited with the command's result.
func TestPressRunsSpecCommandOnSessionKey(t *testing.T) {
	f := newSenderFixture(t)
	f.disp.text = "Spaced repetition is on."

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !pressButton(t, f.conn, 0, 1) { // action 1 = "Not yet"
		t.Fatal("router did not know the pressed prompt")
	}

	calls := f.disp.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatch calls = %v, want exactly one", calls)
	}
	if calls[0][0] != f.chatKey {
		t.Errorf("dispatch session key = %q, want the suggestion's %q", calls[0][0], f.chatKey)
	}
	if calls[0][1] != "/sr snooze" {
		t.Errorf("dispatched %q, want the spec's action 1 command %q", calls[0][1], "/sr snooze")
	}

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %v, want exactly one", edits)
	}
	if edits[0].text != "Spaced repetition is on." {
		t.Errorf("edit text = %q, want the command result", edits[0].text)
	}
	if edits[0].msgID != "msg-1" {
		t.Errorf("edit msgID = %q, want the suggestion message msg-1", edits[0].msgID)
	}
}

// TestPressEmptyResultEditsDone: a command that answers nothing still closes
// the suggestion with "Done".
func TestPressEmptyResultEditsDone(t *testing.T) {
	f := newSenderFixture(t)
	f.disp.text = ""

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 0)

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 || edits[0].text != doneText {
		t.Errorf("edits = %v, want one edit %q", edits, doneText)
	}
}

// TestPressNotExecutableCommandEditsUnavailable: ok=false must not claim
// success — the suggestion edits to the honest unavailable text.
func TestPressNotExecutableCommandEditsUnavailable(t *testing.T) {
	f := newSenderFixture(t)
	f.disp.ok = false

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 0)

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 || edits[0].text != commandUnavailable {
		t.Errorf("edits = %v, want one edit %q", edits, commandUnavailable)
	}
}

// TestPressNotNowEditsOKWithoutSideEffects: the Not now built-in only closes
// the message — no dispatch, no mute, nothing stored.
func TestPressNotNowEditsOKWithoutSideEffects(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 2) // built-in 1: Not now

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 || edits[0].text != notNowText {
		t.Errorf("edits = %v, want one edit %q", edits, notNowText)
	}
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("Not now dispatched commands: %v", calls)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 42, validSpec().ID)
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("Not now muted the suggestion")
	}
}

// TestPressOutOfRangeRunsNothing proves index discipline at both layers: the
// router rejects an out-of-range press outright, and even a forged index
// reaching handlePress directly runs no command and stores no mute.
func TestPressOutOfRangeRunsNothing(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Router level: an out-of-range index is unknown to the router.
	id := interactiveIDOf(t, f.conn.sendAt(0).datas[0])
	if _, _, ok := platform.HandleInteractiveCallback(id + ":99"); ok {
		t.Error("router accepted an out-of-range press")
	}

	// Forged level: handlePress itself runs nothing for a bad index.
	spec, _ := f.reg.Get(validSpec().ID)
	if edit := f.sender.handlePress(spec, "clutch", f.chatKey, 42, 99); edit != "" {
		t.Errorf("handlePress(99) = %q, want empty", edit)
	}
	if edit := f.sender.handlePress(spec, "clutch", f.chatKey, 42, -1); edit != "" {
		t.Errorf("handlePress(-1) = %q, want empty", edit)
	}

	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("out-of-range press dispatched: %v", calls)
	}
	if edits := f.conn.editsSnapshot(); len(edits) != 0 {
		t.Errorf("out-of-range press edited: %v", edits)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 42, validSpec().ID)
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if muted {
		t.Error("out-of-range press muted the suggestion")
	}
}

// --- the mute built-in ----------------------------------------------------

// TestPressStopShowingMutes proves the mute built-in: the press stores the
// suggestion id for the chat and edits the message to the mute confirmation —
// which the sender may only claim because the mute lands under exactly the
// key Send reads (proved by TestSendReturnsMutedAfterMutePress).
func TestPressStopShowingMutes(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 3) // built-in 2: Stop showing me this

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 || edits[0].text != mutedText {
		t.Errorf("edits = %v, want one edit %q", edits, mutedText)
	}
	muted, err := newMuteStore(f.idx).muted("clutch", 42, validSpec().ID)
	if err != nil {
		t.Fatalf("muted: %v", err)
	}
	if !muted {
		t.Error("mute press did not store the suggestion id")
	}
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("mute press dispatched commands: %v", calls)
	}
}

// TestSendReturnsMutedAfterMutePress proves the round trip the confirmation
// text promises: after the mute press, a later Send of that id returns Muted
// and posts nothing, while a different suggestion id still sends.
func TestSendReturnsMutedAfterMutePress(t *testing.T) {
	f := newSenderFixture(t)
	if err := f.reg.Register(Spec{ID: "other.suggestion", Text: "Another thing"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 3) // Stop showing me this

	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send after mute: %v", err)
	}
	if res != ResultMuted {
		t.Errorf("Send after mute = %q, want %q", res, ResultMuted)
	}
	if got := f.conn.sendCount(); got != 1 {
		t.Errorf("posted %d messages total, want 1 (nothing after the mute)", got)
	}

	// Another suggestion id is unaffected: it still sends.
	res, err = f.sender.Send(context.Background(), "clutch", f.chatKey, "other.suggestion")
	if err != nil {
		t.Fatalf("Send other suggestion: %v", err)
	}
	if res != ResultSent || f.conn.sendCount() != 2 {
		t.Errorf("other suggestion: res=%q sends=%d, want sent and a second post", res, f.conn.sendCount())
	}
}

// TestMuteSurvivesPlatformRowAppearingLaterThroughSend is the F1 probe, end
// to end: mute pressed BEFORE the chat has a platform row, the platform row
// then created, Send again — nothing is posted, because the mute was stored
// under the chat-id key Send reads, not under a platform key that only
// exists once the row does.
func TestMuteSurvivesPlatformRowAppearingLaterThroughSend(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 3) // Stop showing me this — no platform row exists yet

	if err := f.idx.SetChatMetadata("clutch", "telegram", 42, "registered", "clutch"); err != nil {
		t.Fatalf("seed platform row: %v", err)
	}

	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send after platform row: %v", err)
	}
	if res != ResultMuted {
		t.Errorf("Send after platform row = %q, want %q", res, ResultMuted)
	}
	if got := f.conn.sendCount(); got != 1 {
		t.Errorf("posted %d messages after the platform row appeared, want 1 (mute lost)", got)
	}
}

// TestMuteWithPlatformRowAlreadyPresentThroughSend pins the mute key
// agreement in the state that is NORMAL in production: the chat already has
// its registered platform row when the mute is pressed (every real chat
// gets one on first contact). The press must store the mute under exactly
// the key Send later reads — the platform-agnostic (agent, chatID) bucket.
// Keying the WRITE by PlatformForChat instead would store the mute where no
// read ever looks, edit "Muted" all the same, and the suggestion would come
// back on the next Send (the mirror image of the row-appears-later test
// above, which pins the READ).
func TestMuteWithPlatformRowAlreadyPresentThroughSend(t *testing.T) {
	f := newSenderFixture(t)

	// The platform row exists BEFORE the suggestion is even sent.
	if err := f.idx.SetChatMetadata("clutch", "telegram", 42, "registered", "clutch"); err != nil {
		t.Fatalf("seed platform row: %v", err)
	}

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pressButton(t, f.conn, 0, 3) // Stop showing me this

	res, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID)
	if err != nil {
		t.Fatalf("Send after mute: %v", err)
	}
	if res != ResultMuted {
		t.Errorf("Send after mute = %q, want %q", res, ResultMuted)
	}
	if got := f.conn.sendCount(); got != 1 {
		t.Errorf("posted %d messages total, want 1 (nothing after the mute)", got)
	}
}

// TestMuteWriteFailureEditsHonestText proves the F1 honesty rule on the
// failure side: when the mute could not be stored, the edit must NOT claim
// "Muted" — the user is told the save failed instead.
func TestMuteWriteFailureEditsHonestText(t *testing.T) {
	f := newSenderFixture(t)

	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.idx.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
	pressButton(t, f.conn, 0, 3) // Stop showing me this — storage now closed

	edits := f.conn.editsSnapshot()
	if len(edits) != 1 {
		t.Fatalf("edits = %v, want exactly one", edits)
	}
	if edits[0].text == mutedText {
		t.Errorf("edit claims %q although the mute was not stored", mutedText)
	}
	if edits[0].text != muteStoreFailedText {
		t.Errorf("edit text = %q, want the honest failure %q", edits[0].text, muteStoreFailedText)
	}
}

// TestSendMuteReadErrorPropagates: an unreadable mute STORE (a DB error, not
// a garbage value) surfaces from Send instead of silently posting over a
// possibly-muted chat.
func TestSendMuteReadErrorPropagates(t *testing.T) {
	f := newSenderFixture(t)
	if err := f.idx.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, validSpec().ID); err == nil {
		t.Error("Send with an unreadable mute store = nil error, want error")
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages despite the read error, want 0", got)
	}
}

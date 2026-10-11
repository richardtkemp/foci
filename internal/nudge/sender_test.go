package nudge

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
// nudge path actually calls.
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

// fakeTextConn is a connection WITHOUT the ButtonSender surface: the nudge
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
	f.sender = NewSender(SenderDeps{
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
		if !strings.HasPrefix(id, "nudge-"+validSpec().ID+"-") {
			t.Errorf("interactive id %q lacks the nudge-<nudgeID>- prefix", id)
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

// TestSendUnknownNudgeIDErrors: an unregistered id is an error, not a send.
func TestSendUnknownNudgeIDErrors(t *testing.T) {
	f := newSenderFixture(t)
	if _, err := f.sender.Send(context.Background(), "clutch", f.chatKey, "nope.nudge"); err == nil {
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
func TestSendNonChatSessionKeyErrors(t *testing.T) {
	f := newSenderFixture(t)
	if _, err := f.sender.Send(context.Background(), "clutch", "clutch/inosend", validSpec().ID); err == nil {
		t.Error("Send(independent session) = nil error, want error")
	}
	if got := f.conn.sendCount(); got != 0 {
		t.Errorf("posted %d messages for a non-chat session, want 0", got)
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
	// dispatcher stays silent and the router knows no nudge prompt.
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("dispatcher ran with a text-only connection: %v", calls)
	}
	if edit, _, ok := platform.HandleInteractiveCallback("nudge-" + validSpec().ID + "-forged:0"); ok || edit != "" {
		t.Error("a nudge press routed without a ButtonSender connection")
	}
}

// --- presses ------------------------------------------------------------

// TestPressRunsSpecCommandOnSessionKey proves the press path: pressing action
// i dispatches exactly Actions[i].Command — the command from the REGISTERED
// SPEC, not from the press data — through the dispatcher on the nudge's
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
		t.Errorf("dispatch session key = %q, want the nudge's %q", calls[0][0], f.chatKey)
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
		t.Errorf("edit msgID = %q, want the nudge message msg-1", edits[0].msgID)
	}
}

// TestPressEmptyResultEditsDone: a command that answers nothing still closes
// the nudge with "Done".
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
// success — the nudge edits to the honest unavailable text.
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
		t.Error("Not now muted the nudge")
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
		t.Error("out-of-range press muted the nudge")
	}
}

// --- the mute built-in ----------------------------------------------------

// TestPressStopShowingMutes proves the mute built-in: the press stores the
// nudge id for the chat and edits the message to the mute confirmation —
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
		t.Error("mute press did not store the nudge id")
	}
	if calls := f.disp.callsSnapshot(); len(calls) != 0 {
		t.Errorf("mute press dispatched commands: %v", calls)
	}
}

// TestSendReturnsMutedAfterMutePress proves the round trip the confirmation
// text promises: after the mute press, a later Send of that id returns Muted
// and posts nothing, while a different nudge id still sends.
func TestSendReturnsMutedAfterMutePress(t *testing.T) {
	f := newSenderFixture(t)
	if err := f.reg.Register(Spec{ID: "other.nudge", Text: "Another thing"}); err != nil {
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

	// Another nudge id is unaffected: it still sends.
	res, err = f.sender.Send(context.Background(), "clutch", f.chatKey, "other.nudge")
	if err != nil {
		t.Fatalf("Send other nudge: %v", err)
	}
	if res != ResultSent || f.conn.sendCount() != 2 {
		t.Errorf("other nudge: res=%q sends=%d, want sent and a second post", res, f.conn.sendCount())
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

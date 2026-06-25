package app

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/platform"
)

// fakeClient is a wsClient whose send channel we drain in tests.
func fakeClient() *wsClient {
	return &wsClient{
		send:     make(chan []byte, 64),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
	}
}

// drain decodes every queued wire frame into (t, payload-map) pairs.
func drain(t *testing.T, c *wsClient) []decoded {
	t.Helper()
	var out []decoded
	for {
		select {
		case b := <-c.send:
			var env struct {
				T string          `json:"t"`
				D json.RawMessage `json:"d"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad wire frame: %v", err)
			}
			d := map[string]any{}
			if len(env.D) > 0 {
				_ = json.Unmarshal(env.D, &d)
			}
			out = append(out, decoded{t: env.T, d: d})
		default:
			return out
		}
	}
}

type decoded struct {
	t string
	d map[string]any
}

func types(ds []decoded) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.t
	}
	return out
}

func TestChatIDForConv_DeterministicAndPositive(t *testing.T) {
	a := chatIDForConv("conv-abc")
	b := chatIDForConv("conv-abc")
	c := chatIDForConv("conv-xyz")
	if a != b {
		t.Errorf("not deterministic: %d != %d", a, b)
	}
	if a == c {
		t.Errorf("distinct conversations collided: %d", a)
	}
	if a <= 0 {
		t.Errorf("chatID must be positive, got %d", a)
	}
}

func TestAppSink_StreamingTranslation(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextDelta{Delta: "Hel"})
	s.Emit(ctx, turnevent.TextDelta{Delta: "lo"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Hello"})

	got := drain(t, c)
	want := []string{
		fap.TypeTyping,    // TurnStart → typing on
		fap.TypeTurnStart, // first delta lazily opens the turn
		fap.TypeTextDelta,
		fap.TypeTextDelta,
		fap.TypeTextEnd, // TurnComplete finalizes the streamed message
		fap.TypeTyping,  // typing off
	}
	if g := types(got); !equal(g, want) {
		t.Fatalf("frame sequence = %v, want %v", g, want)
	}
	// turn.start and text.end must share the turnId.
	if got[1].d["turnId"] != got[4].d["turnId"] {
		t.Errorf("turnId mismatch between turn.start and text.end")
	}
	if got[4].d["finalText"] != "Hello" {
		t.Errorf("text.end finalText = %v, want Hello", got[4].d["finalText"])
	}
}

func TestAppSink_NonStreamedFinalText(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	// No deltas — a single complete reply.
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done"})

	got := drain(t, c)
	if w := types(got); !equal(w, []string{fap.TypeMessage, fap.TypeTyping}) {
		t.Fatalf("frames = %v, want [message typing]", w)
	}
	if got[0].d["text"] != "done" || got[0].d["role"] != "agent" {
		t.Errorf("message payload wrong: %v", got[0].d)
	}
}

func TestAppSink_SilentFinalSuppressed(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "[[NO_RESPONSE]]"})

	got := drain(t, c)
	// Only the typing-off frame; no message for a silent turn.
	if w := types(got); !equal(w, []string{fap.TypeTyping}) {
		t.Fatalf("frames = %v, want just [typing] (silent suppressed)", w)
	}
}

func newTestHub() *Hub {
	return &Hub{
		deps:      platform.ProviderDeps{},
		agents:    make(map[string]*appConn),
		bySession: make(map[string]*convBinding),
		clients:   make(map[*wsClient]struct{}),
		prompts:   make(map[string]*convBinding),
	}
}

func TestSendToSession_EmitsMessageFrame(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c}
	h.bySession[b.sessionKey] = b

	conn := &appConn{hub: h, agentID: "ag"}
	if err := conn.SendToSession(b.sessionKey, "hi there"); err != nil {
		t.Fatal(err)
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeMessage {
		t.Fatalf("frames = %v, want one message", types(got))
	}
	if got[0].d["text"] != "hi there" {
		t.Errorf("text = %v", got[0].d["text"])
	}
}

func TestSendToSession_SilentSuppressed(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c}
	h.bySession[b.sessionKey] = b

	conn := &appConn{hub: h, agentID: "ag"}
	_ = conn.SendToSession(b.sessionKey, "  [[NO_RESPONSE]]  ")
	if got := drain(t, c); len(got) != 0 {
		t.Fatalf("silent send must emit nothing, got %v", types(got))
	}
}

func TestDispatchPing_RepliesPong(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	ping := `{"t":"ping","id":"x"}`
	h.dispatchInbound(c, []byte(ping))
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypePong {
		t.Fatalf("ping must reply pong, got %v", types(got))
	}
}

// --- slice 2: interactive prompts ---

// boundConn wires a hub + socket + binding for a conversation and returns an
// appConn whose default session is that conversation, plus the binding/socket.
func boundConn(t *testing.T) (*Hub, *wsClient, *convBinding, *appConn) {
	t.Helper()
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c, chatID: 7}
	h.bySession[b.sessionKey] = b
	c.convByID["c1"] = b
	conn := &appConn{hub: h, agentID: "ag", defaultSession: b.sessionKey}
	return h, c, b, conn
}

func TestPromptIDFromButtons(t *testing.T) {
	id := promptIDFromButtons([]platform.ButtonChoice{{Label: "Allow", Data: "req-9:0"}, {Label: "Deny", Data: "req-9:1"}})
	if id != "req-9" {
		t.Errorf("promptID = %q, want req-9", id)
	}
	// No buttons → a fresh ULID (non-empty), never a panic.
	if got := promptIDFromButtons(nil); len(got) != 26 {
		t.Errorf("empty buttons should yield a ULID, got %q", got)
	}
}

func TestSendTextWithButtons_EmitsInteractive(t *testing.T) {
	h, c, _, conn := boundConn(t)
	msgID, err := conn.SendTextWithButtons("Allow Bash?",
		[]platform.ButtonChoice{{Label: "Allow", Data: "req-1:0"}, {Label: "Deny", Data: "req-1:1"}}, "im:")
	if err != nil {
		t.Fatal(err)
	}
	if msgID != "req-1" {
		t.Errorf("msgID = %q, want req-1 (the promptID)", msgID)
	}
	if h.bindingForPrompt("req-1") == nil {
		t.Errorf("prompt not registered")
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractive {
		t.Fatalf("frames = %v, want [interactive]", types(ds))
	}
	if ds[0].d["promptId"] != "req-1" {
		t.Errorf("promptId = %v", ds[0].d["promptId"])
	}
	choices, _ := ds[0].d["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices = %v, want 2", ds[0].d["choices"])
	}
	if first, _ := choices[0].(map[string]any); first["data"] != "req-1:0" || first["label"] != "Allow" {
		t.Errorf("choice[0] = %v", choices[0])
	}
}

func TestSendTextWithButtons_OfflineReturnsErr(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", defaultSession: "ag/capp1/9"} // no binding
	if _, err := conn.SendTextWithButtons("x", []platform.ButtonChoice{{Label: "Allow", Data: "r:0"}}, "im:"); err == nil {
		t.Errorf("offline SendTextWithButtons must return an error")
	}
}

// TestInteractive_ButtonRoundTrip drives the real platform interactive
// machinery: present a permission prompt, simulate the app echoing the chosen
// Choice.Data, and assert the callback fired and a resolution edit was emitted.
func TestInteractive_ButtonRoundTrip(t *testing.T) {
	h, c, _, conn := boundConn(t)
	resolve := func() platform.Connection { return conn }

	var chosen string
	_, err := platform.SendInteractiveMessageWithID(resolve, "req-rt", "Allow Bash?",
		[]platform.ButtonChoice{{Label: "Allow", Data: "allow"}, {Label: "Deny", Data: "deny"}},
		func(choice platform.ButtonChoice) string { chosen = choice.Data; return "✅ Approved" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ds := drain(t, c); len(ds) != 1 || ds[0].t != fap.TypeInteractive {
		t.Fatalf("present frames = %v, want [interactive]", types(ds))
	}

	// User taps Allow → the app echoes Choice.Data ("<promptID>:<index>").
	h.handleInteractiveResponse(c, fap.InteractiveResponse{ConversationID: "c1", PromptID: "req-rt", Data: "req-rt:0"})

	if chosen != "allow" {
		t.Errorf("callback choice = %q, want allow", chosen)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractiveEdit {
		t.Fatalf("after tap frames = %v, want [interactive.edit]", types(ds))
	}
	if ds[0].d["text"] != "✅ Approved" {
		t.Errorf("edit text = %v, want ✅ Approved", ds[0].d["text"])
	}
	if h.bindingForPrompt("req-rt") != nil {
		t.Errorf("registration should be cleared after resolution")
	}
}

// TestInteractive_NextQuestionSuppressesEdit verifies the seq-advance guard: a
// callback that re-renders the conversation (e.g. presenting the next question
// of a multi-question ask) must suppress the resolved-prompt edit so the new
// question is not clobbered.
func TestInteractive_NextQuestionSuppressesEdit(t *testing.T) {
	h, c, b, conn := boundConn(t)
	resolve := func() platform.Connection { return conn }

	_, err := platform.SendInteractiveMessageWithID(resolve, "req-mq", "Q1?",
		[]platform.ButtonChoice{{Label: "A", Data: "qa:0"}, {Label: "B", Data: "qa:1"}},
		func(choice platform.ButtonChoice) string {
			// Simulate presenting the next question: any frame advances seq.
			b.send(fap.Typing{ConversationID: "c1", On: true})
			return "✅ A"
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c) // consume the present frame

	h.handleInteractiveResponse(c, fap.InteractiveResponse{ConversationID: "c1", PromptID: "req-mq", Data: "req-mq:0"})

	for _, d := range drain(t, c) {
		if d.t == fap.TypeInteractiveEdit {
			t.Errorf("resolved-prompt edit must be suppressed when seq advanced")
		}
	}
	if h.bindingForPrompt("req-mq") == nil {
		t.Errorf("registration must survive a seq-advancing resolution")
	}
}

func TestEditMessageText_EmitsEditAndClears(t *testing.T) {
	h, c, b, conn := boundConn(t)
	h.registerPrompt("p1", b)
	if err := conn.EditMessageText("p1", "❌ cancelled"); err != nil {
		t.Fatal(err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractiveEdit || ds[0].d["text"] != "❌ cancelled" {
		t.Fatalf("frames = %v / text = %v", types(ds), ds[0].d["text"])
	}
	if h.bindingForPrompt("p1") != nil {
		t.Errorf("registration not cleared")
	}
	// Idempotent: an unknown promptID is a silent no-op.
	if err := conn.EditMessageText("nope", "x"); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, c); len(got) != 0 {
		t.Fatalf("unknown edit must emit nothing, got %v", types(got))
	}
}

func TestRemoveClient_PurgesPrompts(t *testing.T) {
	h, c, b, _ := boundConn(t)
	h.addClient(c)
	h.registerPrompt("p1", b)
	h.removeClient(c)
	if h.bindingForPrompt("p1") != nil {
		t.Errorf("prompt registration should be purged on disconnect")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

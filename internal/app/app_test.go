package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/session"
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
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextDelta{Delta: "Hel"})
	s.Emit(ctx, turnevent.TextDelta{Delta: "lo"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Hello"})

	got := drain(t, c)
	// Opens with warming-on then typing-on (warming precedes typing at TurnStart);
	// closes with typing-off. Deltas may be batched by the stream pump, so this
	// asserts invariants rather than an exact frame count: one turn.start, one
	// text.end (sharing the turnId), ≥1 delta forming a prefix of the final text,
	// and no whole-message frame (a streamed reply is never re-sent).
	if len(got) < 4 || got[0].t != fap.TypeActivity || got[0].d["kind"] != "warming" {
		t.Fatalf("first frame must be activity warming, got %v", types(got))
	}
	if got[1].t != fap.TypeActivity || got[1].d["kind"] != "typing" {
		t.Fatalf("second frame must be activity typing, got %v", types(got))
	}
	if last := got[len(got)-1]; last.t != fap.TypeActivity || last.d["kind"] != "idle" {
		t.Fatalf("last frame must be activity idle, got %v", types(got))
	}
	var turnStarts, textEnds int
	var startTurnID, endTurnID, deltaText, deltaTurnID, finalText string
	for _, d := range got {
		switch d.t {
		case fap.TypeTurnStart:
			turnStarts++
			startTurnID, _ = d.d["turnId"].(string)
		case fap.TypeTextDelta:
			s, _ := d.d["text"].(string)
			deltaText += s
			deltaTurnID, _ = d.d["turnId"].(string)
		case fap.TypeTextEnd:
			textEnds++
			endTurnID, _ = d.d["turnId"].(string)
			finalText, _ = d.d["finalText"].(string)
		case fap.TypeMessage:
			t.Errorf("streamed reply must not also emit a whole message frame")
		}
	}
	if turnStarts != 1 || textEnds != 1 {
		t.Fatalf("want one turn.start + one text.end, got %d/%d (%v)", turnStarts, textEnds, types(got))
	}
	if startTurnID == "" || startTurnID != endTurnID {
		t.Errorf("turn.start/text.end turnId mismatch: %q vs %q", startTurnID, endTurnID)
	}
	if deltaText == "" || !strings.HasPrefix("Hello", deltaText) {
		t.Errorf("streamed deltas %q must be a non-empty prefix of Hello", deltaText)
	}
	if deltaTurnID != "" && deltaTurnID != startTurnID {
		t.Errorf("delta turnId %q != stream turnId %q", deltaTurnID, startTurnID)
	}
	if finalText != "Hello" {
		t.Errorf("text.end finalText = %q, want Hello", finalText)
	}
}

func TestAppSink_InTurnSnapshot(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	if b.info().Activity != "idle" {
		t.Fatal("activity should be idle before any turn")
	}
	s.Emit(ctx, turnevent.TurnStart{})
	if b.info().Activity == "idle" {
		t.Fatal("activity should be non-idle mid-turn")
	}
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "x"})
	if b.info().Activity != "idle" {
		t.Fatal("activity should be idle after TurnComplete")
	}
}

// A turn abandoned without TurnComplete must still clear the snapshot via the
// deferred cleanup, or the roster would report a phantom typing indicator.
func TestAppSink_CleanupClearsInTurn(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)

	s.Emit(context.Background(), turnevent.TurnStart{})
	if b.info().Activity == "idle" {
		t.Fatal("activity should be non-idle mid-turn")
	}
	s.cleanup()
	if b.info().Activity != "idle" {
		t.Fatal("cleanup must clear the activity for an abandoned turn")
	}
}

func TestAppSink_WarmingLifecycle(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	if b.info().Activity != "warming" {
		t.Fatal("activity should be warming at turn start")
	}
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: ""})
	if b.info().Activity != "thinking" {
		t.Fatal("warming should give way to thinking on the first output")
	}
}

func TestAppSink_ToolLifecycle(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.ToolCall{Name: "Bash"})
	if b.info().Activity != "tool" || b.info().ActivityDetail != "Bash" {
		t.Fatalf("activity should be tool/Bash while running, got %q/%q", b.info().Activity, b.info().ActivityDetail)
	}
	s.Emit(ctx, turnevent.ToolResult{Name: "Bash"})
	if b.info().Activity != "warming" || b.info().ActivityDetail != "" {
		t.Fatalf("activity should return to warming after tool result, got %q/%q", b.info().Activity, b.info().ActivityDetail)
	}
}

// TestConvBinding_ActivityResolver pins the unified resolver precedence
// (subagents > waiting > tool > thinking > warming > typing > idle) and proves
// that setting/clearing each input emits exactly one Activity frame per resolved
// change (the binding-side change-check dedups no-op transitions).
func TestConvBinding_ActivityResolver(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}

	// Turn-scoped thinking is in flight.
	b.setTurnActivity(fap.ActivityKindThinking, "")
	if k, d := b.info().Activity, b.info().ActivityDetail; k != "thinking" || d != "" {
		t.Fatalf("after thinking: %q/%q, want thinking/", k, d)
	}

	// Subagents outrank the turn-scoped thinking.
	b.setSubagentDetail("build docs, run tests")
	if k, d := b.info().Activity, b.info().ActivityDetail; k != "subagents" || d != "build docs, run tests" {
		t.Fatalf("subagents must outrank thinking: %q/%q", k, d)
	}

	// Waiting is below subagents — no change while subagents are present (deduped).
	b.setWaitingDetail("scout")
	if b.info().Activity != "subagents" {
		t.Fatalf("subagents must outrank waiting, got %q", b.info().Activity)
	}

	// Clearing subagents surfaces the waiting state (still above the turn thinking).
	b.setSubagentDetail("")
	if k, d := b.info().Activity, b.info().ActivityDetail; k != "waiting" || d != "scout" {
		t.Fatalf("after clearing subagents: %q/%q, want waiting/scout", k, d)
	}

	// Clearing waiting falls back to the turn-scoped thinking.
	b.setWaitingDetail("")
	if b.info().Activity != "thinking" {
		t.Fatalf("after clearing waiting: %q, want thinking", b.info().Activity)
	}

	// Ending the turn resolves to idle.
	b.setTurnActivity(fap.ActivityKindIdle, "")
	if b.info().Activity != "idle" {
		t.Fatalf("after idle turn: %q, want idle", b.info().Activity)
	}

	// Exactly one Activity frame per resolved change; the no-op waiting set while
	// subagents were present emitted nothing.
	kinds := activityKinds(drain(t, c))
	want := []string{"thinking", "subagents", "waiting", "thinking", "idle"}
	if len(kinds) != len(want) {
		t.Fatalf("emitted kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("emitted kinds = %v, want %v", kinds, want)
		}
	}
}

func TestAppSink_ThinkingSnapshot(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	if b.info().Activity == "thinking" {
		t.Fatal("activity should not be thinking before any reasoning")
	}
	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: "hmm"})
	if b.info().Activity != "thinking" {
		t.Fatal("activity should be thinking mid-reasoning")
	}
	s.Emit(ctx, turnevent.TextDelta{Delta: "answer"})
	if b.info().Activity == "thinking" {
		t.Fatal("activity should leave thinking when text begins")
	}
	// A second reasoning phase (think → tool → think) flips it back on.
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: "more"})
	if b.info().Activity != "thinking" {
		t.Fatal("activity should re-arm thinking on a later reasoning phase")
	}
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "answer"})
	if b.info().Activity == "thinking" {
		t.Fatal("activity should not be thinking after TurnComplete")
	}
}

// activityKinds extracts the ordered kind sequence of Activity frames.
func activityKinds(got []decoded) []string {
	var out []string
	for _, d := range got {
		if d.t == fap.TypeActivity {
			k, _ := d.d["kind"].(string)
			out = append(out, k)
		}
	}
	return out
}

func TestAppSink_ThinkingFrames(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: "a"})
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: "b"}) // deduped: same resolved kind
	s.Emit(ctx, turnevent.TextDelta{Delta: "x"})
	s.Emit(ctx, turnevent.ToolCall{Name: "Read"})
	s.Emit(ctx, turnevent.ThinkingDelta{Delta: "c"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "x"})

	got := drain(t, c)
	kinds := activityKinds(got)
	want := []string{"warming", "thinking", "typing", "tool", "thinking", "idle"}
	if len(kinds) != len(want) {
		t.Fatalf("activity kinds = %v, want %v (all: %v)", kinds, want, types(got))
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("activity kinds = %v, want %v", kinds, want)
		}
	}
}

func TestAppSink_NoThinkingFramesWhenAbsent(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextDelta{Delta: "hi"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "hi"})

	for _, k := range activityKinds(drain(t, c)) {
		if k == "thinking" {
			t.Fatalf("thinking-free turn emitted a thinking activity frame")
		}
	}
}

// TestAppSink_MultiReplyNoDoubleDelivery is the regression test for the
// double-delivery bug: a turn with two replies (reply → tool → reply) must
// render as two distinct finalized bubbles, never re-sending a streamed reply as
// a separate whole-message frame. The CC delegated path fires
// TextBlock{Intermediate} for every reply (including the last), then repeats the
// last reply as TurnComplete.FinalText — which the shared delivered-flag must
// suppress.
func TestAppSink_MultiReplyNoDoubleDelivery(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	// reply 1: streams, then completes as an intermediate block.
	s.Emit(ctx, turnevent.TextDelta{Delta: "first"})
	s.Emit(ctx, turnevent.TextBlock{Text: "first", Phase: turnevent.PhaseIntermediate})
	// (a tool call happens here — ignored by the app sink)
	// reply 2: streams, then completes as an intermediate block.
	s.Emit(ctx, turnevent.TextDelta{Delta: "second"})
	s.Emit(ctx, turnevent.TextBlock{Text: "second", Phase: turnevent.PhaseIntermediate})
	// TurnComplete repeats the last reply — must be suppressed, not re-delivered.
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "second"})

	got := drain(t, c)
	var ends []decoded
	for _, d := range got {
		if d.t == fap.TypeMessage {
			t.Fatalf("double delivery: a streamed reply was re-sent as a whole message (%v)", types(got))
		}
		if d.t == fap.TypeTextEnd {
			ends = append(ends, d)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("want 2 text.end frames (one finalized bubble per reply), got %d (%v)", len(ends), types(got))
	}
	if ends[0].d["turnId"] == ends[1].d["turnId"] {
		t.Errorf("the two replies must use distinct turnIds, both %v", ends[0].d["turnId"])
	}
	if ends[0].d["finalText"] != "first" || ends[1].d["finalText"] != "second" {
		t.Errorf("text.end finalTexts = %q,%q want first,second", ends[0].d["finalText"], ends[1].d["finalText"])
	}
}

func TestAppSink_NonStreamedFinalText(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	// No deltas — a single complete reply.
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done"})

	got := drain(t, c)
	if w := types(got); !equal(w, []string{fap.TypeMessage}) {
		t.Fatalf("frames = %v, want [message] (no turn ⇒ no activity change)", w)
	}
	if got[0].d["text"] != "done" || got[0].d["role"] != "agent" {
		t.Errorf("message payload wrong: %v", got[0].d)
	}
}

func TestAppSink_SilentFinalSuppressed(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "[[NO_RESPONSE]]"})

	got := drain(t, c)
	// No message for a silent turn, and no activity change (idle stayed idle).
	if len(got) != 0 {
		t.Fatalf("frames = %v, want none (silent suppressed, no activity change)", types(got))
	}
}

func newTestHub() *Hub {
	return &Hub{
		deps:          platform.ProviderDeps{},
		blobs:         newBlobStore(),
		tokens:        newPushTokens(),
		devices:       newDeviceStore(""),
		pairKeys:      newPairKeyStore(),
		authLim:       newAuthLimiter(authFailMax, authFailWindow),
		agents:        make(map[string]*appConn),
		convs:         make(map[string]*convBinding),
		bySession:     make(map[string]*convBinding),
		clients:       make(map[*wsClient]struct{}),
		prompts:       make(map[string]*convBinding),
		batchPrompts:  make(map[string]*batchPrompt),
		notifs:        make(map[string]*convBinding),
		wizards:       make(map[string]*wizardSession),
		wizardByScope: make(map[string]string),
	}
}

func TestSendToSession_EmitsMessageFrame(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/c7", clients: map[*wsClient]struct{}{c: {}}}
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

func TestSendToSession_FallsBackToDefaultChat(t *testing.T) {
	// An unsolicited send to a session with no binding surfaces in the agent's
	// default app chat instead of vanishing (#959).
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	if err := idx.SetDefaultChat("ag", "app", 42); err != nil {
		t.Fatal(err)
	}
	c := fakeClient()
	def := &convBinding{convID: "cdef", sessionKey: "ag/cdef/9", agentID: "ag", chatID: 42, clients: map[*wsClient]struct{}{c: {}}}
	h.convs[def.convID] = def

	conn := &appConn{hub: h, agentID: "ag"}
	if err := conn.SendToSession("ag/never-bound/1", "surfaced"); err != nil {
		t.Fatal(err)
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeMessage || got[0].d["text"] != "surfaced" {
		t.Fatalf("want one message 'surfaced' on the default chat, got %v", types(got))
	}
	if got[0].d["conversationId"] != "cdef" {
		t.Errorf("routed to conv %v, want cdef (default chat)", got[0].d["conversationId"])
	}
}

func TestSendToSession_StaleDefaultFallsToLatest(t *testing.T) {
	// Default pinned to a conversation that isn't live → the deliver ladder
	// treats it as absent and the send lands in the most recently active live
	// conversation. The default pin is user-owned and left untouched.
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	if err := idx.SetDefaultChat("ag", "app", 42); err != nil {
		t.Fatal(err)
	}
	c := fakeClient()
	other := &convBinding{convID: "cother", sessionKey: "ag/c99", agentID: "ag", chatID: 99, clients: map[*wsClient]struct{}{c: {}}}
	h.convs[other.convID] = other

	conn := &appConn{hub: h, agentID: "ag"}
	if err := conn.SendToSession("ag/inever-bound", "lost"); err != nil {
		t.Fatal(err)
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != "message" {
		t.Fatalf("send must deliver to the latest live conversation, got %v", types(got))
	}
	if got[0].d["conversationId"] != "cother" {
		t.Errorf("routed to conv %v, want cother", got[0].d["conversationId"])
	}
	if dc := idx.DefaultChatForAgent("ag", "app"); dc != 42 {
		t.Errorf("default chat = %d, want 42 unchanged (pins are user-owned)", dc)
	}
}

func TestDeliverBinding_NoConversationsCreates(t *testing.T) {
	// An agent with NO app conversations gets a server-minted one: the send
	// ladder creates a binding (leaving the user-owned default pin unset) and
	// pushes an updated roster to live sockets so a connected device learns
	// it immediately.
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	c := fakeClient()
	h.clients[c] = struct{}{}

	conn := &appConn{hub: h, agentID: "ag"}
	if err := conn.SendToSession("", "hello out of nowhere"); err != nil {
		t.Fatal(err)
	}

	h.mu.RLock()
	nConvs := len(h.convs)
	h.mu.RUnlock()
	if nConvs != 1 {
		t.Fatalf("expected 1 server-created conversation, got %d", nConvs)
	}
	if dc := idx.DefaultChatForAgent("ag", "app"); dc != 0 {
		t.Errorf("default chat = %d, want unset (pins are user-owned, never automatic)", dc)
	}
	// The socket saw a roster push (hello) — the created conversation is
	// buffered on its binding, not the socket (nil client at creation).
	sawHello := false
	for _, f := range drain(t, c) {
		if f.t == "hello" {
			sawHello = true
		}
	}
	if !sawHello {
		t.Error("live socket did not receive a roster push after server-side creation")
	}
}

func TestNotifyUnbound_TargetsDefaultConversationOnly(t *testing.T) {
	// The unbound conn's notifications (broadcast warnings) go to the default
	// conversation via the deliver ladder — NOT a fan-out to every binding.
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	cDef, cOther := fakeClient(), fakeClient()
	def := &convBinding{convID: "cdef", sessionKey: "ag/c42", agentID: "ag", chatID: 42, clients: map[*wsClient]struct{}{cDef: {}}}
	other := &convBinding{convID: "cother", sessionKey: "ag/c99", agentID: "ag", chatID: 99, clients: map[*wsClient]struct{}{cOther: {}}}
	h.convs[def.convID] = def
	h.convs[other.convID] = other
	if err := idx.SetDefaultChat("ag", "app", 42); err != nil {
		t.Fatal(err)
	}

	conn := &appConn{hub: h, agentID: "ag"}
	if msgID := conn.SendNotificationDirect("⚡ rate limited"); msgID == "" {
		t.Fatal("unbound notification should return an editable messageID")
	}
	if got := drain(t, cDef); len(got) != 1 || got[0].t != "notification" {
		t.Fatalf("default conversation should receive the notification, got %v", types(got))
	}
	if got := drain(t, cOther); len(got) != 0 {
		t.Fatalf("non-default conversation must NOT receive the notification, got %v", types(got))
	}
}

func TestSendToSession_SilentSuppressed(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/c7", clients: map[*wsClient]struct{}{c: {}}}
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
	b := &convBinding{convID: "c1", sessionKey: "ag/c7", clients: map[*wsClient]struct{}{c: {}}, chatID: 7}
	h.bySession[b.sessionKey] = b
	c.convByID["c1"] = b
	conn := &appConn{hub: h, agentID: "ag", bound: b.sessionKey}
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
	conn := &appConn{hub: h, agentID: "ag", bound: "ag/c7"} // no binding
	if _, err := conn.SendTextWithButtons("x", []platform.ButtonChoice{{Label: "Allow", Data: "r:0"}}, "im:"); err == nil {
		t.Errorf("offline SendTextWithButtons must return an error")
	}
}

// TestAppConn_SetTypingIsNoOp guards the typing-indicator contract: the platform
// TypingFunc path (refresh-and-auto-expire, built for Telegram/Discord) must NOT
// drive the app. appConn.SetTyping is a no-op; the app's typing is owned solely by
// appSink, which brackets each turn exactly once (TurnStart on, TurnComplete off).
// If SetTyping ever emits a frame again, the platform path's periodic re-asserts
// and intermediate cancels leak back as redundant frames and mid-session flicker.
func TestAppConn_SetTypingIsNoOp(t *testing.T) {
	_, c, _, conn := boundConn(t)
	conn.SetTyping(true)
	conn.SetTyping(false)
	conn.SetTyping(true)
	if got := drain(t, c); len(got) != 0 {
		t.Fatalf("appConn.SetTyping must not emit frames (typing owned by appSink), got %v", types(got))
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

// TestNotification_EditInPlace drives the compaction ⏳→✅ flow: a direct
// notification returns a stable messageID, and EditMessageText re-sends a
// notification carrying the SAME messageID so the client replaces the row in
// place (one message, not two — the #899 bug). The registration is consumed.
func TestNotification_EditInPlace(t *testing.T) {
	h, c, _, conn := boundConn(t)

	msgID := conn.SendNotificationDirect("⏳ Compacting context...")
	if msgID == "" {
		t.Fatal("SendNotificationDirect returned empty messageID; cannot edit in place")
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeNotification {
		t.Fatalf("start frames = %v, want [notification]", types(ds))
	}
	if ds[0].d["messageId"] != msgID {
		t.Errorf("start messageId = %v, want %q", ds[0].d["messageId"], msgID)
	}
	if h.bindingForNotification(msgID) == nil {
		t.Errorf("notification not registered for later edit")
	}

	if err := conn.EditMessageText(msgID, "✅ Context compacted"); err != nil {
		t.Fatal(err)
	}
	ds = drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeNotification {
		t.Fatalf("edit frames = %v, want [notification] (same id replaces the row)", types(ds))
	}
	if ds[0].d["messageId"] != msgID {
		t.Errorf("edit messageId = %v, want %q (must match to replace in place)", ds[0].d["messageId"], msgID)
	}
	if ds[0].d["text"] != "✅ Context compacted" {
		t.Errorf("edit text = %v", ds[0].d["text"])
	}
	if h.bindingForNotification(msgID) != nil {
		t.Errorf("registration should be cleared after the edit is consumed")
	}
}

// TestInteractive_BatchRoundTrip: a capable client (advertised "interactiveBatch")
// gets ONE Interactive frame carrying all questions, and its single Answers reply
// fires the registered callback with every answer at once.
func TestInteractive_BatchRoundTrip(t *testing.T) {
	h, c, b, conn := boundConn(t)
	c.features = map[string]struct{}{featureInteractiveBatch: {}}
	b.attach(c) // hello→attach caches the capability onto the binding

	var got []string
	batched, err := conn.SendInteractiveBatch("req-b",
		[]platform.BatchQuestion{
			{Text: "Color?", Choices: []platform.ButtonChoice{{Label: "Red", Data: "qa:0"}}},
			{Text: "Size?", Choices: []platform.ButtonChoice{{Label: "Large", Data: "qa:1"}}},
		},
		func(answers []string) { got = answers })
	if err != nil || !batched {
		t.Fatalf("SendInteractiveBatch: batched=%v err=%v; want true,nil", batched, err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractive {
		t.Fatalf("present frames = %v, want [interactive]", types(ds))
	}

	// The app submits both answers positionally in one frame.
	h.handleInteractiveResponse(c, fap.InteractiveResponse{ConversationID: "c1", PromptID: "req-b", Answers: []string{"qa:0", "qa:1"}})

	if len(got) != 2 || got[0] != "qa:0" || got[1] != "qa:1" {
		t.Errorf("callback answers = %v, want [qa:0 qa:1]", got)
	}
	if _, ok := h.batchPromptByID("req-b"); ok {
		t.Errorf("batch registration should be cleared after the reply")
	}
}

// A client that did NOT advertise the capability declines batching, so the ask
// layer falls back to sequential presentation. No frame is emitted here.
func TestInteractive_BatchDeclinedWhenUncapable(t *testing.T) {
	_, c, _, conn := boundConn(t)
	// c.features left nil (no capability advertised).
	batched, err := conn.SendInteractiveBatch("req-x",
		[]platform.BatchQuestion{{Text: "Q?", Choices: []platform.ButtonChoice{{Label: "A", Data: "qa:0"}}}}, func([]string) {})
	if err != nil || batched {
		t.Fatalf("uncapable client: batched=%v err=%v; want false,nil", batched, err)
	}
	if ds := drain(t, c); len(ds) != 0 {
		t.Fatalf("declined batch must emit nothing, got %v", types(ds))
	}
}

// TestInteractive_BatchWhenCapableButOffline: a client that advertised the
// capability and then disconnected still batches — the cached feature set keeps
// supportsFeature true, so the frame is registered + persisted for replay rather
// than degrading to sequential prompts.
func TestInteractive_BatchWhenCapableButOffline(t *testing.T) {
	h, c, b, conn := boundConn(t)
	c.features = map[string]struct{}{featureInteractiveBatch: {}}
	b.attach(c)   // capability cached
	b.attach(nil) // disconnect: b.client nil, cached feature set retained

	batched, err := conn.SendInteractiveBatch("req-off",
		[]platform.BatchQuestion{{Text: "Q?", Choices: []platform.ButtonChoice{{Label: "A", Data: "qa:0"}}}}, func([]string) {})
	if err != nil || !batched {
		t.Fatalf("offline capable client: batched=%v err=%v; want true,nil", batched, err)
	}
	if _, ok := h.batchPromptByID("req-off"); !ok {
		t.Errorf("batch prompt should be registered for replay on reconnect")
	}
}

// TestBinding_RehydratesCapsFromDB: a restart rebuilds bindings with no socket;
// ensureBinding rehydrates the last-advertised caps from chat metadata so a
// capable-but-offline app still resolves capability checks (and batches).
func TestBinding_RehydratesCapsFromDB(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	fs, err := newFrameStore(filepath.Join(dir, "frames.db"), 24*time.Hour)
	if err != nil {
		t.Fatalf("newFrameStore: %v", err)
	}
	h := newTestHub()
	h.deps.SessionIndex = idx
	h.frames = fs

	agentID, convID := "ag", "c1"
	chatID := chatIDForConv(convID)
	if err := idx.SetChatMetadata(agentID, "app", chatID, "features", featureInteractiveBatch); err != nil {
		t.Fatalf("SetChatMetadata: %v", err)
	}

	b := h.ensureBinding(nil, agentID, convID) // StartAll rebuilds bindings with a nil socket
	if !b.supportsFeature(featureInteractiveBatch) {
		t.Fatalf("binding should rehydrate caps from DB across a restart")
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
			b.send(fap.Activity{ConversationID: "c1", Kind: "typing"})
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

// hubClient makes an in-memory wsClient bound to a hub and registered, for
// transport-adjacent tests that don't need a real websocket.
func hubClient(h *Hub, deviceID string) *wsClient {
	c := &wsClient{
		send:     make(chan []byte, 8),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
		hub:      h,
		deviceID: deviceID,
	}
	h.addClient(c)
	return c
}

func isClosed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func TestEvictOtherDeviceSockets_ClosesStaleSameDevice(t *testing.T) {
	h := newTestHub()
	old := hubClient(h, "dev-A")
	keep := hubClient(h, "dev-A")
	other := hubClient(h, "dev-B")

	h.evictOtherDeviceSockets(keep, "dev-A")

	if !isClosed(old.done) {
		t.Error("stale same-device socket should be evicted (4409)")
	}
	if isClosed(keep.done) {
		t.Error("the new socket must NOT be evicted")
	}
	if isClosed(other.done) {
		t.Error("a different device's socket must NOT be evicted")
	}

	h.mu.RLock()
	_, oldLive := h.clients[old]
	_, keepLive := h.clients[keep]
	_, otherLive := h.clients[other]
	h.mu.RUnlock()
	if oldLive {
		t.Error("evicted socket should be removed from hub.clients")
	}
	if !keepLive || !otherLive {
		t.Error("surviving sockets should remain in hub.clients")
	}
}

func TestEvictOtherDeviceSockets_EmptyDeviceIDNoOp(t *testing.T) {
	h := newTestHub()
	a := hubClient(h, "")
	b := hubClient(h, "")
	h.evictOtherDeviceSockets(a, "")
	if isClosed(a.done) || isClosed(b.done) {
		t.Error("empty deviceID must evict nothing (un-helloed sockets)")
	}
}

// --- slice 3: reliability (seq / ack / replay / dedup) ---

type envFrame struct {
	t   string
	seq int64
	ack int64
}

// drainEnv decodes queued frames capturing envelope seq/ack (not just payload).
func drainEnv(t *testing.T, c *wsClient) []envFrame {
	t.Helper()
	var out []envFrame
	for {
		select {
		case b := <-c.send:
			var env struct {
				T   string `json:"t"`
				Seq int64  `json:"seq"`
				Ack int64  `json:"ack"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad wire frame: %v", err)
			}
			out = append(out, envFrame{t: env.T, seq: env.Seq, ack: env.Ack})
		default:
			return out
		}
	}
}

// appBackend delivers subagent progress as distinct, seq-tracked
// subagent.start/text/end frames (not blockquoted ordinary messages), so the app
// can collapse a run and open its trace on demand.
func TestAppBackend_SubagentFrames(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	b := h.ensureBinding(c, "ag", "conv-1")
	be := newAppBackend(b)

	be.DeliverSubagentStart("toolu_1", "Explore")
	be.DeliverSubagentText("toolu_1", "found it")
	be.DeliverSubagentEnd("toolu_1")
	ds := drainEnv(t, c)

	if len(ds) != 3 || ds[0].t != fap.TypeSubagentStart ||
		ds[1].t != fap.TypeSubagentText || ds[2].t != fap.TypeSubagentEnd {
		t.Fatalf("frames = %+v, want [subagent.start subagent.text subagent.end]", ds)
	}
	if !be.SubagentTextRaw() {
		t.Error("app SubagentTextRaw() = false, want true")
	}
}

func TestReliability_SeqSurvivesReconnect(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	b.send(fap.Activity{ConversationID: "conv-1", Kind: "typing"}) // seq 1
	b.send(fap.Activity{ConversationID: "conv-1", Kind: "idle"})   // seq 2
	drainEnv(t, c1)

	h.removeClient(c1) // disconnect; durable state survives
	c2 := fakeClient()
	c2.hub = h
	b2 := h.ensureBinding(c2, "ag", "conv-1")
	if b2 != b {
		t.Fatalf("reconnect must reuse the durable binding")
	}
	b2.send(fap.Activity{ConversationID: "conv-1", Kind: "typing"}) // seq must continue at 3
	ds := drainEnv(t, c2)
	if len(ds) != 1 || ds[0].seq != 3 {
		t.Fatalf("post-reconnect frames = %v, want one frame at seq 3", ds)
	}
}

func TestReliability_ReplayOnResume(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	for i := 0; i < 3; i++ {
		b.send(fap.Activity{ConversationID: "conv-1", Kind: "typing"}) // seq 1,2,3
	}
	drainEnv(t, c1)
	h.removeClient(c1)

	// Reconnect: the client rendered up to seq 1, so resume replays 2 and 3.
	c2 := fakeClient()
	c2.hub = h
	h.resumeConversations(c2, []fap.ResumePoint{{ConversationID: "conv-1", Ack: 1}})
	ds := drainEnv(t, c2)
	if len(ds) != 2 || ds[0].seq != 2 || ds[1].seq != 3 {
		t.Fatalf("replay = %v, want frames at seq [2 3]", ds)
	}
}

func TestReliability_OfflineBuffersThenReplays(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	h.removeClient(c1) // offline: binding detached but retained

	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m1", Role: "agent", Text: "queued offline"})

	c2 := fakeClient()
	c2.hub = h
	h.resumeConversations(c2, []fap.ResumePoint{{ConversationID: "conv-1", Ack: 0}})
	ds := drain(t, c2)
	if len(ds) != 1 || ds[0].t != fap.TypeMessage || ds[0].d["text"] != "queued offline" {
		t.Fatalf("offline frame must replay on resume, got %v", types(ds))
	}
}

func TestReliability_InboundDedup(t *testing.T) {
	b := &convBinding{convID: "c1", seen: make(map[string]struct{})}
	if !b.acceptInbound("id-1", 1) {
		t.Fatal("first frame must be accepted")
	}
	if b.acceptInbound("id-1", 1) {
		t.Fatal("duplicate id must be rejected")
	}
	if !b.acceptInbound("id-2", 2) {
		t.Fatal("new id must be accepted")
	}
	if b.clientSeqHW != 2 {
		t.Errorf("clientSeqHW = %d, want 2", b.clientSeqHW)
	}
}

func TestReliability_OutboundAckStampsClientSeq(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}, seen: make(map[string]struct{})}
	b.acceptInbound("u1", 7) // client's outbound seq high-water is 7
	b.send(fap.Activity{ConversationID: "c1", Kind: "typing"})
	ds := drainEnv(t, c)
	if len(ds) != 1 || ds[0].ack != 7 {
		t.Fatalf("outbound ack = %v, want 7 (client seq high-water)", ds)
	}
}

func TestReliability_AckTrimsBuffer(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}, seen: make(map[string]struct{})}
	for i := 0; i < 5; i++ {
		b.send(fap.Activity{ConversationID: "c1", Kind: "typing"}) // seq 1..5
	}
	drainEnv(t, c)
	b.ackInbound(c, 3)
	b.mu.Lock()
	n, first := len(b.buffer), b.buffer[0].seq
	b.mu.Unlock()
	if n != 2 || first != 4 {
		t.Errorf("after ack(3): %d frames starting seq %d, want 2 frames from seq 4", n, first)
	}
}

func TestReliability_BufferTrimsByDepth(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}, seen: make(map[string]struct{})}
	for i := 0; i < defaultReplayBufferDepth+50; i++ {
		b.send(fap.Activity{ConversationID: "c1", Kind: "typing"})
	}
	drainEnv(t, c)
	b.mu.Lock()
	n, first := len(b.buffer), b.buffer[0].seq
	b.mu.Unlock()
	if n != defaultReplayBufferDepth {
		t.Errorf("buffer depth = %d, want %d", n, defaultReplayBufferDepth)
	}
	if first != 51 { // seq 1..50 dropped, 51..1050 retained
		t.Errorf("oldest retained seq = %d, want 51", first)
	}
}

// --- slice 4: media / blobs ---

func TestBlobStore_PutGetRoundTrip(t *testing.T) {
	s := newBlobStore()
	meta, err := s.putBytes([]byte("hello"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if meta.size != 5 || meta.kind != "document" || meta.mime != "text/plain" {
		t.Errorf("meta = %+v", meta)
	}
	got, ok := s.get(meta.id)
	if !ok || got != meta {
		t.Errorf("get(%q) failed", meta.id)
	}
	data, err := os.ReadFile(meta.path)
	if err != nil || string(data) != "hello" {
		t.Errorf("blob file = %q (%v)", data, err)
	}
}

func TestBlobStore_SizeCap(t *testing.T) {
	s := newBlobStore()
	s.maxBytes = 4
	if _, err := s.put(bytes.NewReader([]byte("12345")), "document", "x", "text/plain"); !errors.Is(err, errBlobTooLarge) {
		t.Fatalf("over-cap put error = %v, want errBlobTooLarge", err)
	}
	if _, err := s.put(bytes.NewReader([]byte("1234")), "document", "x", "text/plain"); err != nil {
		t.Fatalf("at-cap put: %v", err)
	}
}

func TestSendPhoto_StoresBlobAndEmitsMedia(t *testing.T) {
	h, c, _, conn := boundConn(t)
	tmp := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(tmp, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendPhoto(tmp, "nice pic"); err != nil {
		t.Fatal(err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeMedia {
		t.Fatalf("frames = %v, want [media]", types(ds))
	}
	d := ds[0].d
	if d["mime"] != "image/png" || d["caption"] != "nice pic" {
		t.Errorf("media payload = %v", d)
	}
	blobID, _ := d["blobId"].(string)
	meta, ok := h.blobs.get(blobID)
	if !ok || meta.size != 7 {
		t.Errorf("blob not stored: ok=%v meta=%+v", ok, meta)
	}
}

func TestResolveAttachments_ReadsBlob(t *testing.T) {
	h := newTestHub()
	meta, err := h.blobs.putBytes([]byte("hello"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	atts := h.resolveAttachments([]fap.AttachmentRef{{BlobID: meta.id, Kind: "document", MIME: "text/plain", Name: "f.txt"}})
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if string(atts[0].Data) != "hello" || atts[0].MimeType != "text/plain" || atts[0].SavedPath != meta.path {
		t.Errorf("attachment = %+v", atts[0])
	}
	if got := h.resolveAttachments([]fap.AttachmentRef{{BlobID: "nope"}}); len(got) != 0 {
		t.Errorf("unknown blob must be skipped, got %v", got)
	}
}

func TestBlobHTTP_UploadThenDownload(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev", "")

	up := httptest.NewRequest(http.MethodPost, "/app/blob", strings.NewReader("filedata"))
	up.Header.Set("Authorization", "Bearer "+d.Token)
	up.Header.Set("Content-Type", "image/png")
	up.Header.Set("X-Filename", "p.png")
	uw := httptest.NewRecorder()
	h.ServeBlobPost(uw, up)
	if uw.Code != http.StatusOK {
		t.Fatalf("upload code = %d", uw.Code)
	}
	var res struct {
		BlobID string `json:"blobId"`
		Size   int64  `json:"size"`
		Mime   string `json:"mime"`
	}
	if err := json.Unmarshal(uw.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.BlobID == "" || res.Size != 8 || res.Mime != "image/png" {
		t.Fatalf("upload result = %+v", res)
	}

	dn := httptest.NewRequest(http.MethodGet, "/app/blob/"+res.BlobID, nil)
	dn.Header.Set("Authorization", "Bearer "+d.Token)
	dw := httptest.NewRecorder()
	h.ServeBlobGet(dw, dn)
	if dw.Code != http.StatusOK || dw.Body.String() != "filedata" {
		t.Fatalf("download code=%d body=%q", dw.Code, dw.Body.String())
	}
	if ct := dw.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
}

func TestBlobHTTP_Auth(t *testing.T) {
	h := newTestHub()

	noAuth := httptest.NewRequest(http.MethodPost, "/app/blob", strings.NewReader("x"))
	nw := httptest.NewRecorder()
	h.ServeBlobPost(nw, noAuth)
	if nw.Code != http.StatusUnauthorized {
		t.Fatalf("no-bearer code = %d, want 401", nw.Code)
	}

	wrong := httptest.NewRequest(http.MethodGet, "/app/blob/abc", nil)
	wrong.Header.Set("Authorization", "Bearer nope")
	ww := httptest.NewRecorder()
	h.ServeBlobGet(ww, wrong)
	if ww.Code != http.StatusForbidden {
		t.Fatalf("wrong-key code = %d, want 403", ww.Code)
	}
}

// --- slice 5: push (offline wake) ---

func TestPushTokens_SetAndAll(t *testing.T) {
	p := newPushTokens()
	p.set("dev1", "tokA")
	p.set("dev2", "tokB")
	p.set("", "ignored")
	p.set("dev3", "")
	if all := p.all(); len(all) != 2 {
		t.Fatalf("tokens = %v, want 2 (empty deviceId/token ignored)", all)
	}
}

func TestPushTokens_RemoveByToken(t *testing.T) {
	p := newPushTokens()
	p.set("dev1", "dead")
	p.set("dev2", "dead") // two devices, same (stale) token
	p.set("dev3", "live")
	p.removeByToken("dead")
	if all := p.all(); len(all) != 1 || all[0] != "live" {
		t.Fatalf("after removeByToken(dead) = %v, want [live]", all)
	}
}

func TestPushPreview_Classification(t *testing.T) {
	final := "the answer"
	cases := []struct {
		f      fap.ServerFrame
		wantOK bool
		want   string
	}{
		{fap.ServerMessage{Text: "hello"}, true, "hello"},
		{fap.TextEnd{FinalText: &final}, true, "the answer"},
		{fap.TextEnd{}, true, "New message"},
		{fap.Media{MIME: "image/png"}, true, "Sent a photo"},
		{fap.Media{MIME: "image/png", Caption: "cap"}, true, "cap"},
		{fap.Media{MIME: "application/pdf"}, true, "Sent a file"},
		// #1061: a named file previews with its filename, plus caption when present.
		{fap.Media{MIME: "application/pdf", Name: "report.pdf"}, true, "report.pdf"},
		{fap.Media{MIME: "application/pdf", Name: "report.pdf", Caption: "here you go"}, true, "report.pdf — here you go"},
		{fap.Notification{Text: "note"}, true, "note"},
		{fap.Interactive{Text: "approve?"}, true, "approve?"},
		{fap.Interactive{Questions: []fap.Question{{Text: "batched ask?"}}}, true, "batched ask?"},
		{fap.Interactive{}, true, "Question from agent"},
		{fap.Activity{Kind: "typing"}, false, ""},
		{fap.Activity{Kind: "thinking", Detail: "grep"}, false, ""},
		{fap.Meta{}, false, ""},
		{fap.TextDelta{Text: "x"}, false, ""},
	}
	for _, c := range cases {
		got, ok := pushPreview(c.f)
		if ok != c.wantOK || got != c.want {
			t.Errorf("pushPreview(%T) = (%q,%v), want (%q,%v)", c.f, got, ok, c.want, c.wantOK)
		}
	}
}

func TestTruncatePreview(t *testing.T) {
	if got := truncatePreview("  hi  "); got != "hi" {
		t.Errorf("trim = %q, want hi", got)
	}
	got := truncatePreview(strings.Repeat("a", 100))
	if !strings.HasSuffix(got, "…") || len(got) != pushPreviewMax+len("…") {
		t.Errorf("truncated len = %d, want %d with ellipsis", len(got), pushPreviewMax+len("…"))
	}
}

func TestOfflineSend_FiresPushForVisibleFramesOnly(t *testing.T) {
	var got []string
	b := &convBinding{
		convID:        "conv-1",
		seen:          make(map[string]struct{}),
		notifyOffline: func(p pushPayload) { got = append(got, p.Preview) },
	} // client nil → offline
	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m", Role: "agent", Text: "hello there"})
	b.send(fap.Activity{ConversationID: "conv-1", Kind: "typing"}) // control frame → no push
	if len(got) != 1 || got[0] != "hello there" {
		t.Fatalf("offline previews = %v, want [hello there]", got)
	}
}

func TestOnlineSend_NoPush(t *testing.T) {
	c := fakeClient()
	var got []string
	b := &convBinding{
		convID:        "c1",
		clients:       map[*wsClient]struct{}{c: {}},
		seen:          make(map[string]struct{}),
		notifyOffline: func(p pushPayload) { got = append(got, p.Preview) },
	}
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m", Role: "agent", Text: "hi"})
	drain(t, c)
	if len(got) != 0 {
		t.Fatalf("online send must not push, got %v", got)
	}
}

func TestPusher_Coalesces(t *testing.T) {
	p := &fcmPusher{tokens: newPushTokens(), window: defaultPushCoalesce, lastPush: make(map[string]time.Time)}
	p.notify(pushPayload{ConvID: "conv-1", Preview: "a"}) // no tokens → no network; updates lastPush
	first := p.lastPush["conv-1"]
	p.notify(pushPayload{ConvID: "conv-1", Preview: "b"}) // within window → coalesced, lastPush unchanged
	if !p.lastPush["conv-1"].Equal(first) {
		t.Errorf("second notify within window must be coalesced")
	}
}

// --- slice 6: multi-agent / session ---

// registerBareAgent registers an appConn without a real *agent.Agent — enough
// for PrimaryBot/roster/binding tests that don't drive a turn.
func registerBareAgent(h *Hub, agentID string) {
	h.agents[agentID] = &appConn{hub: h, agentID: agentID}
	h.agentOrder = append(h.agentOrder, agentID)
}

func TestRoster_AdvertisesNonHiddenCommands(t *testing.T) {
	h := newTestHub()
	reg := command.NewRegistry()
	reg.Register(&command.Command{Name: "pause", Description: "Pause the active question", Category: "session"})
	reg.Register(&command.Command{Name: "secret", Description: "internal", Hidden: true})
	h.agents["ag"] = &appConn{hub: h, agentID: "ag", commands: reg}
	h.agentOrder = append(h.agentOrder, "ag")

	roster := h.agentRoster()
	if len(roster) != 1 {
		t.Fatalf("roster = %d agents, want 1", len(roster))
	}
	cmds := roster[0].Commands
	if len(cmds) != 1 {
		t.Fatalf("advertised commands = %+v, want 1 (hidden excluded)", cmds)
	}
	if cmds[0].Name != "pause" || cmds[0].Description != "Pause the active question" || cmds[0].Category != "session" {
		t.Errorf("command info = %+v, want pause/description/session", cmds[0])
	}
}

func TestConversationOpen_MintsAndAdvertises(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClient()
	c.hub = h

	h.dispatchInbound(c, []byte(`{"t":"conversation.open","id":"x","d":{"agentId":"ag"}}`))

	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeHello {
		t.Fatalf("want [hello] roster reply, got %v", types(ds))
	}
	agents, _ := ds[0].d["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("roster agents = %v", ds[0].d["agents"])
	}
	convs, _ := agents[0].(map[string]any)["conversations"].([]any)
	if len(convs) != 1 {
		t.Fatalf("agent conversations = %v", agents[0])
	}
	conv := convs[0].(map[string]any)
	if conv["id"] == "" || conv["sessionKey"] == "" {
		t.Errorf("conversation info incomplete: %v", conv)
	}
	if len(h.convs) != 1 {
		t.Errorf("expected 1 durable conversation, got %d", len(h.convs))
	}
}

func TestConversationOpen_AdoptsNamedSession(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	sk, err := session.NamedIndependentSessionKey("ag", "work")
	if err != nil {
		t.Fatal(err)
	}

	h.dispatchInbound(c, []byte(`{"t":"conversation.open","id":"x","d":{"agentId":"ag","sessionKey":"`+sk+`"}}`))

	var b *convBinding
	for _, bb := range h.convs {
		b = bb
	}
	if b == nil || b.sessionKey != sk {
		t.Fatalf("adopted sessionKey = %v, want %q", b, sk)
	}
	if h.bySession[sk] != b {
		t.Errorf("bySession not repointed to the named key")
	}
}

func TestConversationOpen_ReusesExistingNamedSession(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	sk, err := session.NamedIndependentSessionKey("ag", "work")
	if err != nil {
		t.Fatal(err)
	}

	open := []byte(`{"t":"conversation.open","id":"x","d":{"agentId":"ag","sessionKey":"` + sk + `"}}`)
	h.dispatchInbound(c, open)
	if len(h.convs) != 1 {
		t.Fatalf("first open: convs = %d, want 1", len(h.convs))
	}
	var firstConvID string
	for id := range h.convs {
		firstConvID = id
	}

	// Reopening the SAME named session must reuse the conversation, not mint a
	// duplicate that races the existing binding in bySession.
	h.dispatchInbound(c, open)
	if len(h.convs) != 1 {
		t.Errorf("reopen minted a duplicate conversation: convs = %d, want 1", len(h.convs))
	}
	if _, ok := h.convs[firstConvID]; !ok {
		t.Errorf("original conversation %q was replaced", firstConvID)
	}
}

func TestServePushRegister_UpdatesToken(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev1", "")
	req := httptest.NewRequest(http.MethodPost, "/app/push/register", strings.NewReader(`{"pushToken":"fresh-tok"}`))
	req.Header.Set("Authorization", "Bearer "+d.Token) // device authenticates as itself
	w := httptest.NewRecorder()
	h.ServePushRegister(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("push register code = %d, want 204", w.Code)
	}
	if got := h.tokens.all(); len(got) != 1 || got[0] != "fresh-tok" {
		t.Errorf("token not registered: %v", got)
	}
}

func TestServeHistory_ReportsSeqHighWater(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev", "")
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1", seq: 42, seen: map[string]struct{}{}}
	h.convs["c1"] = b

	req := httptest.NewRequest(http.MethodGet, "/app/history?conversationId=c1", nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w := httptest.NewRecorder()
	h.ServeHistory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("history code = %d", w.Code)
	}
	var res struct {
		LastSeq int64 `json:"lastSeq"`
		Present bool  `json:"present"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Present || res.LastSeq != 42 {
		t.Errorf("history = %+v, want present lastSeq 42", res)
	}

	// An unknown conversation reports present=false, lastSeq 0 (server restarted
	// / never seen) rather than erroring.
	req = httptest.NewRequest(http.MethodGet, "/app/history?conversationId=ghost", nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w = httptest.NewRecorder()
	h.ServeHistory(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Present || res.LastSeq != 0 {
		t.Errorf("unknown conv = %+v, want absent lastSeq 0", res)
	}
}

func TestSink_EmitsStatusChips(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}
	s := newAppSink(b)
	s.statusFn = func() string { return "5m" }
	s.emitMeta(turnevent.TurnComplete{Model: "claude"})

	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeMeta {
		t.Fatalf("frames = %v, want [meta]", types(got))
	}
	if got[0].d["gap"] != "5m" {
		t.Errorf("meta status chips = %v", got[0].d)
	}
}

func TestSendTextWithButtons_SetsExpiresAt(t *testing.T) {
	_, c, _, conn := boundConn(t)
	_, err := conn.SendTextWithButtons("Allow?",
		[]platform.ButtonChoice{{Label: "Y", Data: "r:0"}}, "im:")
	if err != nil {
		t.Fatal(err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].d["expiresAt"] == nil || ds[0].d["expiresAt"] == "" {
		t.Errorf("interactive must carry expiresAt, got %v", ds[0].d["expiresAt"])
	}
}

func TestAdoptSession_RejectsForeignAgentKey(t *testing.T) {
	h := newTestHub()
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1", seen: map[string]struct{}{}}
	h.convs["c1"] = b
	h.bySession["ag/c1"] = b
	h.adoptSession(b, "other/iwork") // belongs to a different agent
	if b.sessionKey != "ag/c1" {
		t.Errorf("foreign-agent key must be rejected, sessionKey = %q", b.sessionKey)
	}
}

func TestCommandParts(t *testing.T) {
	if got := commandParts(command.Response{Text: "hi"}); len(got) != 1 || got[0] != "hi" {
		t.Errorf("text-only = %v", got)
	}
	if got := commandParts(command.Response{Parts: []string{"a", "b"}}); len(got) != 2 {
		t.Errorf("parts = %v", got)
	}
	if got := commandParts(command.Response{Text: "   "}); got != nil {
		t.Errorf("blank text must yield nil, got %v", got)
	}
}

func TestRouteCommand_NoCommandsErrors(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag") // bare appConn → commands registry nil
	c := fakeClient()
	c.hub = h

	h.routeCommand(c, fap.Command{ConversationID: "c1", AgentID: "ag", Name: "help"})

	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeError || ds[0].d["code"] != "no_commands" {
		t.Fatalf("want no_commands error, got %v / %v", types(ds), ds)
	}
}

// --- slice 8: voice (inbound STT) ---

type fakeSTT struct {
	text string
	err  error
}

func (f fakeSTT) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return f.text, f.err
}

func TestTranscribeVoice_MergesAndDropsVoiceAttachment(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", stt: fakeSTT{text: "hello world"}}
	atts := []platform.Attachment{
		{Type: "voice", Data: []byte("audio"), MimeType: "audio/ogg"},
		{Type: "document", Data: []byte("doc"), MimeType: "text/plain"},
	}
	text, kept := h.transcribeVoice(conn, "", atts)
	if text != "hello world" {
		t.Errorf("text = %q, want transcript", text)
	}
	if len(kept) != 1 || kept[0].Type != "document" {
		t.Errorf("voice attachment should be dropped, kept = %v", kept)
	}
}

func TestTranscribeVoice_AppendsToTypedText(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, stt: fakeSTT{text: "spoken"}}
	text, _ := h.transcribeVoice(conn, "typed", []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}})
	if text != "typed\nspoken" {
		t.Errorf("text = %q, want typed+spoken", text)
	}
}

func TestTranscribeVoice_NoSTTUnchanged(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h} // no transcriber
	atts := []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}}
	text, kept := h.transcribeVoice(conn, "x", atts)
	if text != "x" || len(kept) != 1 {
		t.Errorf("without STT must pass through: %q %v", text, kept)
	}
}

func TestTranscribeVoice_ErrorKeepsAttachment(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, stt: fakeSTT{err: errors.New("boom")}}
	atts := []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}}
	text, kept := h.transcribeVoice(conn, "", atts)
	if text != "" || len(kept) != 1 {
		t.Errorf("transcription error must keep the audio: %q %v", text, kept)
	}
}

func TestVoiceFilename(t *testing.T) {
	cases := map[string]string{
		"audio/ogg":                "voice.ogg",
		"audio/mp4":                "voice.m4a",
		"audio/mpeg":               "voice.mp3",
		"audio/wav":                "voice.wav",
		"application/octet-stream": "voice.ogg",
	}
	for mime, want := range cases {
		if got := voiceFilename(platform.Attachment{MimeType: mime}); got != want {
			t.Errorf("voiceFilename(%q) = %q, want %q", mime, got, want)
		}
	}
}

// --- slice 7: auth hardening (pairing / per-device tokens) ---

func TestDeviceStore_PairValidateRevoke(t *testing.T) {
	s := newDeviceStore("")
	d := s.pair("dev1", "Phone")
	if d.Token == "" {
		t.Fatal("no token minted")
	}
	if got, ok := s.validToken(d.Token); !ok || got.DeviceID != "dev1" {
		t.Fatal("validToken failed for minted token")
	}
	if _, ok := s.validToken("bogus"); ok {
		t.Error("bogus token accepted")
	}
	// Re-pairing replaces the token.
	d2 := s.pair("dev1", "Phone")
	if _, ok := s.validToken(d.Token); ok {
		t.Error("old token still valid after re-pair")
	}
	if _, ok := s.validToken(d2.Token); !ok {
		t.Error("new token invalid after re-pair")
	}
	// Revoke invalidates.
	if _, ok := s.revoke("dev1"); !ok {
		t.Error("revoke failed")
	}
	if _, ok := s.validToken(d2.Token); ok {
		t.Error("token valid after revoke")
	}
}

func TestDeviceStore_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	d := newDeviceStore(path).pair("dev1", "Phone")
	if _, ok := newDeviceStore(path).validToken(d.Token); !ok {
		t.Error("token did not survive store reload")
	}
}

// authToken validates per-device tokens only; there is no shared master key
// (#862). A valid device token authenticates; anything else is rejected.
func TestAuthToken_DeviceTokenOnly(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev1", "")
	if dev, ok := h.authToken(d.Token); !ok || dev.DeviceID != "dev1" {
		t.Error("device token rejected")
	}
	if _, ok := h.authToken("nope"); ok {
		t.Error("bad token accepted")
	}
	if _, ok := h.authToken(""); ok {
		t.Error("empty token accepted")
	}
}

func TestPairHTTP_PairKeyMintsDeviceToken(t *testing.T) {
	h := newTestHub()
	pk, _ := h.pairKeys.mint(time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/app/pair", strings.NewReader(`{"deviceId":"dev1","label":"Phone","pushToken":"ptok"}`))
	req.Header.Set("Authorization", "Bearer "+pk)
	w := httptest.NewRecorder()
	h.ServePair(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pair code = %d", w.Code)
	}
	var res struct {
		DeviceToken string `json:"deviceToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.DeviceToken == "" {
		t.Fatalf("pair result = %s", w.Body.String())
	}
	if _, ok := h.authToken(res.DeviceToken); !ok {
		t.Error("minted token does not authenticate")
	}
	if len(h.tokens.all()) != 1 {
		t.Error("push token not registered during pairing")
	}
}

func TestPairHTTP_DeviceTokenCannotPair(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev1", "")
	req := httptest.NewRequest(http.MethodPost, "/app/pair", strings.NewReader(`{"deviceId":"dev2"}`))
	req.Header.Set("Authorization", "Bearer "+d.Token) // a device token, not a pairing key
	w := httptest.NewRecorder()
	h.ServePair(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("device-token pair code = %d, want 403", w.Code)
	}
}

func TestServePair_AllowlistRejectsUnknownDevice(t *testing.T) {
	h := newTestHub()
	h.allowedDevices = map[string]bool{"dev-ok": true}

	// A device NOT on the allowlist is rejected (with a valid, fresh pairing key).
	pk, _ := h.pairKeys.mint(time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/app/pair", strings.NewReader(`{"deviceId":"dev-bad"}`))
	req.Header.Set("Authorization", "Bearer "+pk)
	w := httptest.NewRecorder()
	h.ServePair(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed device pair code = %d, want 403", w.Code)
	}

	// A device ON the allowlist pairs normally (fresh key — pairing keys are single-use).
	pk, _ = h.pairKeys.mint(time.Minute)
	req = httptest.NewRequest(http.MethodPost, "/app/pair", strings.NewReader(`{"deviceId":"dev-ok"}`))
	req.Header.Set("Authorization", "Bearer "+pk)
	w = httptest.NewRecorder()
	h.ServePair(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("allowed device pair code = %d, want 200", w.Code)
	}
}

func TestNewHub_AppliesPlatformConfig(t *testing.T) {
	maxMB := 7
	depth := 42
	pushOff := false
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{{
			ID: "app",
			App: &config.AppSpecific{
				Host:           "app.example.com",
				Push:           &pushOff,
				ReplayBuffer:   &depth,
				ReplayTTL:      "1h",
				MaxBlobMB:      &maxMB,
				BlobTTL:        "2h",
				PushCoalesce:   "5s",
				DevicesPath:    "custom-devices.json",
				AllowedDevices: []string{"d1", "d2"},
			},
		}},
	}
	h := newHub(platform.ProviderDeps{Config: cfg}) // Ctx nil → no reaper/pusher goroutine

	if h.host != "app.example.com" {
		t.Errorf("host = %q, want app.example.com", h.host)
	}
	if h.replayDepth != 42 {
		t.Errorf("replayDepth = %d, want 42", h.replayDepth)
	}
	if h.replayTTL != time.Hour {
		t.Errorf("replayTTL = %v, want 1h", h.replayTTL)
	}
	if h.blobs.maxBytes != int64(7)<<20 {
		t.Errorf("blob cap = %d, want %d", h.blobs.maxBytes, int64(7)<<20)
	}
	if h.blobs.ttl != 2*time.Hour {
		t.Errorf("blob ttl = %v, want 2h", h.blobs.ttl)
	}
	if !h.allowedDevices["d1"] || !h.allowedDevices["d2"] || h.allowedDevices["d3"] {
		t.Errorf("allowedDevices = %v, want {d1,d2}", h.allowedDevices)
	}
	// host should propagate into advertised caps.
	if h.caps().Host != "app.example.com" {
		t.Errorf("caps.host = %q, want app.example.com", h.caps().Host)
	}
	// A new binding inherits the configured replay depth.
	b := h.ensureBinding(fakeClientForHub(h), "ag", "conv-x")
	if b.replayDepth != 42 || b.replayTTL != time.Hour {
		t.Errorf("binding inherited depth=%d ttl=%v, want 42/1h", b.replayDepth, b.replayTTL)
	}
}

// fakeClientForHub is a fakeClient bound to h (needed because ensureBinding's
// attach registers the conversation on the socket).
func fakeClientForHub(h *Hub) *wsClient {
	c := fakeClient()
	c.hub = h
	return c
}

func TestBlobAuth_AcceptsDeviceToken(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev1", "")
	meta, _ := h.blobs.putBytes([]byte("x"), "document", "f", "text/plain")
	req := httptest.NewRequest(http.MethodGet, "/app/blob/"+meta.id, nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w := httptest.NewRecorder()
	h.ServeBlobGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("device-token blob GET code = %d", w.Code)
	}
}

func TestRevokeHTTP_InvalidatesToken(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev1", "")
	admin := h.devices.pair("admin", "") // any paired device can revoke (#862)
	req := httptest.NewRequest(http.MethodPost, "/app/pair/revoke", strings.NewReader(`{"deviceId":"dev1"}`))
	req.Header.Set("Authorization", "Bearer "+admin.Token)
	w := httptest.NewRecorder()
	h.ServeRevoke(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke code = %d", w.Code)
	}
	if _, ok := h.authToken(d.Token); ok {
		t.Error("token still valid after revoke")
	}
}

func TestAuthLimiter_Lockout(t *testing.T) {
	l := newAuthLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if l.blocked("1.2.3.4") {
			t.Fatalf("blocked after %d fails, too early", i)
		}
		l.fail("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") {
		t.Error("should be locked out after 3 failures")
	}
	l.reset("1.2.3.4")
	if l.blocked("1.2.3.4") {
		t.Error("reset should clear the lockout")
	}
}

// TestRemoteIP covers the parser cases NOT exercised by
// TestSecurity_AuthLimiter_XFFRotationDoesNotBypass (which owns the
// rightmost-hop / rotation-bypass property end-to-end): the single-entry chain
// and the no-XFF fallback to the real socket.
func TestRemoteIP(t *testing.T) {
	// Single entry (no proxy chain) → that entry.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if got := remoteIP(r); got != "8.8.8.8" {
		t.Errorf("single XFF ip = %q, want 8.8.8.8", got)
	}
	// No XFF → fall back to the real socket.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "5.5.5.5:1234"
	if got := remoteIP(r2); got != "5.5.5.5" {
		t.Errorf("RemoteAddr ip = %q, want 5.5.5.5", got)
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

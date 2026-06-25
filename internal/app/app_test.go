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

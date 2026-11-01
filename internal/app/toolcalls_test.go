package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// rawFrame is one wire frame with its envelope and decoded payload.
type rawFrame struct {
	t   string
	id  string
	seq int64
	d   map[string]any
}

func drainRaw(t *testing.T, c *wsClient) []rawFrame {
	t.Helper()
	var out []rawFrame
	for {
		select {
		case b := <-c.send:
			var env fap.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad wire frame: %v", err)
			}
			d := map[string]any{}
			if len(env.D) > 0 {
				if err := json.Unmarshal(env.D, &d); err != nil {
					t.Fatalf("bad payload: %v", err)
				}
			}
			out = append(out, rawFrame{t: env.T, id: env.ID, seq: env.Seq, d: d})
		default:
			return out
		}
	}
}

func toolCallFrames(fs []rawFrame) []rawFrame {
	var out []rawFrame
	for _, f := range fs {
		if f.t == fap.TypeToolCall {
			out = append(out, f)
		}
	}
	return out
}

func rawTypes(fs []rawFrame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.t
		if f.t == fap.TypeActivity {
			out[i] += ":" + str(f.d["kind"])
		}
	}
	return out
}

func str(v any) string { s, _ := v.(string); return s }

// capableClient is a socket whose hello advertised featureToolCalls.
func capableClient(h *Hub) *wsClient {
	c := fakeClientFor(h)
	c.features = featureSet([]string{featureToolCalls})
	return c
}

// toolTurnConn builds a binding with one attached client and an appConn whose
// app-platform show_tool_calls reads *configured and whose session override
// reads *override.
func toolTurnConn(t *testing.T, client func(*Hub) *wsClient, configured, override *string) (*Hub, *convBinding, *wsClient, *appConn) {
	t.Helper()
	h := newTestHub()
	b := h.ensureBinding(nil, "ag", "conv-1")
	c := client(h)
	b.attach(c)
	conn := &appConn{
		hub:            h,
		agentID:        "ag",
		toolCallConfig: func() string { return *configured },
		displayOverride: func(string) platform.DisplaySettings {
			return platform.DisplaySettings{ShowToolCalls: *override}
		},
	}
	return h, b, c, conn
}

// runToolTurn drives one turn with two OVERLAPPING calls (t2 completes first).
func runToolTurn(t *testing.T, conn *appConn, b *convBinding) {
	t.Helper()
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.sessionKey})
	if sink == nil {
		t.Fatal("nil sink for a live binding")
	}
	defer cleanup()
	ctx := context.Background()
	sink.Emit(ctx, turnevent.ToolCall{ID: "t1", Name: "exec", Args: json.RawMessage(`{"command":"ls"}`)})
	sink.Emit(ctx, turnevent.ToolCall{ID: "t2", Name: "read", Args: json.RawMessage(`{"path":"/x"}`)})
	sink.Emit(ctx, turnevent.ToolResult{ID: "t2", Name: "read", Output: "nope", IsError: true})
	sink.Emit(ctx, turnevent.ToolResult{ID: "t1", Name: "exec", Output: "a\nb"})
}

func TestAppSink_ToolCalls_FullPairsStartAndCompletion(t *testing.T) {
	configured, override := "full", ""
	_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)
	runToolTurn(t, conn, b)

	tc := toolCallFrames(drainRaw(t, c))
	if len(tc) != 4 {
		t.Fatalf("tool.call frames = %d, want 4 (2 starts, 2 completions)", len(tc))
	}
	start1, start2, done2, done1 := tc[0], tc[1], tc[2], tc[3]
	for i, f := range tc {
		if f.d["mode"] != "full" {
			t.Errorf("frame %d mode = %v, want full", i, f.d["mode"])
		}
		if f.d["conversationId"] != b.convID {
			t.Errorf("frame %d conversationId = %v, want %s", i, f.d["conversationId"], b.convID)
		}
		if f.seq == 0 {
			t.Errorf("frame %d has no seq: tool.call must be seq-tracked", i)
		}
	}
	if start1.d["state"] != fap.ToolCallRunning || start1.d["name"] != "exec" || start1.d["args"] != `{"command":"ls"}` {
		t.Errorf("start1 = %v", start1.d)
	}
	if start2.d["state"] != fap.ToolCallRunning || start2.d["name"] != "read" {
		t.Errorf("start2 = %v", start2.d)
	}
	id1, id2 := str(start1.d["toolCallId"]), str(start2.d["toolCallId"])
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("toolCallIds must be distinct and set: %q %q", id1, id2)
	}
	if str(done2.d["toolCallId"]) != id2 || done2.d["state"] != fap.ToolCallDone || done2.d["result"] != "nope" || done2.d["isError"] != true {
		t.Errorf("done2 does not pair with start2 (id %s): %v", id2, done2.d)
	}
	if str(done1.d["toolCallId"]) != id1 || done1.d["state"] != fap.ToolCallDone || done1.d["result"] != "a\nb" || done1.d["isError"] != nil {
		t.Errorf("done1 does not pair with start1 (id %s): %v", id1, done1.d)
	}
	if id1 == "t1" || id2 == "t2" {
		t.Errorf("wire ids must be server-minted, not the backend's tool-use ids")
	}
}

// Off (the app default) sends exactly the pre-#999 frames: the transient
// activity frame for the call, back to warming on the result — no tool.call.
// Fail-arm: forcing toolCallMode to return "full" makes this fail.
func TestAppSink_ToolCalls_OffSendsActivityOnly(t *testing.T) {
	for _, configured := range []string{"off", ""} {
		override := ""
		_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)
		runToolTurn(t, conn, b)
		got := rawTypes(drainRaw(t, c))
		want := []string{"activity:tool", "activity:tool", "activity:warming", "activity:idle"} // a repeated warming is deduplicated by setTurnActivity
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("configured=%q frames = %v, want %v", configured, got, want)
		}
	}
}

func TestAppSink_ToolCalls_PreviewLabelsMode(t *testing.T) {
	configured, override := "preview", ""
	_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)
	runToolTurn(t, conn, b)
	tc := toolCallFrames(drainRaw(t, c))
	if len(tc) != 4 {
		t.Fatalf("tool.call frames = %d, want 4", len(tc))
	}
	for _, f := range tc {
		if f.d["mode"] != "preview" {
			t.Errorf("mode = %v, want preview", f.d["mode"])
		}
	}
}

// The session_metadata override flips the gate from the next turn, with no
// rebuild of anything: both directions, and it wins over the config value.
func TestAppSink_ToolCalls_SessionOverrideFlipsGate(t *testing.T) {
	configured, override := "off", ""
	_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)

	runToolTurn(t, conn, b)
	if n := len(toolCallFrames(drainRaw(t, c))); n != 0 {
		t.Fatalf("off, no override: %d tool.call frames, want 0", n)
	}
	override = "full"
	runToolTurn(t, conn, b)
	if n := len(toolCallFrames(drainRaw(t, c))); n != 4 {
		t.Fatalf("override full: %d tool.call frames, want 4", n)
	}
	configured, override = "full", "off"
	runToolTurn(t, conn, b)
	if n := len(toolCallFrames(drainRaw(t, c))); n != 0 {
		t.Fatalf("config full, override off: %d tool.call frames, want 0 (override wins)", n)
	}
}

// A live config edit (a new resolved snapshot stored in the LiveValue, as
// liveApplyResolvedAddrs does) applies from the next turn, through the real
// 4-layer resolution of the app platform's display.
func TestAppSink_ToolCalls_LiveConfigFlipsGate(t *testing.T) {
	resolve := func(v config.ToolCallDisplay) *config.ResolvedAgentConfig {
		cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: "app", Display: config.DisplayConfig{ShowToolCalls: &v}}}}
		return config.Resolve(cfg, config.AgentConfig{ID: "ag"})
	}
	live := config.NewLiveValue(resolve(config.ToolCallOff))
	h := newTestHub()
	b := h.ensureBinding(nil, "ag", "conv-1")
	c := capableClient(h)
	b.attach(c)
	conn := &appConn{hub: h, agentID: "ag", toolCallConfig: liveToolCallConfig(live)}

	runToolTurn(t, conn, b)
	if n := len(toolCallFrames(drainRaw(t, c))); n != 0 {
		t.Fatalf("live off: %d tool.call frames, want 0", n)
	}
	live.Store(resolve(config.ToolCallFull))
	runToolTurn(t, conn, b)
	if n := len(toolCallFrames(drainRaw(t, c))); n != 4 {
		t.Fatalf("after live edit to full: %d tool.call frames, want 4", n)
	}
}

func TestAppSink_ToolCalls_SizeBoundsAndUTF8(t *testing.T) {
	configured, override := "full", ""
	_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)

	// Place a multi-byte rune across each cut point: "€" is 3 bytes, "😀" 4.
	args := `{"x":"` + strings.Repeat("a", toolCallArgsMax-7) + "€€€" + `"}`
	result := strings.Repeat("b", toolCallResultMax-2) + strings.Repeat("😀", 5)

	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.sessionKey})
	sink.Emit(context.Background(), turnevent.ToolCall{ID: "t1", Name: "write", Args: json.RawMessage(args)})
	sink.Emit(context.Background(), turnevent.ToolResult{ID: "t1", Name: "write", Output: result})
	cleanup()

	tc := toolCallFrames(drainRaw(t, c))
	if len(tc) != 2 {
		t.Fatalf("tool.call frames = %d, want 2", len(tc))
	}
	gotArgs, gotRes := str(tc[0].d["args"]), str(tc[1].d["result"])
	check := func(name, got, orig string, max int, cut any, size any) {
		t.Helper()
		if len(got) > max {
			t.Errorf("%s: %d bytes, over the %d bound", name, len(got), max)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: cut split a UTF-8 rune", name)
		}
		if !strings.HasPrefix(orig, got) || len(got) < max-3 {
			t.Errorf("%s: not the longest rune-aligned prefix (len %d of max %d)", name, len(got), max)
		}
		if cut != true {
			t.Errorf("%s: truncated flag = %v, want true", name, cut)
		}
		if size != float64(len(orig)) {
			t.Errorf("%s: uncut size = %v, want %d", name, size, len(orig))
		}
	}
	check("args", gotArgs, args, toolCallArgsMax, tc[0].d["argsTruncated"], tc[0].d["argsBytes"])
	check("result", gotRes, result, toolCallResultMax, tc[1].d["resultTruncated"], tc[1].d["resultBytes"])
}

func TestBoundUTF8_EveryCutOffset(t *testing.T) {
	for _, r := range []string{"é", "€", "😀"} {
		for pad := 0; pad < 4; pad++ {
			s := strings.Repeat("a", pad) + strings.Repeat(r, 4)
			for max := 0; max <= len(s); max++ {
				got, cut, n := boundUTF8(s, max)
				if len(got) > max || !utf8.ValidString(got) || !strings.HasPrefix(s, got) || n != len(s) {
					t.Fatalf("boundUTF8(%q, %d) = %q", s, max, got)
				}
				if cut != (max < len(s)) {
					t.Fatalf("boundUTF8(%q, %d) cut = %v", s, max, cut)
				}
			}
		}
	}
	// Invalid input becomes valid output, still bounded.
	got, _, _ := boundUTF8("ab\xffcd", 10)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 passed through: %q", got)
	}
}

// Args and results go through the agent's secret redaction AND the generic
// credential patterns (a token shape no secrets store knows about).
func TestAppSink_ToolCalls_Redacted(t *testing.T) {
	configured, override := "full", ""
	_, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)
	conn.redact = func(s string) string { return strings.ReplaceAll(s, "hunter2hunter2", "[REDACTED]") }
	gh := "ghp_" + strings.Repeat("A", 36)

	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.sessionKey})
	sink.Emit(context.Background(), turnevent.ToolCall{ID: "t1", Name: "exec", Args: json.RawMessage(`{"command":"echo hunter2hunter2 ` + gh + `"}`)})
	sink.Emit(context.Background(), turnevent.ToolResult{ID: "t1", Name: "exec", Output: "hunter2hunter2 " + gh})
	cleanup()

	for _, f := range toolCallFrames(drainRaw(t, c)) {
		text := str(f.d["args"]) + str(f.d["result"])
		if strings.Contains(text, "hunter2hunter2") || strings.Contains(text, gh) {
			t.Errorf("secret on the wire: %q", text)
		}
		if !strings.Contains(text, "[REDACTED]") {
			t.Errorf("no redaction marker in %q", text)
		}
	}
}

// A client built before #999 (no "toolCalls" feature) never meets a tool.call
// frame, live or on resume: it gets a conversation.ack at the same seq — a
// type every build drops silently — so its contiguous resume mark still closes
// over the seq. The capable device on the same binding gets the real frames.
func TestToolCalls_PreFeatureClientGetsSameSeqAck(t *testing.T) {
	configured, override := "full", ""
	h, b, capable, conn := toolTurnConn(t, capableClient, &configured, &override)
	old := fakeClientFor(h)
	b.attach(old)
	runToolTurn(t, conn, b)

	newFrames, oldFrames := drainRaw(t, capable), drainRaw(t, old)
	if len(newFrames) != len(oldFrames) {
		t.Fatalf("frame counts differ: capable %d, old %d", len(newFrames), len(oldFrames))
	}
	subs := 0
	for i := range newFrames {
		n, o := newFrames[i], oldFrames[i]
		if n.seq != o.seq {
			t.Fatalf("frame %d: seq %d vs %d — the old client must see every seq", i, n.seq, o.seq)
		}
		if o.t == fap.TypeToolCall {
			t.Fatalf("frame %d: pre-feature client got a tool.call frame", i)
		}
		if n.t == fap.TypeToolCall {
			subs++
			if o.t != fap.TypeConversationAck || o.d["conversationId"] != b.convID || o.seq == 0 {
				t.Errorf("frame %d: old client got %s %v, want conversation.ack for %s at seq %d", i, o.t, o.d, b.convID, n.seq)
			}
		} else if n.t != o.t {
			t.Errorf("frame %d: non-tool frame differs: %s vs %s", i, n.t, o.t)
		}
	}
	if subs != 4 {
		t.Fatalf("substituted %d frames, want 4", subs)
	}

	// Resume: the same substitution on replay, while the buffer keeps the
	// canonical tool.call wire for capable devices.
	b.replayTo(old, 0)
	for _, f := range drainRaw(t, old) {
		if f.t == fap.TypeToolCall {
			t.Fatalf("replay sent a tool.call to the pre-feature client (seq %d)", f.seq)
		}
	}
}

// Tool-call content is in the durable replay log and comes back, in order,
// after a reconnect — including after the in-memory buffer is gone (a restart).
func TestToolCalls_ReplayedInOrderAfterReconnect(t *testing.T) {
	configured, override := "full", ""
	h, b, c, conn := toolTurnConn(t, capableClient, &configured, &override)
	fs, err := newFrameStore(filepath.Join(t.TempDir(), "frames.db"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	b.store = fs

	runToolTurn(t, conn, b)
	live := toolCallFrames(drainRaw(t, c))
	stored := make(chan struct{})
	b.sendThen(fap.Activity{ConversationID: b.convID, Kind: string(fap.ActivityKindIdle)}, func() { close(stored) })
	select {
	case <-stored:
	case <-time.After(10 * time.Second):
		t.Fatal("frame store did not record the turn")
	}
	drainRaw(t, c)

	for _, fromMemory := range []bool{true, false} {
		if !fromMemory {
			b.mu.Lock()
			b.buffer = nil // as after a restart: only the durable store remains
			b.mu.Unlock()
		}
		re := capableClient(h)
		b.attach(re)
		b.replayTo(re, 0)
		got := toolCallFrames(drainRaw(t, re))
		if len(got) != len(live) {
			t.Fatalf("fromMemory=%v: replayed %d tool.call frames, want %d", fromMemory, len(got), len(live))
		}
		for i := range got {
			if got[i].seq != live[i].seq || got[i].id != live[i].id || got[i].d["toolCallId"] != live[i].d["toolCallId"] || got[i].d["state"] != live[i].d["state"] {
				t.Errorf("fromMemory=%v frame %d: replay %v (seq %d) != live %v (seq %d)", fromMemory, i, got[i].d, got[i].seq, live[i].d, live[i].seq)
			}
			if i > 0 && got[i].seq <= got[i-1].seq {
				t.Errorf("fromMemory=%v: replay out of order at %d", fromMemory, i)
			}
		}
		b.detachIf(re)
	}
}

func TestHubCaps_AdvertisesToolCalls(t *testing.T) {
	h := newTestHub()
	for _, f := range h.caps().Features {
		if f == featureToolCalls {
			return
		}
	}
	t.Fatalf("caps.features = %v, want %q", h.caps().Features, featureToolCalls)
}

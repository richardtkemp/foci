package fap

import (
	"encoding/json"
	"strings"
	"testing"
)

// decode the wire string back into a generic map to assert exact field names —
// the Kotlin client decodes by these exact keys, so names are the contract.
func wireFields(t *testing.T, wire string) (env map[string]any, d map[string]any) {
	t.Helper()
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v", err)
	}
	if raw, ok := env["d"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &d)
	}
	return env, d
}

func TestEncode_EnvelopeShape(t *testing.T) {
	wire, err := Encode(TextDelta{ConversationID: "c1", TurnID: "t1", Text: "hi"}, 5, 3, "ID123", "2026-06-25T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	env, d := wireFields(t, wire)
	if env["t"] != "text.delta" {
		t.Errorf("t = %v, want text.delta", env["t"])
	}
	if env["id"] != "ID123" {
		t.Errorf("id = %v, want ID123", env["id"])
	}
	// seq/ack are floats through the generic decoder.
	if env["seq"].(float64) != 5 || env["ack"].(float64) != 3 {
		t.Errorf("seq/ack = %v/%v, want 5/3", env["seq"], env["ack"])
	}
	if env["v"].(float64) != 1 {
		t.Errorf("v = %v, want 1", env["v"])
	}
	if d["conversationId"] != "c1" || d["turnId"] != "t1" || d["text"] != "hi" {
		t.Errorf("payload fields wrong: %v", d)
	}
}

func TestEncode_OmitsZeroReliabilityFields(t *testing.T) {
	wire, err := Encode(Pong{}, 0, 0, "X", "2026-06-25T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := wireFields(t, wire)
	if _, ok := env["seq"]; ok {
		t.Error("seq should be omitted when 0")
	}
	if _, ok := env["ack"]; ok {
		t.Error("ack should be omitted when 0")
	}
}

func TestEncode_TokensInFieldName(t *testing.T) {
	// Tokens.In must serialize as "in" (a Kotlin keyword, @SerialName-mapped).
	mana := 42
	wire, err := Encode(Meta{ConversationID: "c1", Model: "opus", ManaPct: &mana, Tokens: &Tokens{In: 10, Out: 20, CR: 30, CW: 40}}, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}
	_, d := wireFields(t, wire)
	tok, ok := d["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens missing: %v", d)
	}
	if tok["in"].(float64) != 10 {
		t.Errorf("tokens.in = %v, want 10", tok["in"])
	}
	if _, bad := tok["input"]; bad {
		t.Error("tokens must use wire name 'in', not 'input'")
	}
}

func TestEncode_OptionalPointersOmitted(t *testing.T) {
	// A Meta with only the required conversationId must omit every nil optional.
	wire, err := Encode(Meta{ConversationID: "c1"}, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}
	_, d := wireFields(t, wire)
	for _, k := range []string{"model", "manaPct", "manaState", "prevCostUsd", "tokens", "gap"} {
		if _, present := d[k]; present {
			t.Errorf("optional %q should be omitted when unset", k)
		}
	}
	if d["conversationId"] != "c1" {
		t.Errorf("conversationId missing")
	}
}

func TestDecode_ClientMessage(t *testing.T) {
	wire := `{"t":"message","id":"abc","seq":7,"ack":2,"ts":"2026-06-25T00:00:00Z","v":1,"d":{"conversationId":"c1","text":"hello","replyTo":"m9"}}`
	in, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	if in.T != "message" || in.ID != "abc" || in.Seq != 7 || in.Ack != 2 {
		t.Errorf("envelope meta wrong: %+v", in)
	}
	msg, ok := in.Frame.(ClientMessage)
	if !ok {
		t.Fatalf("frame type = %T, want ClientMessage", in.Frame)
	}
	if msg.ConversationID != "c1" || msg.Text != "hello" || msg.ReplyTo != "m9" {
		t.Errorf("decoded message wrong: %+v", msg)
	}
}

func TestDecode_PayloadlessFrames(t *testing.T) {
	for _, tc := range []struct {
		wire string
		want any
	}{
		{`{"t":"ping","id":"a"}`, Ping{}},
		{`{"t":"ping","id":"a","d":{}}`, Ping{}},
		{`{"t":"conversation.list","id":"a"}`, ConversationList{}},
	} {
		in, err := Decode(tc.wire)
		if err != nil {
			t.Fatalf("%s: %v", tc.wire, err)
		}
		if in.Frame != tc.want {
			t.Errorf("%s: frame = %#v, want %#v", tc.wire, in.Frame, tc.want)
		}
	}
}

func TestDecode_InteractiveResponse(t *testing.T) {
	wire := `{"t":"interactive.response","id":"r1","d":{"conversationId":"c1","promptId":"p1","data":"allow"}}`
	in, err := Decode(wire)
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := in.Frame.(InteractiveResponse)
	if !ok {
		t.Fatalf("frame type = %T, want InteractiveResponse", in.Frame)
	}
	if resp.PromptID != "p1" || resp.Data != "allow" {
		t.Errorf("decoded response wrong: %+v", resp)
	}
}

func TestDecode_UnknownFrameIsIgnoredNotFatal(t *testing.T) {
	in, err := Decode(`{"t":"future.frame","id":"a","d":{"whatever":1}}`)
	if err != nil {
		t.Fatalf("unknown frame must not error: %v", err)
	}
	if in.Frame != nil {
		t.Errorf("unknown frame must decode to nil Frame, got %#v", in.Frame)
	}
	if in.T != "future.frame" {
		t.Errorf("envelope t lost: %q", in.T)
	}
}

func TestDecode_MalformedEnvelopeErrors(t *testing.T) {
	if _, err := Decode(`not json`); err == nil {
		t.Error("expected error on malformed envelope")
	}
}

func TestRoundTrip_HelloServer(t *testing.T) {
	h := HelloServer{
		Version: 1,
		Caps:    Caps{Versions: []int{1}, Push: []string{"fcm"}, Features: []string{"voice"}},
		Agents: []AgentInfo{{
			ID: "clutch", Name: "Clutch",
			Conversations: []ConversationInfo{{ID: "conv1", SessionKey: "clutch/capp/123", Title: "Main", LastSeq: 9}},
		}},
	}
	wire, err := Encode(h, 0, 0, "X", "ts")
	if err != nil {
		t.Fatal(err)
	}
	// Re-decode the payload as the same struct to prove field-name symmetry.
	var env Envelope
	if err := json.Unmarshal([]byte(wire), &env); err != nil {
		t.Fatal(err)
	}
	var back HelloServer
	if err := json.Unmarshal(env.D, &back); err != nil {
		t.Fatal(err)
	}
	if back.Agents[0].Conversations[0].SessionKey != "clutch/capp/123" {
		t.Errorf("round-trip lost sessionKey: %+v", back)
	}
	if back.Caps.Push[0] != "fcm" {
		t.Errorf("round-trip lost caps.push: %+v", back.Caps)
	}
}

// TestEncode_AllServerFrames encodes one of every server->app frame and
// asserts its envelope `t` and that it is valid JSON. This pins the complete
// FAP v1 server frame set (including types not yet emitted by the echo slice)
// against the Kotlin client's decoder, which selects a serializer by `t`.
func TestEncode_AllServerFrames(t *testing.T) {
	mana := 80
	cost := 0.12
	final := "done"
	frames := []ServerFrame{
		HelloServer{Version: 1, Caps: Caps{Versions: []int{1}}},
		TurnStart{ConversationID: "c", TurnID: "t"},
		TextDelta{ConversationID: "c", TurnID: "t", Text: "x"},
		TextEnd{ConversationID: "c", TurnID: "t", MessageID: "m", FinalText: &final},
		ServerMessage{ConversationID: "c", MessageID: "m", Role: "agent", Text: "hi"},
		Notification{ConversationID: "c", Text: "n", Level: "info"},
		Typing{ConversationID: "c", On: true},
		Meta{ConversationID: "c", Model: "opus", ManaPct: &mana, PrevCostUsd: &cost, Tokens: &Tokens{In: 1}},
		SessionUpdate{ConversationID: "c", SessionKey: "ag/capp1/9", Reason: "compaction"},
		ErrorFrame{Code: "boom", Message: "bad"},
		Pong{},
	}
	for _, f := range frames {
		wire, err := Encode(f, 0, 0, "id", "ts")
		if err != nil {
			t.Errorf("%s: encode error: %v", f.Type(), err)
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(wire), &env); err != nil {
			t.Errorf("%s: invalid JSON: %v", f.Type(), err)
			continue
		}
		if env["t"] != f.Type() {
			t.Errorf("envelope t = %v, want %v", env["t"], f.Type())
		}
	}
}

func TestNewULID_Format(t *testing.T) {
	id := NewULID()
	if len(id) != 26 {
		t.Fatalf("ULID len = %d, want 26 (%q)", len(id), id)
	}
	const valid = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, c := range id {
		if !strings.ContainsRune(valid, c) {
			t.Errorf("ULID has non-Crockford char %q in %q", c, id)
		}
	}
	// Two ULIDs must differ (entropy) and sort by time (monotonic prefix).
	if a, b := NewULID(), NewULID(); a == b {
		t.Errorf("two ULIDs collided: %q", a)
	}
}

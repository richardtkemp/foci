package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"foci/internal/delegator"
)

// Delivery tracking (#2050) unit tests. Transcript fixture lines are copied
// from real claude-code 2.1.280 transcripts captured by the #2050 probe
// (/tmp/probe2050, clutch notes/2050.md §2), trimmed of fields foci ignores.
const (
	fixtureFoldedAttachment = `{"parentUuid":"283155a6-b704-425d-8d06-2f64540fb919","isSidechain":false,"attachment":{"type":"queued_command","prompt":"Steer: the codeword is QUOKKA-3368. Include it in your final reply.","source_uuid":"4b6093ea-2bc4-4fa5-a6bc-61e98b541266","commandMode":"prompt","timestamp":"2026-09-28T13:49:31.479Z"},"type":"attachment","uuid":"ed7181d6-cc46-47d4-9f2d-f27b6ba57f2e","sessionId":"165dfc52-89d9-497e-a0e6-7391add8edd1","version":"2.1.280"}`
	fixtureUserRecord       = `{"type":"user","uuid":"f555218f-e91e-49ff-9c22-18ac7b6eac08","message":{"role":"user","content":"Reply with just the word ONE."},"sessionId":"e3cc16cd-8a40-4d65-b0f4-b05817abd8f6"}`
	// queue-operation records are written on receipt, before any fold, and
	// carry no uuid; one naming an id only in its content proves nothing.
	fixtureQueueOperation = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T13:51:42.188Z","sessionId":"094812b4-2909-4e16-b988-0ddd81bdbcc8","content":"mentions 9d0a0e12-0ffc-4f52-8d23-9c723c7c5088"}`
	// An assistant record whose TEXT names an id is not a fold either.
	fixtureAssistantMention = `{"type":"assistant","uuid":"aaaaaaaa-0000-0000-0000-000000000000","message":{"content":[{"type":"text","text":"id 7e1baad8-9adc-4439-942f-4c456ef04ef8"}]}}`
)

// hookRecorder captures DeliveryHooks calls.
type hookRecorder struct {
	mu          sync.Mutex
	pending     []delegator.PendingInput
	consumed    []delegator.PendingInput
	undelivered [][]delegator.PendingInput
	dropped     []delegator.PendingInput
}

func (r *hookRecorder) hooks() delegator.DeliveryHooks {
	return delegator.DeliveryHooks{
		OnPending: func(p delegator.PendingInput) {
			r.mu.Lock()
			r.pending = append(r.pending, p)
			r.mu.Unlock()
		},
		OnConsumed: func(p delegator.PendingInput) {
			r.mu.Lock()
			r.consumed = append(r.consumed, p)
			r.mu.Unlock()
		},
		OnUndelivered: func(ps []delegator.PendingInput) {
			r.mu.Lock()
			r.undelivered = append(r.undelivered, ps)
			r.mu.Unlock()
		},
		OnDropped: func(p delegator.PendingInput) {
			r.mu.Lock()
			r.dropped = append(r.dropped, p)
			r.mu.Unlock()
		},
	}
}

func ids(ps []delegator.PendingInput) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func writeSessionTranscript(t *testing.T, workDir, sessionID string, lines ...string) {
	t.Helper()
	path, err := ccTranscriptPath(workDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptInputs_MatchesBothFoldShapesOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := strings.Join([]string{fixtureQueueOperation, fixtureFoldedAttachment, fixtureAssistantMention, fixtureUserRecord}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"4b6093ea-2bc4-4fa5-a6bc-61e98b541266": true,  // attachment queued_command source_uuid
		"f555218f-e91e-49ff-9c22-18ac7b6eac08": true,  // type:user uuid
		"9d0a0e12-0ffc-4f52-8d23-9c723c7c5088": false, // only in a queue-operation's content
		"7e1baad8-9adc-4439-942f-4c456ef04ef8": false, // only in assistant text
		"aaaaaaaa-0000-0000-0000-000000000000": false, // an assistant record's own uuid
	}
	var q []string
	for id := range want {
		q = append(q, id)
	}
	got, err := transcriptInputs(path, q)
	if err != nil {
		t.Fatal(err)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("transcriptInputs[%s] = %v, want %v", id, got[id], w)
		}
	}
}

func TestTranscriptInputs_MissingFileIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := transcriptInputs(filepath.Join(t.TempDir(), "nope.jsonl"), []string{"x"}); err == nil {
		t.Fatal("want an error for a missing transcript (caller then redelivers at-least-once)")
	}
}

// The uuid foci writes is the one it tracks, and the id a redelivery passes is
// reused rather than replaced.
func TestInject_WritesTrackedUUID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())

	refs := []delegator.InputRef{{ConversationID: "c1", MessageID: "m1"}}
	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser, Text: "hi", Turn: &delegator.TurnEvents{}, Refs: refs, ID: "fixed-id", Redeliveries: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &wire); err != nil {
		t.Fatal(err)
	}
	if wire["uuid"] != "fixed-id" {
		t.Fatalf("wire uuid = %v, want the inject's id", wire["uuid"])
	}
	if len(rec.pending) != 1 || rec.pending[0].ID != "fixed-id" || rec.pending[0].Redeliveries != 2 ||
		len(rec.pending[0].Refs) != 1 || rec.pending[0].Refs[0] != refs[0] || rec.pending[0].Text != "hi" {
		t.Fatalf("OnPending = %+v, want the written input with its refs and redelivery count", rec.pending)
	}

	// A fresh fold mints a new uuid, distinct from the turn's.
	buf.Reset()
	if err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceSteer, Text: "steer"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &wire); err != nil {
		t.Fatal(err)
	}
	if u, _ := wire["uuid"].(string); u == "" || u == "fixed-id" || u != rec.pending[1].ID {
		t.Fatalf("steer wire uuid = %q, tracked %q — want a fresh uuid that matches the tracked one", u, rec.pending[1].ID)
	}
	if wire["priority"] != "next" {
		t.Fatalf("steer priority = %v, want next", wire["priority"])
	}
}

// Slash commands are commands to CC, not conversation input: untracked.
func TestInject_SlashCommandsUntracked(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())
	if err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourcePass, Text: "/context"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.pending) != 0 || len(b.pendingInputs) != 0 {
		t.Fatalf("slash command was tracked: %+v", rec.pending)
	}
}

func TestOnInputAck_RemovesAndReportsConsumed(t *testing.T) {
	t.Parallel()
	b := newTestBackend(&bytes.Buffer{})
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())
	for _, id := range []string{"a", "b", "c"} {
		if err := b.trackWrite(trackedInput{id: id, text: id}, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	b.OnInputAck([]string{"b", "unknown"})
	if got := ids(rec.consumed); len(got) != 1 || got[0] != "b" {
		t.Fatalf("consumed = %v, want [b]", got)
	}
	if got := ids(b.pendingInputs); strings.Join(got, ",") != "a,c" {
		t.Fatalf("pending = %v, want [a c] in write order", got)
	}
	b.OnInputAck([]string{"b"}) // a repeat ack is a no-op
	if len(rec.consumed) != 1 {
		t.Fatalf("repeat ack re-reported: %v", ids(rec.consumed))
	}
}

func TestRegisterInput_RefusedAfterFinalize(t *testing.T) {
	t.Parallel()
	b := newTestBackend(&bytes.Buffer{})
	b.turnMu.Lock()
	b.inputsClosed = true
	b.turnMu.Unlock()
	wrote := false
	err := b.trackWrite(trackedInput{text: "late"}, func(string) error { wrote = true; return nil })
	if !errors.Is(err, delegator.ErrBackendClosed) || wrote {
		t.Fatalf("err=%v wrote=%v, want ErrBackendClosed and no write", err, wrote)
	}
	// A steer refused this way re-routes through the inbox's failed-dispatch
	// path; a begin-turn never touches turn state.
	if err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceUser, Text: "x", Turn: &delegator.TurnEvents{}}); !errors.Is(err, delegator.ErrBackendClosed) {
		t.Fatalf("begin-turn err = %v, want ErrBackendClosed", err)
	}
	if b.IsTurnInFlight() {
		t.Fatal("a refused begin-turn left a turn in flight")
	}
}

// A write that fails because the process is gone stays pending for finalize to
// hand back, and reports success so the message has one owner. Any other
// failure is the caller's: dropped from tracking, error returned.
func TestWriteInput_FailureOwnership(t *testing.T) {
	t.Parallel()
	b := newTestBackend(&bytes.Buffer{})
	b.cmd = &exec.Cmd{} // a started process: finalize is guaranteed to run
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())

	if err := b.trackWrite(trackedInput{id: "gone"}, func(string) error { return syscall.EPIPE }); err != nil {
		t.Fatalf("transport-gone write returned %v, want nil (finalize owns it)", err)
	}
	if got := ids(b.pendingInputs); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("pending = %v, want [gone]", got)
	}

	boom := errors.New("encode failed")
	if err := b.trackWrite(trackedInput{id: "bad"}, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := ids(b.pendingInputs); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("pending = %v, want [gone] (failed write dropped)", got)
	}
	if got := ids(rec.dropped); len(got) != 1 || got[0] != "bad" {
		t.Fatalf("dropped = %v, want [bad]", got)
	}
}

// Not parallel: the transcript lives under $HOME.
func TestHandBackPending_TranscriptDecides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := "/work/agent"
	b := newTestBackend(&bytes.Buffer{})
	b.workDir = workDir
	b.sessionID = "sess-1"
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())

	// folded-a is in the transcript as a queued_command attachment, folded-b
	// as a user record; lost-* never made it; capped has used its redeliveries.
	writeSessionTranscript(t, workDir, "sess-1",
		`{"type":"queue-operation","operation":"enqueue","content":"lost-1"}`,
		`{"type":"attachment","attachment":{"type":"queued_command","source_uuid":"folded-a"}}`,
		`{"type":"user","uuid":"folded-b"}`)
	for _, in := range []trackedInput{
		{id: "lost-1"}, {id: "folded-a"}, {id: "lost-2", redeliveries: 1}, {id: "folded-b"},
		{id: "capped", redeliveries: delegator.MaxRedeliveries},
	} {
		if err := b.trackWrite(in, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	b.handBackPending()

	if got := strings.Join(ids(rec.consumed), ","); got != "folded-a,folded-b" {
		t.Errorf("consumed = %s, want folded-a,folded-b", got)
	}
	if len(rec.undelivered) != 1 {
		t.Fatalf("OnUndelivered calls = %d, want 1", len(rec.undelivered))
	}
	und := rec.undelivered[0]
	if got := strings.Join(ids(und), ","); got != "lost-1,lost-2" {
		t.Errorf("undelivered = %s, want lost-1,lost-2 in write order", got)
	}
	if und[0].Redeliveries != 1 || und[1].Redeliveries != 2 {
		t.Errorf("redelivery counts = %d,%d, want 1,2 (bumped)", und[0].Redeliveries, und[1].Redeliveries)
	}
	if got := ids(rec.dropped); len(got) != 1 || got[0] != "capped" {
		t.Errorf("dropped = %v, want [capped]", got)
	}
	if len(b.pendingInputs) != 0 {
		t.Errorf("pending not cleared: %v", ids(b.pendingInputs))
	}
}

// With no readable transcript every unconsumed input is redelivered:
// at-least-once beats at-most-once.
func TestHandBackPending_NoTranscriptRedeliversAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := newTestBackend(&bytes.Buffer{})
	b.workDir = "/work/agent"
	b.sessionID = "sess-missing"
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())
	for _, id := range []string{"x", "y"} {
		_ = b.trackWrite(trackedInput{id: id}, func(string) error { return nil })
	}
	b.handBackPending()
	if len(rec.undelivered) != 1 || strings.Join(ids(rec.undelivered[0]), ",") != "x,y" {
		t.Fatalf("undelivered = %v, want [[x y]]", rec.undelivered)
	}
}

// At idle nothing is handed back — CC can legitimately hold a line written as
// the run ended — but inputs the transcript already holds are marked consumed.
func TestSweepPendingAtIdle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := newTestBackend(&bytes.Buffer{})
	b.workDir = "/work/agent"
	b.sessionID = "sess-2"
	rec := &hookRecorder{}
	b.SetDeliveryHooks(rec.hooks())
	writeSessionTranscript(t, b.workDir, "sess-2", `{"type":"user","uuid":"in-transcript"}`)
	for _, id := range []string{"in-transcript", "still-queued"} {
		_ = b.trackWrite(trackedInput{id: id}, func(string) error { return nil })
	}
	b.onSessionIdle()
	if got := ids(rec.consumed); len(got) != 1 || got[0] != "in-transcript" {
		t.Fatalf("consumed = %v, want [in-transcript]", got)
	}
	if len(rec.undelivered) != 0 {
		t.Fatalf("idle handed back %v, want nothing", rec.undelivered)
	}
	if got := ids(b.pendingInputs); len(got) != 1 || got[0] != "still-queued" {
		t.Fatalf("pending = %v, want [still-queued]", got)
	}
}

// finalizeExit closes inputs before it hands back, and hands back before it
// completes the turn — so redeliveries queue ahead of anything that follows.
func TestFinalizeExit_HandsBackBeforeTurnComplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := newTestBackend(&bytes.Buffer{})
	b.workDir = "/work/agent"
	b.sessionID = "sess-3"
	var order []string
	h := delegator.DeliveryHooks{OnUndelivered: func(ps []delegator.PendingInput) { order = append(order, "handback:"+strings.Join(ids(ps), ",")) }}
	b.SetDeliveryHooks(h)
	turn := &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) { order = append(order, "turn-complete") }}
	if err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceUser, Text: "p", Turn: turn, ID: "primary"}); err != nil {
		t.Fatal(err)
	}
	b.finalizeExit(errors.New("exit status 1"))
	if got := strings.Join(order, " "); got != "handback:primary turn-complete" {
		t.Fatalf("order = %q, want handback before turn-complete", got)
	}
	if err := b.trackWrite(trackedInput{text: "after"}, func(string) error { return nil }); !errors.Is(err, delegator.ErrBackendClosed) {
		t.Fatalf("write after finalize: %v, want refused", err)
	}
}

type ackHandler struct {
	mockHandler
	acks [][]string
}

func (h *ackHandler) OnInputAck(ids []string) { h.acks = append(h.acks, ids) }

func TestReader_DispatchesInputAck(t *testing.T) {
	t.Parallel()
	lines := strings.Join([]string{
		// message_start of the response that consumed U1 (real 2.1.280 shape, trimmed)
		`{"type":"stream_event","event":{"type":"message_start"},"user_message_uuids":["U1"],"user_message_uuid":"U1","session_id":"s"}`,
		// singular field only
		`{"type":"system","subtype":"thinking_tokens","user_message_uuid":"U2"}`,
		// a record that only MENTIONS the field name in its text is not an ack
		`{"type":"assistant","message":{"content":[{"type":"text","text":"see \"user_message_uuids\": [\"U3\"]"}]}}`,
	}, "\n") + "\n"
	h := &ackHandler{}
	NewReader(strings.NewReader(lines), h).Run(context.Background())
	if len(h.acks) != 2 || strings.Join(h.acks[0], ",") != "U1" || strings.Join(h.acks[1], ",") != "U2" {
		t.Fatalf("acks = %v, want [[U1] [U2]]", h.acks)
	}
	if len(h.streamEvents) != 1 || len(h.assistants) != 1 {
		t.Fatalf("ack detection swallowed normal dispatch: streams=%d assistants=%d", len(h.streamEvents), len(h.assistants))
	}
}

package ccstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
)

// The two frames CC 2.1.314 streamed in the #3002 probe (a throwaway
// CLAUDE_CONFIG_DIR whose OAuth token had expired and could not refresh),
// verbatim but for the usage object, trimmed to its counted keys.
//
// compactAuthFrame: a /compact. CC writes NO assistant line for it: the
// transcript gets a system local_command record (same uuid) instead. It has no
// top-level "error" field.
//
// turnAuthFrame: an ordinary turn. CC writes its assistant line, with this id.
const (
	compactAuthFrame = `{"type":"assistant","message":{"id":"7210d536-c711-4e10-a697-444cccff1d51","model":"<synthetic>","role":"assistant","stop_reason":"end_turn","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"Error during compaction: Failed to authenticate: OAuth session expired and could not be refreshed"}]},"parent_tool_use_id":null,"local_command_source":"<local-command-stderr>Error during compaction: Failed to authenticate: OAuth session expired and could not be refreshed</local-command-stderr>","session_id":"136647be-ed58-4268-8127-b1adfbd0e2d3","uuid":"70359313-2e18-4643-a575-1103cbb6da1c","local_command_run":{"command":"compact","args":""},"local_command_outcome":{"kind":"failed"}}`
	turnAuthFrame    = `{"type":"assistant","message":{"id":"ed27d2c0-2e36-4782-b764-9a6497b22d2a","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"Failed to authenticate: OAuth session expired and could not be refreshed"}]},"parent_tool_use_id":null,"session_id":"136647be-ed58-4268-8127-b1adfbd0e2d3","uuid":"3605dddd-996b-4f96-9414-07511b1bbe12","error":"authentication_failed","is_api_error_message":true}`
)

func decodeFrame(t *testing.T, raw string) *AssistantMessage {
	t.Helper()
	var m AssistantMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// TestAssistantAuthError: both probe frames ended in an authentication error;
// a real reply and CC's no-response placeholder did not.
func TestAssistantAuthError(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"compact frame, no error field", compactAuthFrame, true},
		{"turn frame, authentication_failed", turnAuthFrame, true},
		{"real reply", `{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"Failed to authenticate is a phrase I can say"}]}}`, false},
		{"no response placeholder", `{"type":"assistant","message":{"id":"x","model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"No response requested."}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := assistantAuthError(decodeFrame(t, tc.raw)); got != tc.want {
				t.Errorf("assistantAuthError = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthErrorCallSettlesAtZero is #3002: a quiet-hours /compact ran while
// CC's OAuth was expired. The stream named the failed attempt, CC wrote no
// assistant line for it, and the window closed with an invStreamIdBooked ERROR
// and a result "not settled within its bound". Dick's ruling (2026-11-10): a
// call that ended in an auth error is settled at zero cost, with no alarm.
func TestAuthErrorCallSettlesAtZero(t *testing.T) {
	m := decodeFrame(t, compactAuthFrame)
	mu := map[string]ModelUsage{opus: {InputTokens: 5, OutputTokens: 5}}

	t.Run("no line: booked at $0, not awaited, no alarm", func(t *testing.T) {
		tb := newTestBook(t, mu) // the remainder baseline: CC's restored counts
		tb.mainTailRunning(true)
		tb.mainRead(tb.clock)
		tb.streamNamed(m.Message.ID, m.UUID, "T1", tb.clock)
		tb.authErrored(m.Message.ID, authErrorLine(m, tb.clock))
		if n := tb.unseenNamed(); n != 0 {
			t.Errorf("unseenNamed = %d, want 0: the line never comes, a flush must not wait for it", n)
		}
		tb.result(mu, 0, "", tb.clock)
		tb.advance(ccLineBound + time.Second)
		tb.mainRead(tb.clock)
		tb.settle(false)
		if len(tb.results) != 0 {
			t.Errorf("%d result(s) pending, want the result settled", len(tb.results))
		}
		if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
			t.Errorf("%d invStreamIdBooked alarm(s), want none: the call ended in an auth error", n)
		}
		c := find(tb.calls(t), m.Message.ID)
		if c == nil {
			t.Fatalf("auth-error call not booked")
		}
		if c.finality != accounting.FinalityCompleted || c.model != "<synthetic>" || c.turn != "T1" ||
			!c.cost.Valid || c.cost.Float64 != 0 || len(c.tokens.NonZero()) != 0 {
			t.Errorf("booked %+v, want completed <synthetic> on T1 at $0 with no counts", c)
		}
		// A late line (an ordinary turn's, CC writes one) is a copy.
		tb.mainLine(line(m.Message.ID, "<synthetic>", tb.clock, "end_turn", 0, 0, 0, 0, 0))
		if n := len(tb.calls(t)); n != 1 {
			t.Errorf("%d calls after the late line, want 1", n)
		}
	})

	t.Run("line comes: booked from it, once", func(t *testing.T) {
		tm := decodeFrame(t, turnAuthFrame)
		tb := newTestBook(t, nil)
		tb.streamNamed(tm.Message.ID, tm.UUID, "T1", tb.clock)
		tb.authErrored(tm.Message.ID, authErrorLine(tm, tb.clock))
		tb.mainLine(line(tm.Message.ID, "<synthetic>", tb.clock, "stop_sequence", 0, 0, 0, 0, 0))
		tb.closeWindows(tb.window)
		calls := tb.calls(t)
		if len(calls) != 1 || find(calls, tm.Message.ID) == nil {
			t.Errorf("calls = %+v, want the one call", calls)
		}
		if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
			t.Errorf("%d invStreamIdBooked alarm(s), want none", n)
		}
	})

	t.Run("ended turn closes with only the auth-error call named", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.streamNamed(m.Message.ID, m.UUID, "T1", tb.clock)
		tb.turnEnded("T1", tb.clock)
		tb.authErrored(m.Message.ID, authErrorLine(m, tb.clock))
		if !tb.closed["T1"] {
			t.Error("T1's activity still open: it waits for a line that never comes")
		}
	})
}

// TestLedgerFlushPassesOnAuthErrorCompact is #3002 through OnAssistant and the
// real main tail: the stream names the failed /compact, the transcript gets only
// CC's system local_command record, and a turn's flush does not wait for an
// assistant line. Not parallel: it sets the bound.
func TestLedgerFlushPassesOnAuthErrorCompact(t *testing.T) {
	orig := ccBarrierBound
	ccBarrierBound = time.Minute // far past the deadline below: waiting it out fails
	t.Cleanup(func() { ccBarrierBound = orig })
	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	b.ledger.Store(lg)
	t.Cleanup(lg.close)
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lg.startMainTail(transcript, 0)
	b.OnAssistant(decodeFrame(t, compactAuthFrame))
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"system","subtype":"local_command","uuid":"70359313-2e18-4643-a575-1103cbb6da1c","content":"<local-command-stderr>Error during compaction: Failed to authenticate: OAuth session expired and could not be refreshed</local-command-stderr>","level":"info"}` + "\n")
	_ = f.Close()
	lg.poke()
	done := make(chan struct{})
	go func() { lg.flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("flush waited for the line of a call that ended in an auth error")
	}
}

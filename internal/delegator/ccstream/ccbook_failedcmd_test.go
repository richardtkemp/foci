package ccstream

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
)

// compactNetFrame is the frame CC 2.1.325 streamed in the #3211 probe: a
// /compact resumed with HTTPS_PROXY on a closed port, so every API call failed
// until its retries ran out (verbatim but for the usage object, trimmed to its
// counted keys). CC streamed no api_retry frame for it, and wrote NO assistant
// line: the transcript got only a system local_command record. The live case
// (work Mac, CC 2.1.326, 2026-11-21 02:56) was the same shape, with ENOTFOUND:
// a quiet-hours /compact during a network outage.
const compactNetFrame = `{"type":"assistant","message":{"id":"e970ab3b-368d-4a7f-af6f-07b57d2f143d","model":"<synthetic>","role":"assistant","stop_reason":"end_turn","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"Error during compaction: API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)"}]},"parent_tool_use_id":null,"local_command_source":"<local-command-stderr>Error during compaction: API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)</local-command-stderr>","session_id":"7132c71f-a75d-43cc-9002-ab6a8df71689","uuid":"d91c0568-8559-4471-9d9a-efcebc424bba","local_command_run":{"command":"compact","args":""},"local_command_outcome":{"kind":"failed"}}`

// turnNetFrame is an ordinary turn's call that exhausted its retries in the same
// probe (ECONNREFUSED after the tool result). CC writes its assistant line
// (isApiErrorMessage, stop_reason stop_sequence), so the book keeps waiting for
// it: if CC ever stops writing that line, invStreamIdBooked must still fire.
const turnNetFrame = `{"type":"assistant","message":{"id":"6bd48013-13b0-49b5-88f8-109dd784eeec","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)"}]},"parent_tool_use_id":null,"session_id":"844897db-8c3a-4dae-aa54-9241998f8903","uuid":"ff0c7394-9eb9-4eab-8c11-7b665d660e21","error":"server_error"}`

// TestAssistantFailedNoCost: an auth error and a synthetic failed local command
// are not waited for. An ordinary call's synthetic API-error frame IS waited for
// (CC writes its line), as are a real reply, a real reply carrying a failed
// outcome, a synthetic success outcome and CC's no-response placeholder.
func TestAssistantFailedNoCost(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"compact, network error", compactNetFrame, true},
		{"turn, network error: CC writes its line, still awaited", turnNetFrame, false},
		{"compact, auth error", compactAuthFrame, true},
		{"turn, auth error", turnAuthFrame, true},
		{"real reply", `{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"Error during compaction: API Error"}]}}`, false},
		{"real reply with a local command outcome", `{"type":"assistant","message":{"id":"msg_2","model":"claude-opus-5","stop_reason":"end_turn","content":[]},"local_command_outcome":{"kind":"failed"}}`, false},
		{"synthetic, local command succeeded", `{"type":"assistant","message":{"id":"x","model":"<synthetic>","stop_reason":"end_turn","content":[]},"local_command_outcome":{"kind":"success"}}`, false},
		{"no response placeholder", `{"type":"assistant","message":{"id":"x","model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"No response requested."}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := assistantFailedNoCost(decodeFrame(t, tc.raw)); got != tc.want {
				t.Errorf("assistantFailedNoCost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNetworkFailedCompactSettlesAtZero is #3211: the stream named a /compact
// that failed on a network outage, CC wrote no assistant line for it, and the
// window closed with an invStreamIdBooked ERROR. It settles at $0 with no alarm,
// as #3002 does for an auth error. A call that did not fail and whose line
// never came still alarms.
func TestNetworkFailedCompactSettlesAtZero(t *testing.T) {
	m := decodeFrame(t, compactNetFrame)
	mu := map[string]ModelUsage{opus: {InputTokens: 5, OutputTokens: 5}}

	tb := newTestBook(t, mu)
	tb.mainTailRunning(true)
	tb.mainRead(tb.clock)
	tb.streamNamed(m.Message.ID, m.UUID, "T1", tb.clock)
	tb.callFailed(m.Message.ID, failedCallLine(m, tb.clock))
	if n := tb.unseenNamed(); n != 0 {
		t.Errorf("unseenNamed = %d, want 0", n)
	}
	tb.result(mu, 0, "", tb.clock)
	tb.advance(ccLineBound + time.Second)
	tb.mainRead(tb.clock)
	tb.settle(false)
	if len(tb.results) != 0 {
		t.Errorf("%d result(s) pending, want the result settled", len(tb.results))
	}
	if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
		t.Errorf("%d invStreamIdBooked alarm(s), want none: the call failed on the network", n)
	}
	c := find(tb.calls(t), m.Message.ID)
	if c == nil {
		t.Fatalf("failed call not booked")
	}
	if c.finality != accounting.FinalityCompleted || c.model != "<synthetic>" || c.turn != "T1" ||
		!c.cost.Valid || c.cost.Float64 != 0 || len(c.tokens.NonZero()) != 0 {
		t.Errorf("booked %+v, want completed <synthetic> on T1 at $0 with no counts", c)
	}

	t.Run("an ordinary call's API-error frame whose line never comes alarms", func(t *testing.T) {
		tm := decodeFrame(t, turnNetFrame)
		tb := newTestBook(t, nil)
		tb.streamNamed(tm.Message.ID, tm.UUID, "T1", tb.clock)
		if assistantFailedNoCost(tm) { // as OnAssistant decides
			tb.callFailed(tm.Message.ID, failedCallLine(tm, tb.clock))
		}
		if n := tb.unseenNamed(); n != 1 {
			t.Errorf("unseenNamed = %d, want 1: CC writes this call's line, the book waits for it", n)
		}
		tb.closeWindows(tb.window)
		if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 1 {
			t.Errorf("%d invStreamIdBooked alarm(s), want 1: CC stopped writing an API-error line", n)
		}
	})

	t.Run("an ordinary call's API-error frame books from its line", func(t *testing.T) {
		tm := decodeFrame(t, turnNetFrame)
		tb := newTestBook(t, nil)
		tb.streamNamed(tm.Message.ID, tm.UUID, "T1", tb.clock)
		tb.mainLine(line(tm.Message.ID, "<synthetic>", tb.clock, "stop_sequence", 0, 0, 0, 0, 0))
		tb.closeWindows(tb.window)
		if c := find(tb.calls(t), tm.Message.ID); c == nil || !c.cost.Valid || c.cost.Float64 != 0 {
			t.Errorf("call = %+v, want booked at $0 from its line", c)
		}
		if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
			t.Errorf("%d invStreamIdBooked alarm(s), want none", n)
		}
	})

	t.Run("a call that did not fail still alarms", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.streamNamed("msg_lost", "u1", "T1", tb.clock)
		tb.closeWindows(tb.window)
		if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 1 {
			t.Errorf("%d invStreamIdBooked alarm(s), want 1: a real broken expectation", n)
		}
	})
}

// TestLedgerFlushPassesOnNetworkFailedCompact is #3211 through OnAssistant and
// the real main tail: the transcript gets only CC's system local_command record,
// and a flush does not wait for an assistant line. Not parallel: it sets the
// bound.
func TestLedgerFlushPassesOnNetworkFailedCompact(t *testing.T) {
	orig := ccBarrierBound
	ccBarrierBound = time.Minute
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
	b.OnAssistant(decodeFrame(t, compactNetFrame))
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"system","subtype":"local_command","uuid":"0d3e2f8e-3b5c-4f0e-9a51-5c1d1f4c2a77","content":"<local-command-stderr>Error during compaction: API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)</local-command-stderr>","level":"info"}` + "\n")
	_ = f.Close()
	lg.poke()
	done := make(chan struct{})
	go func() { lg.flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("flush waited for the line of a /compact that failed on the network")
	}
}

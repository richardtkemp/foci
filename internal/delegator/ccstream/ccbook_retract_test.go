package ccstream

import (
	"testing"
	"time"

	"foci/internal/delegator/accounting"
)

// TestForkCopyOfMainCallBooksOnMainThread is #2492's live finding on CC
// 2.1.314: a fork subagent's transcript copies ONE parent line, the main
// thread's call that spawned it (same message id), stamped at that call's own
// time. When the fork's tail reads it before the main tail does, the call used
// to book as the fork's (actor = the fork) and the main line, finding the id
// done, left it named: a spurious invStreamIdBooked at its window's close. The
// call is the main thread's; it books there, once, with no alarm.
func TestForkCopyOfMainCallBooksOnMainThread(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_parent", "u-p", "T1", tb.clock) // the main thread's Agent call
	tb.tailOpened("fork-a", "T1")
	tb.subLine("fork-a", "T1", line("msg_parent", opus, tb.clock, "tool_use", 10, 200, 0, 0, 0)) // the fork's copy, read first
	tb.subLine("fork-a", "T1", line("msg_fork", opus, tb.clock, "end_turn", 5, 8, 0, 0, 0))
	tb.tailClosed("fork-a")
	tb.mainLine(line("msg_parent", opus, tb.clock, "tool_use", 10, 200, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 15, OutputTokens: 208}}, 0, tb.clock)
	tb.advance(time.Minute)

	calls := tb.calls(t)
	p := find(calls, "msg_parent")
	if p == nil || p.actor != "" || p.finality != accounting.FinalityCompleted {
		t.Errorf("msg_parent = %+v, want booked on the main thread (actor \"\"), completed", p)
	}
	if f := find(calls, "msg_fork"); f == nil || f.actor != "fork-a" {
		t.Errorf("msg_fork = %+v, want the fork's own call, actor fork-a", f)
	}
	if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
		t.Errorf("%d invStreamIdBooked alarm(s), want none: the main line came", n)
	}
}

// TestMainLineAfterSubBookedClearsNamed: whatever booked a named call first,
// its main-transcript line coming later means the line DID come, so the window
// close raises no invStreamIdBooked for it.
func TestMainLineAfterSubBookedClearsNamed(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_x", "u-x", "T1", tb.clock)
	tb.done["msg_x"] = true // booked by another path before the main tail read it
	tb.mainLine(line("msg_x", opus, tb.clock, "end_turn", 1, 1, 0, 0, 0))
	tb.closeWindows(tb.window)
	if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
		t.Errorf("%d invStreamIdBooked alarm(s), want none", n)
	}
}

// TestTranscriptRetractionIsNotAwaited is #2492's CC 2.1.307+ probe: when the
// model answers stop_reason=tool_use with no parsable tool call, CC retries it
// (up to twice). The failed attempt streams as a normal assistant frame (so the
// stream names it) but is tombstoned: no transcript line ever carries its id.
// The record CC writes in its place (the retry prompt, or the final
// "could not be parsed" message) lists the attempt's frame uuid in
// retractedMessageUuids. The book takes that as a retraction: the call is not
// waited for and raises no invStreamIdBooked.
func TestTranscriptRetractionIsNotAwaited(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_bad", "u-bad", "T1", tb.clock)
	tb.streamNamed("msg_good", "u-good", "T1", tb.clock)
	retracted := parseCCRetraction([]byte(`{"type":"user","isMeta":true,"uuid":"u-meta","retractedMessageUuids":["u-bad"],` +
		`"message":{"role":"user","content":[{"type":"text","text":"The previous response failed to produce a valid tool call. Please retry the tool call now."}]}}`))
	tb.retract(retracted)
	tb.mainLine(line("msg_good", opus, tb.clock, "end_turn", 1000, 50, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 2000, OutputTokens: 100}}, 0, tb.clock)
	if named := tb.unseenNamed(); named != 0 {
		t.Errorf("unseenNamed = %d, want 0: the retracted attempt's line never comes", named)
	}
	tb.advance(time.Minute)
	if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
		t.Errorf("%d invStreamIdBooked alarm(s), want none: CC retracted msg_bad", n)
	}
	if g := find(tb.calls(t), "msg_good"); g == nil || g.finality != accounting.FinalityCompleted {
		t.Errorf("msg_good = %+v, want completed", g)
	}
}

func TestParseCCRetraction(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`{"type":"user","retractedMessageUuids":["a","b"]}`, []string{"a", "b"}},
		{`{"type":"assistant","message":{"id":"x"},"retractedMessageUuids":["c"]}`, []string{"c"}},
		{`{"type":"user","retractedMessageUuids":[]}`, nil},
		{`{"type":"user","message":{"content":"retractedMessageUuids"}}`, nil},
		{`not json "retractedMessageUuids"`, nil},
	} {
		got := parseCCRetraction([]byte(tc.raw))
		if len(got) != len(tc.want) {
			t.Errorf("parseCCRetraction(%s) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseCCRetraction(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		}
	}
}

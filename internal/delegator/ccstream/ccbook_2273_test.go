package ccstream

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/sqlite"
)

// #2273: run turns end in the ledger (ended_at, stop_reason), and the CC
// adapter's §8.6 / §8.7 checks — invUnseenField on a transcript usage object
// and invMainTranscriptFinal on a later main line of a booked call.

// turnRow is one stored turn's columns, NULLs as "".
type turnRow struct {
	session, agent, backend, source, started, ended, stop, activity string
}

func readTurn(t *testing.T, path, turn string) (turnRow, bool) {
	t.Helper()
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var r turnRow
	var agent, ended, stop, activity sql.NullString
	err = db.QueryRow(`SELECT session, agent_id, backend, source, started_at, ended_at, stop_reason, activity_closed_at
		FROM turns WHERE turn_id = ?`, turn).Scan(&r.session, &agent, &r.backend, &r.source, &r.started, &ended, &stop, &activity)
	if err == sql.ErrNoRows {
		return turnRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	r.agent, r.ended, r.stop, r.activity = agent.String, ended.String, stop.String, activity.String
	return r, true
}

// runTurnOf is the run turn the book minted for window w.
func (tb *testBook) runTurnOf(t *testing.T, w int) string {
	t.Helper()
	turn, ok := tb.runTurn[w]
	if !ok {
		t.Fatalf("premise: no run turn minted for window %d", w)
	}
	return turn
}

// TestRunTurnEndsAtItsResult: a run no foci turn opened books on a minted
// run turn, whose row gets ended_at at the run's result and the result's
// stop_reason. The exit that follows ends every run turn again, and must not
// move the end the result recorded.
func TestRunTurnEndsAtItsResult(t *testing.T) {
	tb := newTestBook(t, nil)
	minted := tb.clock
	tb.streamNamed("msg_r1", "", "", tb.clock)
	turn := tb.runTurnOf(t, 0)
	tb.advance(time.Second)
	tb.mainLine(line("msg_r1", opus, tb.clock, "end_turn", 10, 20, 0, 0, 0))
	tb.advance(time.Second)
	resultAt := tb.clock
	tb.result(map[string]ModelUsage{opus: {InputTokens: 10, OutputTokens: 20}}, 0, "end_turn", tb.clock)
	tb.advance(time.Minute)
	tb.exit()

	r, ok := readTurn(t, tb.path, turn)
	if !ok {
		t.Fatalf("run turn %s not stored", turn)
	}
	if r.ended != stamp(resultAt) {
		t.Errorf("ended_at = %q, want the result's time %q (not the exit's)", r.ended, stamp(resultAt))
	}
	if r.stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", r.stop)
	}
	if want := (turnRow{session: "cap/c1", agent: "cap", backend: accounting.BackendCCStream,
		source: accounting.SourceAutonomous, started: stamp(minted)}); r.session != want.session || r.agent != want.agent ||
		r.backend != want.backend || r.source != want.source || r.started != want.started {
		t.Errorf("run turn facts = %+v, want %+v", r, want)
	}
}

// TestRunTurnEndsAtExitWithoutResult: a run whose result never came ends with
// its process, and with no stop reason.
func TestRunTurnEndsAtExitWithoutResult(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_r2", "", "", tb.clock)
	turn := tb.runTurnOf(t, 0)
	tb.mainLine(line("msg_r2", opus, tb.clock, "tool_use", 10, 20, 0, 0, 0))
	tb.advance(time.Second)
	exitAt := tb.clock
	tb.exit()

	r, ok := readTurn(t, tb.path, turn)
	if !ok {
		t.Fatalf("run turn %s not stored", turn)
	}
	if r.ended != stamp(exitAt) {
		t.Errorf("ended_at = %q, want the exit's time %q", r.ended, stamp(exitAt))
	}
	if r.stop != "" {
		t.Errorf("stop_reason = %q, want NULL: no result said why the run stopped", r.stop)
	}
}

// TestRunTurnStoredBeforeAnyCall: a run turn is stored, running, as it is
// minted. One whose activity closes before any call is booked on it — its only named call was retracted (#2492), so the
// result ends and closes it at once — still has its row, with its end and
// its activity close. The close is an update of the row; on a row not yet
// stored it would be lost.
func TestRunTurnStoredBeforeAnyCall(t *testing.T) {
	tb := newTestBook(t, nil)
	minted := tb.clock
	tb.streamNamed("msg_r3", "frame-1", "", tb.clock)
	turn := tb.runTurnOf(t, 0)
	// Stored, and running, from the moment it is minted.
	if r, ok := readTurn(t, tb.path, turn); !ok || r.activity != "" || r.ended != "" || r.started != stamp(minted) {
		t.Fatalf("run turn %s at mint: stored=%v %+v, want stored, running, started at the mint", turn, ok, r)
	}
	tb.retract([]string{"frame-1"})
	tb.advance(time.Second)
	resultAt := tb.clock
	tb.result(map[string]ModelUsage{opus: {InputTokens: 10, OutputTokens: 20}}, 0, "end_turn", tb.clock)

	r, ok := readTurn(t, tb.path, turn)
	if !ok {
		t.Fatalf("run turn %s not stored though its activity closed", turn)
	}
	if r.activity != stamp(resultAt) {
		t.Errorf("activity_closed_at = %q, want %q", r.activity, stamp(resultAt))
	}
	if r.ended != stamp(resultAt) || r.stop != "end_turn" {
		t.Errorf("ended_at, stop_reason = %q, %q; want %q, end_turn", r.ended, r.stop, stamp(resultAt))
	}
	if r.source != accounting.SourceAutonomous || r.started != stamp(minted) || r.backend != accounting.BackendCCStream {
		t.Errorf("run turn facts = %+v", r)
	}
	for _, c := range tb.calls(t) {
		if c.turn == turn {
			t.Errorf("premise: call %s booked on the run turn", c.key)
		}
	}
}

// TestFociTurnEndIsNotTheBooks: a foci-opened turn's ended_at is the agent
// layer's to write; the book closes only its activity.
func TestFociTurnEndIsNotTheBooks(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.recordTurn(t, "cap/c1@1")
	tb.streamNamed("msg_f1", "", "cap/c1@1", tb.clock)
	tb.mainLine(line("msg_f1", opus, tb.clock, "end_turn", 10, 20, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 10, OutputTokens: 20}}, 0, "end_turn", tb.clock)
	tb.turnEnded("cap/c1@1", tb.clock)
	tb.exit()
	r, _ := readTurn(t, tb.path, "cap/c1@1")
	if r.ended != "" || r.stop != "" {
		t.Errorf("foci turn ended_at, stop_reason = %q, %q; want both NULL (the agent layer writes them)", r.ended, r.stop)
	}
	if r.activity == "" {
		t.Error("foci turn's activity not closed")
	}
}

// rawLine builds a main-transcript assistant line with a usage object given
// verbatim.
func rawLine(id string, at time.Time, usage string) []byte {
	return fmt.Appendf(nil, `{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"stop_reason":"end_turn","usage":%s,"content":[]}}`,
		at.Format(time.RFC3339Nano), id, opus, usage)
}

// realUsage is a usage object with every key CC 2.1.3xx writes.
const realUsage = `{"input_tokens":2,"cache_creation_input_tokens":585,"cache_read_input_tokens":125344,"output_tokens":535,` +
	`"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},` +
	`"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":585,"ephemeral_5m_input_tokens":0},` +
	`"inference_geo":"global","iterations":[{"input_tokens":2,"output_tokens":535,"cache_read_input_tokens":125344,` +
	`"cache_creation_input_tokens":585,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":585},"type":"message"}],` +
	`"speed":"standard","fallback_credit":null}`

// feedRaw parses raw as the main tail does and hands it to the book as a call
// the stream named.
func (tb *testBook) feedRaw(t *testing.T, id string, raw []byte) {
	t.Helper()
	l, _ := parseCCRecord(raw)
	if l == nil {
		t.Fatalf("premise: line %s did not parse", id)
	}
	tb.recordTurn(t, "cap/c1@1")
	tb.streamNamed(id, "", "cap/c1@1", tb.clock)
	tb.mainLine(l)
}

func (tb *testBook) unseenDetails() []string {
	var out []string
	for _, a := range tb.alarms {
		if a.Invariant == accounting.InvUnseenField {
			out = append(out, a.Detail)
		}
	}
	return out
}

// TestUnseenFieldSilentOnRealUsage: every key a real CC 2.1.3xx usage object
// carries is known, iterations included.
func TestUnseenFieldSilentOnRealUsage(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.feedRaw(t, "msg_u0", rawLine("msg_u0", tb.clock, realUsage))
	if d := tb.unseenDetails(); len(d) != 0 {
		t.Errorf("invUnseenField on a real usage object: %v", d)
	}
	if len(tb.calls(t)) != 1 {
		t.Error("premise: the call was not booked")
	}
}

// TestUnseenFieldFiresOnceForANewKey: a key CC adds — nested, here — alarms
// with its path and value, the call is still booked, and the same key on a
// later call (or a history copy) does not alarm again.
func TestUnseenFieldFiresOnceForANewKey(t *testing.T) {
	tb := newTestBook(t, nil)
	withNew := strings.Replace(realUsage, `"ephemeral_5m_input_tokens":0}`, `"ephemeral_5m_input_tokens":0,"ephemeral_24h_input_tokens":77}`, 1)
	tb.feedRaw(t, "msg_u1", rawLine("msg_u1", tb.clock, withNew))
	tb.feedRaw(t, "msg_u2", rawLine("msg_u2", tb.clock, withNew))
	tb.mainLine(mustParse(t, rawLine("msg_u1", tb.clock, withNew))) // a history copy

	d := tb.unseenDetails()
	if len(d) != 1 {
		t.Fatalf("invUnseenField raised %d times, want once: %v", len(d), d)
	}
	if !strings.Contains(d[0], "cache_creation.ephemeral_24h_input_tokens=77") || !strings.Contains(d[0], "claude-code transcript") {
		t.Errorf("alarm must name the adapter, the key path and its value: %s", d[0])
	}
	if n := len(tb.calls(t)); n != 2 {
		t.Errorf("booked %d calls, want 2: an unseen key does not stop the booking", n)
	}
}

// TestUnseenFieldFiresOnNonStandardRate: foci prices standard rates only, so
// a fast-mode call (speed "fast") or a non-standard service tier alarms. The
// call is still booked as before.
func TestUnseenFieldFiresOnNonStandardRate(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"speed fast", `"speed":"standard"`, `"speed":"fast"`, `speed="fast"`},
		{"priority tier", `"service_tier":"standard"`, `"service_tier":"priority"`, `service_tier="priority"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBook(t, nil)
			tb.feedRaw(t, "msg_s1", rawLine("msg_s1", tb.clock, strings.Replace(realUsage, tc.from, tc.to, 1)))
			d := tb.unseenDetails()
			if len(d) != 1 || !strings.Contains(d[0], tc.want) {
				t.Errorf("want one invUnseenField naming %s, got %v", tc.want, d)
			}
			if len(tb.calls(t)) != 1 {
				t.Error("the call must still be booked")
			}
		})
	}
}

func mustParse(t *testing.T, raw []byte) *ccLine {
	t.Helper()
	l, _ := parseCCRecord(raw)
	if l == nil {
		t.Fatal("premise: line did not parse")
	}
	return l
}

// TestMainTranscriptFinal: a later main line for a call booked in this process
// is a copy when its counts are identical, or when its usage is all zero (CC
// re-appends such copies); with any other counts the line the call was booked
// from was not final, which alarms naming the call and both counts. The first
// booking stands either way.
func TestMainTranscriptFinal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		later *ccLine
		alarm bool
	}{
		{"identical copy", line("msg_m1", opus, bookStart, "end_turn", 10, 20, 300, 0, 40), false},
		{"all-zero copy", line("msg_m1", opus, bookStart, "end_turn", 0, 0, 0, 0, 0), false},
		{"different counts", line("msg_m1", opus, bookStart, "end_turn", 10, 25, 300, 0, 40), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBook(t, nil)
			tb.recordTurn(t, "cap/c1@1")
			tb.streamNamed("msg_m1", "", "cap/c1@1", tb.clock)
			tb.mainLine(line("msg_m1", opus, tb.clock, "end_turn", 10, 20, 300, 0, 40))
			tb.mainLine(tc.later)

			n := tb.alarmsOf(accounting.InvMainTranscriptFinal)
			if tc.alarm != (n == 1) || n > 1 {
				t.Fatalf("invMainTranscriptFinal raised %d times, want alarm=%v: %+v", n, tc.alarm, tb.alarms)
			}
			if tc.alarm {
				d := tb.alarms[len(tb.alarms)-1].Detail
				for _, want := range []string{"msg_m1", "output=20", "output=25"} {
					if !strings.Contains(d, want) {
						t.Errorf("alarm missing %q: %s", want, d)
					}
				}
			}
			calls := tb.calls(t)
			if len(calls) != 1 || calls[0].tokens["output"] != 20 {
				t.Errorf("the first booking must stand: %+v", calls)
			}
		})
	}
}

// TestMainTranscriptFinalSkipsSubagentCalls: the rule is for main-thread
// calls; a main line for an id a subagent's transcript booked is not checked.
func TestMainTranscriptFinalSkipsSubagentCalls(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.recordTurn(t, "cap/c1@1")
	tb.tailOpened("agent-1", "cap/c1@1")
	tb.subLine("agent-1", "cap/c1@1", line("msg_a1", opus, tb.clock, "end_turn", 10, 20, 0, 0, 0))
	tb.mainLine(line("msg_a1", opus, tb.clock, "end_turn", 10, 99, 0, 0, 0))
	if n := tb.alarmsOf(accounting.InvMainTranscriptFinal); n != 0 {
		t.Errorf("invMainTranscriptFinal on a subagent's call: %+v", tb.alarms)
	}
}

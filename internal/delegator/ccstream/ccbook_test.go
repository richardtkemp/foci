package ccstream

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// ---- harness --------------------------------------------------------------

// testBook is a ccBook on a scratch shadow ledger with a settable clock.
type testBook struct {
	*ccBook
	path   string
	alarms []accounting.Alarm
	clock  time.Time
}

func newTestBook(t *testing.T, baseline map[string]ModelUsage) *testBook {
	t.Helper()
	tb := &testBook{path: filepath.Join(t.TempDir(), "api-shadow.db"), clock: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)}
	l, _, err := accounting.Open(tb.path, accounting.Options{Shadow: true, NoBackup: true,
		OnAlarm: func(a accounting.Alarm) { tb.alarms = append(tb.alarms, a) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tb.ccBook = newCCBook(l, log.NewComponentLogger("ccstream-test"), "cap/c1", "cap", time.Time{}, baseline)
	tb.now = func() time.Time { return tb.clock }
	return tb
}

// advance moves the clock and settles.
func (tb *testBook) advance(d time.Duration) {
	tb.clock = tb.clock.Add(d)
	tb.settle(false)
}

// alarmsOf counts the alarms raised for one invariant.
func (tb *testBook) alarmsOf(inv string) int {
	n := 0
	for _, a := range tb.alarms {
		if a.Invariant == inv {
			n++
		}
	}
	return n
}

// bookedCall is one row of the shadow ledger.
type bookedCall struct {
	key, turn, actor, kind, finality, model string
	tokens                                  modelinfo.Tokens
	cost                                    sql.NullFloat64
}

func (tb *testBook) calls(t *testing.T) []bookedCall {
	t.Helper()
	db, err := sqlite.OpenReadOnly(tb.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT c.id, COALESCE(c.call_key, ''), COALESCE(c.turn_id, ''), c.actor, c.kind, c.finality, c.model, cc.cost_usd
		FROM api_calls c JOIN call_costs cc ON cc.id = c.id ORDER BY c.id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []bookedCall
	var ids []int64
	for rows.Next() {
		var id int64
		var c bookedCall
		if err := rows.Scan(&id, &c.key, &c.turn, &c.actor, &c.kind, &c.finality, &c.model, &c.cost); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		out = append(out, c)
	}
	_ = rows.Close()
	for i, id := range ids {
		out[i].tokens = modelinfo.Tokens{}
		tr, err := db.Query(`SELECT class, count FROM call_tokens WHERE call_id = ?`, id)
		if err != nil {
			t.Fatal(err)
		}
		for tr.Next() {
			var c string
			var n int
			_ = tr.Scan(&c, &n)
			out[i].tokens[modelinfo.Class(c)] = n
		}
		_ = tr.Close()
	}
	return out
}

func find(calls []bookedCall, key string) *bookedCall {
	for i := range calls {
		if calls[i].key == key {
			return &calls[i]
		}
	}
	return nil
}

// line builds a transcript assistant line.
func line(id, model string, at time.Time, stop string, in, out, read, w5m, w1h int) *ccLine {
	u := ccUsageJSON{InputTokens: in, OutputTokens: out, CacheReadInputTokens: read,
		CacheCreationInputTokens: w5m + w1h, CacheCreation: &CacheCreationSplit{Ephemeral5m: w5m, Ephemeral1h: w1h}}
	t, m := ccTokens(u)
	return &ccLine{id: id, model: model, at: at, stopReason: stop, complete: stop != "", tokens: t, method: m}
}

const opus = "claude-opus-5"

// ---- replay of real captures (#2112 P0) -----------------------------------

type replayEvent struct {
	T       float64               `json:"t"`
	Kind    string                `json:"kind"`
	ID      string                `json:"id"`
	Agent   string                `json:"agent"`
	Line    string                `json:"line"`
	MU      map[string]ModelUsage `json:"mu"`
	Running int                   `json:"running"`
}

type replayFixture struct {
	Baseline map[string]ModelUsage `json:"baseline"`
	Events   []replayEvent         `json:"events"`
}

func loadReplay(t *testing.T, name string) replayFixture {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "ledger", name+".json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx replayFixture
	if err := json.NewDecoder(z).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// replay feeds a capture to a fresh book in capture order, on the capture's
// own clock, and flushes it as at process exit. It returns the cumulative
// totals CC itself reported last: the cost-state record, else the last result.
func replay(t *testing.T, name string) (*testBook, map[string]ModelUsage, replayFixture) {
	t.Helper()
	fx := loadReplay(t, name)
	tb := newTestBook(t, fx.Baseline)
	start := tb.clock
	var final map[string]ModelUsage
	for _, e := range fx.Events {
		tb.clock = start.Add(time.Duration(e.T * float64(time.Second)))
		switch e.Kind {
		case "named":
			tb.streamNamed(e.ID, "", tb.clock)
		case "result":
			tb.result(e.MU, e.Running, tb.clock)
			final = e.MU
		case "boundary":
			tb.compactBoundary("", tb.clock)
		case "tailOpened":
			tb.tailOpened(e.Agent, "")
		case "tailClosed":
			tb.tailClosed(e.Agent)
		case "mainLine", "subLine":
			l, cs := parseCCRecord([]byte(e.Line))
			switch {
			case cs != nil && e.Kind == "mainLine":
				tb.costStateSeen(cs.ModelUsage, tb.clock)
				final = cs.ModelUsage
			case l != nil && e.Kind == "mainLine":
				tb.mainLine(l)
			case l != nil:
				tb.subLine(e.Agent, "", l)
			}
		}
		tb.settle(false)
	}
	tb.advance(10 * time.Second)
	tb.exit()
	return tb, final, fx
}

// TestReplayConservation is T9 on real captures: whatever order the stream,
// the transcripts and the results arrive in, every token CC's own cumulative
// totals count is booked exactly once — Σ counted calls + remainder rows ==
// CC's final totals − the baseline it started from, per model and class —
// with no negative remainder and no named call left unbooked. Interrupted
// calls are booked and priced but CC counts them nowhere, so they are outside
// the sum.
func TestReplayConservation(t *testing.T) {
	for _, name := range []string{"run1", "run2", "r_resume_graceful", "r_resume_midturn", "r_resume_term", "r_mid2"} {
		t.Run(name, func(t *testing.T) {
			tb, final, fx := replay(t, name)
			if final == nil {
				t.Fatal("fixture premise: no result and no cost-state")
			}
			if n := tb.alarmsOf(accounting.InvNegativeRemainder); n != 0 {
				t.Errorf("negative remainders: %+v", tb.alarms)
			}
			if n := tb.alarmsOf(accounting.InvStreamIdBooked); n != 0 {
				t.Errorf("named calls never booked: %+v", tb.alarms)
			}
			got := map[string]modelinfo.Tokens{}
			var calls, derived float64
			kinds := map[string]int{}
			for _, c := range tb.calls(t) {
				kinds[c.kind+"/"+c.finality]++
				if c.cost.Valid {
					if c.finality == accounting.FinalityDerived {
						derived += c.cost.Float64
					} else {
						calls += c.cost.Float64
					}
				}
				if c.finality == accounting.FinalityInterrupted {
					continue
				}
				if got[c.model] == nil {
					got[c.model] = modelinfo.Tokens{}
				}
				for class, n := range remainderClasses(c.tokens) {
					got[c.model][class] += n
				}
			}
			t.Logf("%s: calls $%.4f, remainder rows $%.4f, copies %d, rows %v", name, calls, derived, tb.copies, kinds)
			for _, m := range slices.Sorted(maps.Keys(final)) {
				want, base := modelUsageTokens(final[m]), modelUsageTokens(fx.Baseline[m])
				for _, class := range slices.Sorted(maps.Keys(want)) {
					if w := want[class] - base[class]; got[m][class] != w {
						t.Errorf("%s %s: booked %d, CC counts %d", m, class, got[m][class], w)
					}
				}
			}
		})
	}
}

// TestReplayHistoryReappend is T23 on the real capture of two idle
// compactions in one process (run2): the second re-appends the whole prior
// history to the main transcript with the same ids, in batch writes. Those
// copies are dropped — the ledger holds each call once, the conservation test
// above passes over the same capture — and both compactions are booked as the
// remainder of their quiet windows.
func TestReplayHistoryReappend(t *testing.T) {
	tb, _, _ := replay(t, "run2")
	if tb.copies == 0 {
		t.Error("no copies dropped: the capture's 2nd compaction re-appends the history")
	}
	seen := map[string]bool{}
	compactions := 0
	for _, c := range tb.calls(t) {
		if seen[c.key] {
			t.Errorf("call %s booked twice", c.key)
		}
		seen[c.key] = true
		if c.kind == accounting.KindCompaction {
			compactions++
		}
	}
	if compactions == 0 {
		t.Error("no compaction booked from the quiet-window remainder")
	}
}

// TestReplayInterruptedIsPriced is T21 on the P0-e capture (r_mid2): a
// resumed process interrupted mid-turn leaves one stopless main-thread line.
// It is booked from that line as interrupted, priced (Dick 12:08), and the
// conservation above holds only because it is kept out of the subtraction.
func TestReplayInterruptedIsPriced(t *testing.T) {
	tb, _, _ := replay(t, "r_mid2")
	var n int
	for _, c := range tb.calls(t) {
		if c.finality == accounting.FinalityInterrupted {
			n++
			if !c.cost.Valid || c.cost.Float64 <= 0 {
				t.Errorf("interrupted call %s cost = %v, want priced", c.key, c.cost)
			}
		}
	}
	if n != 1 {
		t.Errorf("interrupted calls = %d, want 1", n)
	}
}

// ---- rules, one each -------------------------------------------------------

// TestMainCallBooksOnItsNamingTurn: a main-thread call books from its
// transcript line on the turn open when the stream named it — even if the
// line lands after the result (T15) — and its cache writes keep their
// observed TTL.
func TestMainCallBooksOnItsNamingTurn(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_1", "cap/c1@1", tb.clock)
	tb.result(map[string]ModelUsage{opus: {InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 300}}, 0, tb.clock)
	tb.advance(time.Millisecond)
	tb.mainLine(line("msg_1", opus, tb.clock, "end_turn", 10, 20, 0, 0, 300))
	tb.advance(time.Millisecond)

	c := find(tb.calls(t), "msg_1")
	if c == nil || c.turn != "cap/c1@1" || c.tokens[modelinfo.ClassCacheWrite1h] != 300 || c.finality != accounting.FinalityCompleted {
		t.Fatalf("call = %+v, want completed on cap/c1@1 with 300 1h writes", c)
	}
	for _, x := range tb.calls(t) {
		if x.kind == accounting.KindOverhead {
			t.Errorf("remainder booked %+v, want none: the call covers modelUsage exactly", x)
		}
	}
}

// TestHistoryCopiesAreDropped is T23 in miniature: a transcript line whose id
// the stream never named is a copy of history and is never booked, a
// re-appended copy of a call already booked is dropped without touching the
// ledger, and a genuinely new, named call in the same batch still books.
func TestHistoryCopiesAreDropped(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("msg_new", "T1", tb.clock)
	tb.mainLine(line("msg_old", opus, tb.clock, "end_turn", 1, 1, 0, 0, 0)) // pre-cutover history
	tb.mainLine(line("msg_new", opus, tb.clock, "end_turn", 5, 5, 0, 0, 0))
	tb.mainLine(line("msg_new", opus, tb.clock, "end_turn", 5, 5, 0, 0, 0)) // re-appended copy
	tb.result(map[string]ModelUsage{opus: {InputTokens: 5, OutputTokens: 5}}, 0, tb.clock)
	tb.advance(time.Second)

	calls := tb.calls(t)
	if len(calls) != 1 || calls[0].key != "msg_new" {
		t.Fatalf("calls = %+v, want only msg_new", calls)
	}
	if tb.copies != 2 {
		t.Errorf("copies = %d, want 2 (the unnamed history line and the re-append)", tb.copies)
	}
}

// TestSubagentStoplessAndCompleted covers T1 and T4: a subagent's partial
// line is superseded by its completed line (booked once, with the final
// output), and an id that never completes is booked `stopless` at its
// last-seen counts — its 5m cache write on the subagent's own call — when a
// later id appears in the same file. Both book on the turn that spawned it.
func TestSubagentStoplessAndCompleted(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.tailOpened("agent-a", "T1")
	tb.subLine("agent-a", "T2", line("s1", opus, tb.clock, "", 100, 1, 0, 15135, 0))
	tb.subLine("agent-a", "T2", line("s2", opus, tb.clock, "", 50, 2, 0, 0, 0))
	tb.subLine("agent-a", "T2", line("s2", opus, tb.clock, "end_turn", 50, 400, 0, 0, 0))
	tb.tailClosed("agent-a")

	calls := tb.calls(t)
	s1, s2 := find(calls, "s1"), find(calls, "s2")
	if s1 == nil || s1.finality != accounting.FinalityStopless || s1.tokens[modelinfo.ClassCacheWrite5m] != 15135 ||
		s1.actor != "agent-a" || s1.turn != "T1" {
		t.Errorf("s1 = %+v, want stopless, 15135 5m writes, on agent-a's spawning turn T1", s1)
	}
	if s2 == nil || s2.finality != accounting.FinalityCompleted || s2.tokens[modelinfo.ClassOutput] != 400 || len(calls) != 2 {
		t.Errorf("s2 = %+v (%d calls), want one completed call with the final 400 output", s2, len(calls))
	}
}

// TestSubagentRereadIsHarmless is T10: a reactivation tail re-reads a
// subagent's transcript from byte 0; nothing books twice.
func TestSubagentRereadIsHarmless(t *testing.T) {
	tb := newTestBook(t, nil)
	for range 2 {
		tb.tailOpened("agent-a", "T1")
		tb.subLine("agent-a", "T1", line("s1", opus, tb.clock, "end_turn", 10, 10, 0, 0, 0))
		tb.tailClosed("agent-a")
	}
	if calls := tb.calls(t); len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
}

// TestRemainderOnlyAtQuietPoints is T7: no remainder is booked while a
// subagent runs; the next quiet point books the whole increment since the
// baseline, as overhead with no turn (T13: a utility call CC counts and no
// transcript holds); a booked set larger than CC's count alarms and books
// nothing, never a clamp.
func TestRemainderOnlyAtQuietPoints(t *testing.T) {
	tb := newTestBook(t, map[string]ModelUsage{opus: {InputTokens: 1000}})
	tb.streamNamed("m1", "T1", tb.clock)
	tb.mainLine(line("m1", opus, tb.clock, "end_turn", 10, 10, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 1015, OutputTokens: 10}}, 1, tb.clock) // a subagent runs
	tb.advance(time.Second)
	for _, c := range tb.calls(t) {
		if c.kind == accounting.KindOverhead {
			t.Fatalf("remainder booked while a subagent ran: %+v", c)
		}
	}
	tb.streamNamed("m2", "T2", tb.clock)
	tb.mainLine(line("m2", opus, tb.clock, "end_turn", 20, 5, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 1040, OutputTokens: 15}}, 0, tb.clock)
	tb.advance(time.Second)
	var over *bookedCall
	calls := tb.calls(t)
	for i := range calls {
		if calls[i].kind == accounting.KindOverhead {
			over = &calls[i]
		}
	}
	if over == nil || over.turn != "" || over.tokens[modelinfo.ClassInput] != 10 || over.tokens[modelinfo.ClassOutput] != 0 {
		t.Fatalf("overhead = %+v, want input 10 (1040 − 1000 − 10 − 20), no turn", over)
	}

	tb.streamNamed("m3", "T3", tb.clock)
	tb.mainLine(line("m3", opus, tb.clock, "end_turn", 999, 0, 0, 0, 0))
	tb.result(map[string]ModelUsage{opus: {InputTokens: 1041, OutputTokens: 15}}, 0, tb.clock)
	tb.advance(time.Second)
	if tb.alarmsOf(accounting.InvNegativeRemainder) != 1 {
		t.Errorf("alarms = %+v, want one invNegativeRemainder", tb.alarms)
	}
	for _, c := range tb.calls(t) {
		if c.kind == accounting.KindOverhead && c.key != over.key {
			t.Errorf("a negative remainder booked %+v", c)
		}
	}
}

// TestInterruptedVersusStopless is T21/T22: in one window, a main-thread call
// seen only at a stopless line is INTERRUPTED — booked, priced, and kept out
// of the subtraction (CC counts it nowhere) — while a subagent's stopless call
// IS subtracted (CC counts it). With the classification right the remainder is
// exact; swapping either would make it wrong.
func TestInterruptedVersusStopless(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.streamNamed("m1", "T1", tb.clock)
	tb.mainLine(line("m1", opus, tb.clock, "", 300, 1, 0, 0, 0)) // interrupted: never completes
	tb.tailOpened("agent-a", "T1")
	tb.subLine("agent-a", "T1", line("s1", opus, tb.clock, "", 100, 1, 0, 0, 0))
	tb.tailClosed("agent-a")
	// CC counts the subagent's call with its true output (7) and nothing of m1.
	tb.result(map[string]ModelUsage{opus: {InputTokens: 100, OutputTokens: 7}}, 0, tb.clock)
	tb.advance(time.Second)

	calls := tb.calls(t)
	m1, s1 := find(calls, "m1"), find(calls, "s1")
	if m1 == nil || m1.finality != accounting.FinalityInterrupted || !m1.cost.Valid || m1.cost.Float64 <= 0 {
		t.Errorf("m1 = %+v, want an interrupted, priced call", m1)
	}
	if s1 == nil || s1.finality != accounting.FinalityStopless {
		t.Errorf("s1 = %+v, want stopless", s1)
	}
	var over *bookedCall
	for i := range calls {
		if calls[i].kind == accounting.KindOverhead {
			over = &calls[i]
		}
	}
	if tb.alarmsOf(accounting.InvNegativeRemainder) != 0 || over == nil ||
		over.tokens[modelinfo.ClassOutput] != 6 || over.tokens[modelinfo.ClassInput] != 0 {
		t.Errorf("overhead = %+v alarms %+v, want exactly the stopless call's missing output (6)", over, tb.alarms)
	}
}

// TestIdleCompactionIsItsOwnTurn is T24/T5: a /compact between turns reports
// nothing per call (its result.usage is zero, P0-a); it is the remainder of
// the quiet window holding its compact_boundary, booked exactly, as a
// compaction on a compaction turn of its own. A compaction inside a turn is
// booked on that turn (Dick, 13:44).
func TestIdleCompactionIsItsOwnTurn(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.compactBoundary("", tb.clock)
	tb.result(map[string]ModelUsage{opus: {InputTokens: 50000, OutputTokens: 2000}}, 0, tb.clock)
	tb.advance(time.Second)
	tb.compactBoundary("cap/c1@9", tb.clock)
	tb.result(map[string]ModelUsage{opus: {InputTokens: 90000, OutputTokens: 3000}}, 0, tb.clock)
	tb.advance(time.Second)

	var comp []bookedCall
	for _, c := range tb.calls(t) {
		if c.kind == accounting.KindCompaction {
			comp = append(comp, c)
		}
	}
	if len(comp) != 2 {
		t.Fatalf("compactions = %+v, want 2", comp)
	}
	if !strings.HasSuffix(comp[0].turn, ":compaction") || comp[0].tokens[modelinfo.ClassInput] != 50000 {
		t.Errorf("idle compaction = %+v, want the whole delta on a compaction turn of its own", comp[0])
	}
	if comp[1].turn != "cap/c1@9" || comp[1].tokens[modelinfo.ClassInput] != 40000 || comp[1].tokens[modelinfo.ClassOutput] != 1000 {
		t.Errorf("in-turn compaction = %+v, want its delta on the open turn", comp[1])
	}
}

// TestExitFlush is T8: at exit the last remainder comes from the cost-state
// CC appended at a graceful close; without one, from the last result.
func TestExitFlush(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		t.Run(fmt.Sprintf("graceful=%v", graceful), func(t *testing.T) {
			tb := newTestBook(t, nil)
			tb.result(map[string]ModelUsage{opus: {InputTokens: 100}}, 1, tb.clock) // not quiet
			tb.advance(time.Second)
			if graceful {
				tb.costStateSeen(map[string]ModelUsage{opus: {InputTokens: 130}}, tb.clock)
			}
			tb.exit()
			want := 100
			if graceful {
				want = 130
			}
			var got int
			for _, c := range tb.calls(t) {
				got += c.tokens[modelinfo.ClassInput]
			}
			if got != want {
				t.Errorf("flushed input = %d, want %d", got, want)
			}
		})
	}
}

// TestUnnamedMainLineBeforeLaunchIsNotThisProcess: a subagent line billed
// before this process launched is an earlier process's.
func TestSubagentLineBeforeLaunchIsSkipped(t *testing.T) {
	tb := newTestBook(t, nil)
	tb.launch = tb.clock
	tb.subLine("agent-a", "T1", line("old", opus, tb.clock.Add(-time.Minute), "end_turn", 1, 1, 0, 0, 0))
	if calls := tb.calls(t); len(calls) != 0 {
		t.Errorf("calls = %+v, want none", calls)
	}
}

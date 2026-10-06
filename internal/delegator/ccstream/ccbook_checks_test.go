package ccstream

import (
	"bytes"
	"database/sql"
	"maps"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// The #2111 P3 checks and turn activity on the Claude Code adapter: the live
// per-process divergence alarm (§8.1), the overhead bound (§8.2), and
// activity_closed_at closing when a turn's last subagent stops (R8).

// recordTurn stores a foci turn as the agent layer does at turn start.
func (tb *testBook) recordTurn(t *testing.T, turn string) {
	t.Helper()
	if err := tb.l.Update(func(tx *accounting.Tx) error {
		return tx.RecordTurn(accounting.Turn{TurnID: turn, Session: "cap/c1", Backend: accounting.BackendCCStream,
			Source: accounting.SourceUser, StartedAt: tb.clock})
	}); err != nil {
		t.Fatal(err)
	}
}

// activity is a stored turn's activity_closed_at ("" while still running).
func activity(t *testing.T, path, turn string) string {
	t.Helper()
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var at sql.NullString
	if err := db.QueryRow(`SELECT activity_closed_at FROM turns WHERE turn_id = ?`, turn).Scan(&at); err != nil {
		t.Fatalf("turn %s: %v", turn, err)
	}
	return at.String
}

func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// TestReplayChecksSilentOnRealCaptures: on every real capture, CC's own cost
// and the ledger's price agree and no remainder is oversized, so neither
// check fires on correct data (the silent half of T16).
func TestReplayChecksSilentOnRealCaptures(t *testing.T) {
	compared := 0
	for _, name := range []string{"run1", "run2", "r_resume_graceful", "r_resume_midturn", "r_resume_term", "r_mid2"} {
		t.Run(name, func(t *testing.T) {
			tb, _, _ := replay(t, name)
			if n := tb.alarmsOf(accounting.InvCostDivergence) + tb.alarmsOf(accounting.InvOverheadBounded); n != 0 {
				t.Errorf("alarms on a real capture: %+v", tb.alarms)
			}
			for m, s := range tb.procBooked {
				t.Logf("%s %s: ledger $%.4f", name, m, s.cost)
			}
			if len(tb.procBooked) > 0 {
				compared++
			}
		})
	}
	// r_mid2 holds only an interrupted call, which CC counts nowhere.
	if compared < 5 {
		t.Errorf("premise: the divergence check compared %d captures, want every one with counted spend (5)", compared)
	}
}

// TestCostDivergenceAlarm is T16: at a quiet point the ledger's price of the
// process's counted calls is compared with CC's own cost since launch. A rate
// that disagrees with CC's (here: CC's cost 10% over the ledger's price)
// alarms with the figures; agreement is silent. An interrupted call is priced
// but CC counts it nowhere, so it is on neither side; and a call of the next
// window, booked before the quiet result settles, is not compared against a
// report that does not count it.
func TestCostDivergenceAlarm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		factor float64
		alarms int
	}{
		{"agrees", 1, 0},
		{"rate disagrees", 1.10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBook(t, map[string]ModelUsage{opus: {InputTokens: 5000, CostUSD: 3}})
			tb.streamNamed("m0", "T1", tb.clock)
			tb.mainLine(line("m0", opus, tb.clock, "", 300, 1, 0, 0, 0)) // interrupted
			tb.streamNamed("m1", "T1", tb.clock)
			main := line("m1", opus, tb.clock, "end_turn", 10, 20, 0, 0, 4000)
			tb.mainLine(main)
			price := mustCost(t, main.tokens)
			tb.result(map[string]ModelUsage{opus: {InputTokens: 5010, OutputTokens: 20, CacheCreationInputTokens: 4000,
				CostUSD: 3 + tc.factor*price}}, 0, tb.clock)
			// The next window's call is booked before the result settles.
			tb.streamNamed("m2", "T2", tb.clock)
			tb.mainLine(line("m2", opus, tb.clock, "end_turn", 10, 90000, 0, 0, 0))
			tb.advance(time.Second)
			if got := tb.alarmsOf(accounting.InvCostDivergence); got != tc.alarms {
				t.Errorf("divergence alarms = %d, want %d: %+v", got, tc.alarms, tb.alarms)
			}
			if n := tb.alarmsOf(accounting.InvNegativeRemainder); n != 0 {
				t.Errorf("premise: negative remainder %+v", tb.alarms)
			}
		})
	}
}

// TestOverheadBounded: a remainder booked as overhead larger than both
// $0.50 and 2% of CC's cost for its window alarms — spend no transcript holds
// (here: a subagent whose tail never opened). A small overhead is silent, as
// is one within 2% of a large window, and a compaction (its own call, not
// overhead) is not bounded.
func TestOverheadBounded(t *testing.T) {
	big := modelinfo.Tokens{modelinfo.ClassOutput: 200000}
	if mustCost(t, big) <= ccOverheadBoundUSD {
		t.Fatal("premise: the missing spend must exceed the $ bound")
	}
	t.Run("missing source alarms", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.result(map[string]ModelUsage{opus: {OutputTokens: 200000, CostUSD: mustCost(t, big)}}, 0, tb.clock)
		tb.advance(time.Second)
		if n := tb.alarmsOf(accounting.InvOverheadBounded); n != 1 {
			t.Errorf("alarms = %+v, want one invOverheadBounded", tb.alarms)
		}
	})
	t.Run("small overhead is silent", func(t *testing.T) {
		tb := newTestBook(t, nil)
		small := modelinfo.Tokens{modelinfo.ClassOutput: 300}
		tb.result(map[string]ModelUsage{opus: {OutputTokens: 300, CostUSD: mustCost(t, small)}}, 0, tb.clock)
		tb.advance(time.Second)
		if n := tb.alarmsOf(accounting.InvOverheadBounded); n != 0 {
			t.Errorf("alarms = %+v, want none", tb.alarms)
		}
	})
	t.Run("within 2% of a large window is silent", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.streamNamed("m1", "T1", tb.clock)
		main := line("m1", opus, tb.clock, "end_turn", 0, 0, 0, 0, 20_000_000)
		tb.mainLine(main)
		over := modelinfo.Tokens{modelinfo.ClassOutput: 60000}
		window := mustCost(t, main.tokens) + mustCost(t, over)
		if o := mustCost(t, over); o <= ccOverheadBoundUSD || o > ccOverheadBoundShare*window {
			t.Fatalf("premise: overhead $%.2f must be over $%.2f and within 2%% of $%.2f", o, ccOverheadBoundUSD, window)
		}
		tb.result(map[string]ModelUsage{opus: {OutputTokens: 60000, CacheCreationInputTokens: 20_000_000, CostUSD: window}}, 0, tb.clock)
		tb.advance(time.Second)
		if n := tb.alarmsOf(accounting.InvOverheadBounded); n != 0 {
			t.Errorf("alarms = %+v, want none", tb.alarms)
		}
	})
	t.Run("a compaction is not overhead", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.compactBoundary("", tb.clock)
		tb.result(map[string]ModelUsage{opus: {OutputTokens: 200000, CostUSD: mustCost(t, big)}}, 0, tb.clock)
		tb.advance(time.Second)
		if n := tb.alarmsOf(accounting.InvOverheadBounded); n != 0 {
			t.Errorf("alarms = %+v, want none", tb.alarms)
		}
	})
}

// TestOverheadBoundedSeverity is #2175: an over-bound overhead that is output
// tokens only is CC bug #84223 (subagent transcripts missing their final
// record), so it alarms at WARN and names the bug; any other class in the
// overhead keeps it an ERROR.
func TestOverheadBoundedSeverity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usage    ModelUsage
		tokens   modelinfo.Tokens
		wantWarn bool
	}{
		{"output only warns", ModelUsage{OutputTokens: 200000},
			modelinfo.Tokens{modelinfo.ClassOutput: 200000}, true},
		{"input too stays an error", ModelUsage{InputTokens: 50000, OutputTokens: 200000},
			modelinfo.Tokens{modelinfo.ClassInput: 50000, modelinfo.ClassOutput: 200000}, false},
		{"cache read too stays an error", ModelUsage{CacheReadInputTokens: 50000, OutputTokens: 200000},
			modelinfo.Tokens{modelinfo.ClassCacheRead: 50000, modelinfo.ClassOutput: 200000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBook(t, nil)
			u := tc.usage
			u.CostUSD = mustCost(t, tc.tokens)
			tb.result(map[string]ModelUsage{opus: u}, 0, tb.clock)
			tb.advance(time.Second)
			var got []accounting.Alarm
			for _, a := range tb.alarms {
				if a.Invariant == accounting.InvOverheadBounded {
					got = append(got, a)
				}
			}
			if len(got) != 1 {
				t.Fatalf("alarms = %+v, want one invOverheadBounded", tb.alarms)
			}
			if got[0].Warn != tc.wantWarn {
				t.Errorf("Warn = %v, want %v: %+v", got[0].Warn, tc.wantWarn, got[0])
			}
			if named := strings.Contains(got[0].Detail, "anthropics/claude-code/issues/84223"); named != tc.wantWarn {
				t.Errorf("detail names CC bug #84223 = %v, want %v: %s", named, tc.wantWarn, got[0].Detail)
			}
		})
	}
}

// TestTurnActivityClosesWhenItsSubagentsStop is T14 for activity_closed_at: a
// turn that spawned no subagent closes at its end; one whose background
// subagent outlives it stays running until the subagent's tail closes, and
// reopens if the subagent is resumed on it; a turn whose named call's line
// has not been read yet waits for it; and a run no foci turn opened closes
// at its result.
func TestTurnActivityClosesWhenItsSubagentsStop(t *testing.T) {
	t.Run("no subagent", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.recordTurn(t, "T1")
		tb.turnEnded("T1", tb.clock)
		if got := activity(t, tb.path, "T1"); got != stamp(tb.clock) {
			t.Errorf("activity = %q, want closed at the turn's end %s", got, stamp(tb.clock))
		}
	})
	t.Run("background subagent outlives its turn", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.recordTurn(t, "T1")
		tb.tailOpened("agent-a", "T1")
		tb.turnEnded("T1", tb.clock)
		if got := activity(t, tb.path, "T1"); got != "" {
			t.Fatalf("activity = %q at turn end, want still running: its subagent's tail is open", got)
		}
		tb.clock = tb.clock.Add(time.Minute)
		tb.subLine("agent-a", "T2", line("s1", opus, tb.clock, "", 100, 1, 0, 0, 0))
		tb.tailClosed("agent-a")
		closedAt := stamp(tb.clock)
		if got := activity(t, tb.path, "T1"); got != closedAt {
			t.Fatalf("activity = %q, want closed when the tail closed %s", got, closedAt)
		}
		// A SendMessage resume runs the subagent again on its spawning turn.
		tb.clock = tb.clock.Add(time.Minute)
		tb.tailOpened("agent-a", "T3")
		if got := activity(t, tb.path, "T1"); got != "" {
			t.Fatalf("activity = %q after a resume, want running again", got)
		}
		tb.clock = tb.clock.Add(time.Minute)
		tb.tailClosed("agent-a")
		if got := activity(t, tb.path, "T1"); got != stamp(tb.clock) {
			t.Errorf("activity = %q, want closed again at %s", got, stamp(tb.clock))
		}
	})
	t.Run("named call not yet read", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.recordTurn(t, "T1")
		tb.streamNamed("m1", "T1", tb.clock)
		tb.turnEnded("T1", tb.clock)
		if got := activity(t, tb.path, "T1"); got != "" {
			t.Fatalf("activity = %q, want running until m1 is booked", got)
		}
		tb.clock = tb.clock.Add(time.Millisecond)
		tb.mainLine(line("m1", opus, tb.clock, "end_turn", 1, 1, 0, 0, 0))
		if got := activity(t, tb.path, "T1"); got != stamp(tb.clock) {
			t.Errorf("activity = %q, want closed once m1 booked %s", got, stamp(tb.clock))
		}
	})
	t.Run("run turn closes at its result", func(t *testing.T) {
		tb := newTestBook(t, nil)
		tb.streamNamed("m1", "", tb.clock)
		tb.mainLine(line("m1", opus, tb.clock, "end_turn", 1, 1, 0, 0, 0))
		run := find(tb.calls(t), "m1").turn
		tb.result(map[string]ModelUsage{opus: {InputTokens: 1, OutputTokens: 1}}, 0, tb.clock)
		if got := activity(t, tb.path, run); got != stamp(tb.clock) {
			t.Errorf("run turn activity = %q, want closed at its result %s", got, stamp(tb.clock))
		}
	})
}

// TestTurnEndedAfterExitClosesTheTurn: the agent layer can complete a turn
// after its CC process has exited and the adapter has flushed (finalizeExit
// claims an in-flight turn). Nothing reads the adapter's queue then, so the
// turn's end is applied directly and its activity still closes. Not parallel:
// it sets the process's live ledger.
func TestTurnEndedAfterExitClosesTheTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	live, _, err := accounting.Open(path, accounting.Options{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(live)
	t.Cleanup(func() { accounting.SetLive(nil); _ = live.Close() })
	if err := live.Update(func(tx *accounting.Tx) error {
		return tx.RecordTurn(accounting.Turn{TurnID: "cap/c1@1", Session: "cap/c1", Backend: accounting.BackendCCStream,
			Source: accounting.SourceUser, StartedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	b.ledger.Store(lg)
	b.turnMu.Lock()
	b.beginTurnLocked(&delegator.TurnEvents{TurnID: "cap/c1@1", OnTurnComplete: func(*delegator.TurnResult) {}})
	b.stateEventsSeen = true
	b.turnMu.Unlock()
	lg.close()
	b.completeTurn("idle")
	if got := activity(t, path, "cap/c1@1"); got == "" {
		t.Error("turn still running after its process exited and the turn completed")
	}
}

// TestRefusedAttemptBookedByCategory is #2200: a model refusal fallback leaves
// the refused attempt in no transcript, so its spend reached the remainder and
// was booked as unexplained overhead (an invOverheadBounded ERROR). It is now
// booked as the refused call on the turn it belongs to, priced by Anthropic's
// refusal billing: a refusal before any output is billed only in the bio,
// frontier_llm and reasoning_extraction categories, a mid-stream one always.
// CC's own cost counts the attempt either way, so an unbilled one is excluded
// from the divergence check rather than booked at CC's figure.
func TestRefusedAttemptBookedByCategory(t *testing.T) {
	const fallback = "claude-opus-4-8"
	// The 2026-10-06 incident's refused attempt: no output, 1h cache writes.
	refused := modelinfo.Tokens{modelinfo.ClassInput: 4, modelinfo.ClassCacheRead: 22380, modelinfo.ClassCacheWrite1h: 79451}
	if mustCost(t, refused) <= ccOverheadBoundUSD {
		t.Fatal("premise: the refused attempt must exceed the overhead bound")
	}
	for _, tc := range []struct {
		name      string
		category  string
		retracted []string
		output    int
		billed    bool
	}{
		{"cyber before any output is not billed", "cyber", nil, 0, false},
		{"no category is not billed", "", nil, 0, false},
		{"bio before any output is billed", "bio", nil, 0, true},
		{"frontier_llm before any output is billed", "frontier_llm", nil, 0, true},
		{"reasoning_extraction before any output is billed", "reasoning_extraction", nil, 0, true},
		{"cyber mid-stream (output streamed) is billed", "cyber", nil, 120, true},
		{"cyber mid-stream (message retracted) is billed", "cyber", []string{"u1"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBook(t, nil)
			tb.recordTurn(t, "T1")
			tok := maps.Clone(refused)
			if tc.output > 0 {
				tok[modelinfo.ClassOutput] = tc.output
			}
			tb.refusalFallback(ccRefusal{turn: "T1", at: tb.clock, model: opus, fallback: fallback,
				category: tc.category, requestID: "req_1", retracted: len(tc.retracted)})
			tb.streamNamed("m1", "T1", tb.clock)
			ans := line("m1", fallback, tb.clock, "end_turn", 4, 300, 0, 0, 100000)
			tb.mainLine(ans)
			ansCost, ok := modelinfo.CostAsOf(fallback, tb.clock, ans.tokens)
			if !ok {
				t.Fatal("premise: fallback model unpriced")
			}
			tb.result(map[string]ModelUsage{
				opus: {InputTokens: 4, OutputTokens: tc.output, CacheReadInputTokens: 22380,
					CacheCreationInputTokens: 79451, CostUSD: mustCost(t, tok)},
				fallback: {InputTokens: 4, OutputTokens: 300, CacheCreationInputTokens: 100000, CostUSD: ansCost},
			}, 0, tb.clock)
			tb.advance(time.Second)
			if len(tb.alarms) != 0 {
				t.Errorf("alarms = %+v, want none: a refusal fallback is a known cause", tb.alarms)
			}
			c := find(tb.calls(t), "refusal:req_1")
			if c == nil {
				t.Fatalf("no refusal row booked: %+v", tb.calls(t))
			}
			if c.kind != accounting.KindCall || c.turn != "T1" || c.model != opus {
				t.Errorf("refusal row = %+v, want a call on T1 on %s", c, opus)
			}
			want := 0.0
			if tc.billed {
				want = mustCost(t, tok)
			}
			if !c.cost.Valid || math.Abs(c.cost.Float64-want) > 1e-9 {
				t.Errorf("refusal cost = %v, want $%.6f (billed=%v)", c.cost, want, tc.billed)
			}
		})
	}
}

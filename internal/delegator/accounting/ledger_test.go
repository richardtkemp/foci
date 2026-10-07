package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

// openLedger opens a fresh ledger in a temp dir, collecting its alarms.
func openLedger(t *testing.T) (*Ledger, *[]Alarm) {
	t.Helper()
	var alarms []Alarm
	l, rep, err := Open(filepath.Join(t.TempDir(), "api.db"), Options{OnAlarm: func(a Alarm) { alarms = append(alarms, a) }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if rep != nil {
		t.Fatalf("Open on an empty db migrated something: %+v", rep)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, &alarms
}

var t0 = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

// turn is a minimal open turn.
func turn(id string) Turn {
	return Turn{TurnID: id, Session: "agent/c1", AgentID: "agent", Backend: BackendCCStream, Source: SourceUser, StartedAt: t0}
}

// call is a minimal completed call on turn "T".
func call(key string, tokens modelinfo.Tokens) Call {
	return Call{
		Key: key, Backend: BackendCCStream, Provider: "anthropic", Model: "claude-opus-5",
		Session: "agent/c1", AgentID: "agent", TurnID: "T",
		Kind: KindCall, Finality: FinalityCompleted, ClassMethod: ClassMethodObserved,
		BilledAt: t0, Tokens: tokens,
	}
}

func mustUpdate(t *testing.T, l *Ledger, fn func(*Tx) error) {
	t.Helper()
	if err := l.Update(fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func book(t *testing.T, l *Ledger, cs ...Call) []Booked {
	t.Helper()
	var out []Booked
	mustUpdate(t, l, func(tx *Tx) error {
		for _, c := range cs {
			b, err := tx.Book(c)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	return out
}

// viewCost reads one call's cost from the call_costs view.
func viewCost(t *testing.T, l *Ledger, id int64) sql.NullFloat64 {
	t.Helper()
	var c sql.NullFloat64
	if err := l.db.QueryRow(`SELECT cost_usd FROM call_costs WHERE id = ?`, id).Scan(&c); err != nil {
		t.Fatalf("read call_costs %d: %v", id, err)
	}
	return c
}

// multiRowGroup finds a real models.jsonl rate group with at least two dated
// rows whose input rate differs, and a leaf id that resolves to it alone, so
// the re-price identity is exercised across a price change.
func multiRowGroup(t *testing.T) (model string, change string) {
	t.Helper()
	byGroup := map[string][]modelinfo.RateRow{}
	for _, r := range modelinfo.RateTable() {
		if r.Class == modelinfo.ClassInput && r.USDPerUnit != nil {
			byGroup[r.RateModel] = append(byGroup[r.RateModel], r)
		}
	}
	for _, rm := range slices.Sorted(maps.Keys(byGroup)) {
		rows := byGroup[rm]
		for i := 1; i < len(rows); i++ {
			if *rows[i].USDPerUnit != *rows[i-1].USDPerUnit && rows[i].EffectiveFrom != "" {
				leaf := strings.SplitN(rm, "|", 2)[0]
				if got, ok := modelinfo.ResolveRateModel(leaf, t0, 0); ok && got == rm {
					return leaf, rows[i].EffectiveFrom
				}
			}
		}
	}
	t.Fatal("no rate group in models.jsonl has a dated input-rate change — the identity would never cross one")
	return "", ""
}

// TestRepriceIdentity is T11 (#2111 §9): for any call, the call_costs view
// and modelinfo.CostAsOf agree — the same figure when priced, NULL exactly
// when CostAsOf reports the call unpriced. It spans exact, variant,
// punctuation-folded, family-resolved, unresolved and synthetic models, every
// class in the vocabulary plus one outside it, and billing dates before,
// across and after a real price change.
func TestRepriceIdentity(t *testing.T) {
	changing, change := multiRowGroup(t)
	changeAt, err := time.Parse("2006-01-02", change)
	if err != nil {
		t.Fatalf("change date %q: %v", change, err)
	}
	models := []string{
		changing,
		"claude-opus-5", "claude/claude-opus-5-5", "claude-haiku-4-5-20251001",
		"anthropic/claude-sonnet-4-5", "claude-opus-4-8", "claude-opus-4-6[1m]",
		"claude-sonnet-9-9", "claude-code", "glm-5.2", "openrouter/z-ai/glm-5-turbo",
		"mystery-model-x", "<synthetic>",
		// Prompt-size tiers (#2240): random counts put most calls over the
		// 100K / 200K thresholds and some under.
		"claude-haiku-5-5", "claude-sonnet-4",
	}
	classes := append(modelinfo.Classes(), "audio")
	dates := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), // before every dated row
		changeAt.Add(-time.Second), changeAt, changeAt.Add(23 * time.Hour),
		// Local-offset times whose local date is a day after their UTC date:
		// rates key on the UTC date, so the second prices BEFORE the change.
		time.Date(2026, 9, 29, 0, 30, 0, 0, time.FixedZone("BST", 3600)),
		changeAt.Add(-30 * time.Minute).In(time.FixedZone("UTC+1", 3600)),
		t0,
	}

	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	rng := rand.New(rand.NewPCG(2111, 11))
	var calls []Call
	for i := range 600 {
		tok := modelinfo.Tokens{}
		for _, c := range classes {
			if rng.IntN(3) == 0 {
				tok[c] = rng.IntN(2_000_000)
			}
		}
		if c := classes[rng.IntN(len(classes))]; c == "audio" && rng.IntN(4) != 0 {
			delete(tok, c) // keep unknown-class calls a minority
		}
		c := call(fmt.Sprintf("k%d", i), tok)
		c.Model = models[rng.IntN(len(models))]
		c.BilledAt = dates[rng.IntN(len(dates))]
		calls = append(calls, c)
	}
	booked := book(t, l, calls...)

	var priced, unpriced int
	for i, c := range calls {
		want, ok := modelinfo.CostAsOf(c.Model, c.BilledAt, c.Tokens)
		got := viewCost(t, l, booked[i].ID)
		switch {
		case !ok && got.Valid:
			t.Errorf("%s @%s %v: view priced it at $%v, CostAsOf says unpriced", c.Model, c.BilledAt, c.Tokens, got.Float64)
		case ok && !got.Valid:
			t.Errorf("%s @%s %v: view says NULL, CostAsOf priced it at $%v", c.Model, c.BilledAt, c.Tokens, want)
		case ok && math.Abs(got.Float64-want) > 1e-9*math.Max(1, want):
			t.Errorf("%s @%s %v: view $%.12f, CostAsOf $%.12f", c.Model, c.BilledAt, c.Tokens, got.Float64, want)
		}
		if ok {
			priced++
		} else {
			unpriced++
		}
	}
	if priced < 100 || unpriced < 20 {
		t.Fatalf("only %d priced and %d unpriced calls — the identity was not exercised on both arms", priced, unpriced)
	}
}

// TestTTLClassesPriceAtTheirOwnRates is T11's second half: each cache-write
// class prices at its own rate in the view (opus-5: 5m $6.25/M, 1h $10/M,
// TTL-unknown at the higher 1h rate).
func TestTTLClassesPriceAtTheirOwnRates(t *testing.T) {
	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	b := book(t, l,
		call("5m", modelinfo.Tokens{modelinfo.ClassCacheWrite5m: 1_000_000}),
		call("1h", modelinfo.Tokens{modelinfo.ClassCacheWrite1h: 1_000_000}),
		call("unknown", modelinfo.Tokens{modelinfo.ClassCacheWrite: 1_000_000}),
	)
	for i, want := range []float64{6.25, 10, 10} {
		if got := viewCost(t, l, b[i].ID); !got.Valid || math.Abs(got.Float64-want) > 1e-12 {
			t.Errorf("call %d: cost %+v, want $%v", i, got, want)
		}
	}
}

// TestUnpricedIsNullNeverZero is T19: a model with no rate group, a class with
// no rate, and a class outside the vocabulary each leave the call's cost NULL
// and raise their alarm; aggregates count them rather than adding $0.
func TestUnpricedIsNullNeverZero(t *testing.T) {
	l, alarms := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	unknownModel := call("m", modelinfo.Tokens{modelinfo.ClassInput: 1000})
	unknownModel.Model = "mystery-model-x"
	noRate := call("r", modelinfo.Tokens{modelinfo.ClassInput: 1000, modelinfo.ClassWebSearch: 2})
	noRate.Model = "claude-code" // carries no per-search rate
	badClass := call("c", modelinfo.Tokens{modelinfo.ClassInput: 1000, "audio": 5})
	good := call("g", modelinfo.Tokens{modelinfo.ClassInput: 1_000_000})
	synthetic := call("s", modelinfo.Tokens{})
	synthetic.Model = "<synthetic>"
	b := book(t, l, unknownModel, noRate, badClass, good, synthetic)

	for i, name := range []string{"unknown model", "class with no rate", "class outside vocabulary"} {
		if got := viewCost(t, l, b[i].ID); got.Valid {
			t.Errorf("%s: cost $%v, want NULL", name, got.Float64)
		}
	}
	if got := viewCost(t, l, b[3].ID); !got.Valid || got.Float64 != 5 {
		t.Errorf("priced call: cost %+v, want $5", got)
	}
	if got := viewCost(t, l, b[4].ID); !got.Valid || got.Float64 != 0 {
		t.Errorf("synthetic call with no tokens: cost %+v, want $0", got)
	}

	var inv []string
	for _, a := range *alarms {
		inv = append(inv, a.Invariant)
	}
	slices.Sort(inv)
	if want := []string{InvClassNoRate, InvClassNoRate, InvModelNotInTable}; !slices.Equal(inv, want) {
		t.Errorf("alarms = %v, want %v", *alarms, want)
	}

	var cost float64
	var unpriced, calls int
	if err := l.db.QueryRow(`SELECT cost_usd, unpriced_calls, calls FROM turn_costs WHERE turn_id = 'T'`).
		Scan(&cost, &unpriced, &calls); err != nil {
		t.Fatal(err)
	}
	if cost != 5 || unpriced != 3 || calls != 5 {
		t.Errorf("turn_costs = $%v, %d unpriced of %d calls; want $5, 3 unpriced of 5", cost, unpriced, calls)
	}
}

// TestBookDedupsByKey: the unique (backend, key) index is the dedup. A replay
// with identical counts is silent; one with different counts keeps the first
// booking and raises invSameIDDifferentUsage.
func TestBookDedupsByKey(t *testing.T) {
	l, alarms := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	first := call("msg_1", modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassOutput: 0})
	b := book(t, l, first, call("msg_1", modelinfo.Tokens{modelinfo.ClassInput: 10}))
	if b[0].Duplicate || !b[1].Duplicate || b[0].ID != b[1].ID {
		t.Fatalf("identical replay: %+v, want the second a duplicate of the first", b)
	}
	if len(*alarms) != 0 {
		t.Fatalf("identical replay alarmed: %v", *alarms)
	}
	// The same key on another backend is a different call.
	other := call("msg_1", modelinfo.Tokens{modelinfo.ClassInput: 10})
	other.Backend = BackendOpencode
	if b := book(t, l, other); b[0].Duplicate {
		t.Fatal("same key on another backend was treated as a duplicate")
	}

	b = book(t, l, call("msg_1", modelinfo.Tokens{modelinfo.ClassInput: 99}))
	if !b[0].Duplicate {
		t.Fatal("changed replay was booked as a new call")
	}
	if len(*alarms) != 1 || (*alarms)[0].Invariant != InvSameIDDifferentUsage {
		t.Fatalf("alarms = %v, want one invSameIDDifferentUsage", *alarms)
	}
	var n int
	if err := l.db.QueryRow(`SELECT count FROM call_tokens WHERE call_id = ? AND class = 'input'`, b[0].ID).Scan(&n); err != nil || n != 10 {
		t.Fatalf("stored input = %d (%v), want the first booking's 10", n, err)
	}
}

// TestAlarmsOnlyAfterCommit: a booking rolled back never alarms.
func TestAlarmsOnlyAfterCommit(t *testing.T) {
	l, alarms := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	c := call("m", modelinfo.Tokens{modelinfo.ClassInput: 1})
	c.Model = "mystery-model-x"
	boom := errors.New("boom")
	if err := l.Update(func(tx *Tx) error {
		if _, err := tx.Book(c); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Update = %v, want boom", err)
	}
	if len(*alarms) != 0 {
		t.Fatalf("a rolled-back booking alarmed: %v", *alarms)
	}
	var n int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d calls after rollback, want 0", n)
	}
}

// TestBookRejectsBrokenCalls: the Call contract is enforced before SQL, and
// the schema backs it (a call cannot name a turn that does not exist).
func TestBookRejectsBrokenCalls(t *testing.T) {
	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	mut := map[string]func(*Call){
		"negative count":    func(c *Call) { c.Tokens = modelinfo.Tokens{modelinfo.ClassInput: -1} },
		"no key":            func(c *Call) { c.Key = "" },
		"legacy with a key": func(c *Call) { c.Kind, c.Finality = KindLegacy, FinalityLegacy },
		"no turn":           func(c *Call) { c.TurnID = "" },
		"overhead on turn":  func(c *Call) { c.Kind = KindOverhead },
		"no billing time":   func(c *Call) { c.BilledAt = time.Time{} },
		"unknown turn":      func(c *Call) { c.TurnID = "nope" },
		"bad finality":      func(c *Call) { c.Finality = "final-ish" },
		"legacy fields":     func(c *Call) { c.LegacyID = 7 },
		"recorded, live":    func(c *Call) { c.CostBasis = CostBasisRecorded },
		"recorded, no figure": func(c *Call) {
			c.Key, c.Kind, c.Finality, c.CostBasis = "", KindLegacy, FinalityLegacy, CostBasisRecorded
		},
		"bad basis": func(c *Call) { c.CostBasis = "guessed" },
	}
	for _, name := range slices.Sorted(maps.Keys(mut)) {
		c := call("k-"+name, modelinfo.Tokens{modelinfo.ClassInput: 1})
		mut[name](&c)
		if err := l.Update(func(tx *Tx) error { _, err := tx.Book(c); return err }); err == nil {
			t.Errorf("%s: booked, want an error", name)
		}
	}
	// Overhead is the one call with no turn.
	o := call("overhead:1", modelinfo.Tokens{modelinfo.ClassInput: 1})
	o.Kind, o.Finality, o.TurnID = KindOverhead, FinalityDerived, ""
	book(t, l, o)
}

// TestTurnCostsIsLive is the R8 half of T14: a turn reads still_running until
// its activity closes, its cost follows the calls as they book, and its split
// and context fill come from the calls.
func TestTurnCostsIsLive(t *testing.T) {
	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	parent := call("p1", modelinfo.Tokens{modelinfo.ClassInput: 100, modelinfo.ClassCacheRead: 2000, modelinfo.ClassCacheWrite1h: 300, modelinfo.ClassOutput: 50})
	later := call("p2", modelinfo.Tokens{modelinfo.ClassInput: 7, modelinfo.ClassCacheRead: 2400, modelinfo.ClassOutput: 9})
	later.BilledAt = t0.Add(time.Minute)
	book(t, l, parent, later)

	read := func() (cost, parentCost, subCost float64, running bool, fill int) {
		t.Helper()
		if err := l.db.QueryRow(`SELECT cost_usd, parent_cost_usd, subagent_cost_usd, still_running, context_fill
			FROM turn_costs WHERE turn_id = 'T'`).Scan(&cost, &parentCost, &subCost, &running, &fill); err != nil {
			t.Fatal(err)
		}
		return
	}
	cost1, _, _, running, fill := read()
	if !running {
		t.Error("open turn with no activity_closed_at reads not running")
	}
	if fill != 7+2400 {
		t.Errorf("context_fill = %d, want the LAST parent call's input+cache (2407)", fill)
	}

	sub := call("s1", modelinfo.Tokens{modelinfo.ClassInput: 5000, modelinfo.ClassCacheWrite5m: 1000})
	sub.Actor, sub.BilledAt = "toolu_1", t0.Add(2*time.Minute)
	book(t, l, sub)
	cost2, parentCost, subCost, _, _ := read()
	subWant, _ := modelinfo.CostAsOf(sub.Model, sub.BilledAt, sub.Tokens)
	if math.Abs(cost2-cost1-subWant) > 1e-12 || math.Abs(subCost-subWant) > 1e-12 || math.Abs(parentCost-cost1) > 1e-12 {
		t.Errorf("after a subagent call: cost %v (was %v), parent %v, subagent %v; want +%v on the subagent side", cost2, cost1, parentCost, subCost, subWant)
	}

	closed := turn("T")
	closed.EndedAt, closed.ActivityClosedAt = t0.Add(3*time.Minute), t0.Add(3*time.Minute)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(closed) })
	if _, _, _, running, _ := read(); running {
		t.Error("turn reads still running after its activity closed")
	}
}

// TestOpenRerendersRates: the rate tables are a full re-render of modelinfo
// on every open, so a copy that drifted is corrected.
func TestOpenRerendersRates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	l, _, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`UPDATE token_rates SET usd_per_unit = 999`); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	l, _, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var n, want int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM token_rates WHERE usd_per_unit = 999`).Scan(&n)
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM token_rates`).Scan(&want)
	if n != 0 || want != len(modelinfo.RateTable()) {
		t.Errorf("after reopen: %d drifted rows, %d rows; want 0 and %d", n, want, len(modelinfo.RateTable()))
	}
}

// TestInterruptedCallsArePriced pins Dick's #2111 §13.8 ruling (2026-09-29
// 12:08): an interrupted main-thread call — sent and billed, though CC counts
// it nowhere — is priced like any other call, in the call and in its turn.
func TestInterruptedCallsArePriced(t *testing.T) {
	l, _ := openLedger(t)
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(turn("T")) })
	done := call("done", modelinfo.Tokens{modelinfo.ClassInput: 1_000_000})
	cut := call("cut", modelinfo.Tokens{modelinfo.ClassCacheRead: 1_000_000, modelinfo.ClassCacheWrite1h: 100_000, modelinfo.ClassOutput: 1})
	cut.Finality = FinalityInterrupted
	b := book(t, l, done, cut)
	want, ok := modelinfo.CostAsOf(cut.Model, cut.BilledAt, cut.Tokens)
	if got := viewCost(t, l, b[1].ID); !ok || !got.Valid || math.Abs(got.Float64-want) > 1e-12 || want == 0 {
		t.Fatalf("interrupted call costs %+v, want $%v", got, want)
	}
	var turnCost float64
	_ = l.db.QueryRow(`SELECT cost_usd FROM turn_costs WHERE turn_id = 'T'`).Scan(&turnCost)
	if math.Abs(turnCost-(5+want)) > 1e-12 {
		t.Errorf("turn cost $%v, want $%v including the interrupted call", turnCost, 5+want)
	}
}

// TestSetTurnActivityReopensAndCloses: a backend whose spend outlives its turn
// closes the turn's activity itself, and can reopen it (still_running again)
// when spend resumes on it — which RecordTurn's never-overwrite-with-empty
// upsert cannot do. A turn not stored is left alone.
func TestSetTurnActivityReopensAndCloses(t *testing.T) {
	l, _ := openLedger(t)
	closed := turn("T")
	closed.EndedAt, closed.ActivityClosedAt = t0, t0
	mustUpdate(t, l, func(tx *Tx) error { return tx.RecordTurn(closed) })
	running := func() bool {
		t.Helper()
		var r bool
		if err := l.db.QueryRow(`SELECT still_running FROM turn_costs WHERE turn_id = 'T'`).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if err := l.SetTurnActivity("T", time.Time{}); err != nil || !running() {
		t.Fatalf("reopen: err %v, running %v; want running", err, running())
	}
	if err := l.SetTurnActivity("T", t0.Add(time.Minute)); err != nil || running() {
		t.Fatalf("close: err %v, running %v; want closed", err, running())
	}
	if err := l.SetTurnActivity("absent", t0); err != nil {
		t.Errorf("a turn not stored: %v", err)
	}
	var n int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&n)
	if n != 1 {
		t.Errorf("turns = %d, want 1: SetTurnActivity never inserts", n)
	}
}

// TestCloseOrphanedTurns: at gateway start no earlier backend process
// survives, so a turn an earlier gateway left running is closed at its last
// call, else its end, else its start; a turn begun since start, and a closed
// one, are left alone.
func TestCloseOrphanedTurns(t *testing.T) {
	l, _ := openLedger(t)
	withCall, ended, bare, fresh, done := turn("T"), turn("E"), turn("B"), turn("F"), turn("D")
	ended.EndedAt = t0.Add(time.Minute)
	start := t0.Add(time.Hour)
	fresh.StartedAt = start.Add(time.Second)
	done.EndedAt, done.ActivityClosedAt = t0.Add(time.Minute), t0.Add(2*time.Minute)
	mustUpdate(t, l, func(tx *Tx) error {
		for _, x := range []Turn{withCall, ended, bare, fresh, done} {
			if err := tx.RecordTurn(x); err != nil {
				return err
			}
		}
		return nil
	})
	c := call("c1", modelinfo.Tokens{modelinfo.ClassInput: 1})
	c.BilledAt = t0.Add(30 * time.Minute)
	book(t, l, c)
	n, err := l.CloseOrphanedTurns(start)
	if err != nil || n != 3 {
		t.Fatalf("closed %d (err %v), want 3", n, err)
	}
	for id, want := range map[string]string{
		"T": formatTime(c.BilledAt), "E": formatTime(ended.EndedAt), "B": formatTime(t0),
		"F": "", "D": formatTime(done.ActivityClosedAt),
	} {
		var got sql.NullString
		if err := l.db.QueryRow(`SELECT activity_closed_at FROM turns WHERE turn_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.String != want {
			t.Errorf("turn %s activity = %q, want %q", id, got.String, want)
		}
	}
}

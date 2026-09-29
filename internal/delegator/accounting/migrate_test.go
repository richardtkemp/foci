package accounting

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// v1Schema is the pre-ledger api_calls table (testdata/v1_api_calls.sql).
func v1Schema(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/v1_api_calls.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// v1 is one pre-ledger row to write. turn, when set, is the turn_* group.
type v1 struct {
	id             int64
	ts             time.Time
	session, model string
	callType       string
	fill           [4]int // input, output, cache_read, cache_write
	turn           *[4]int
	searches       int
	cost, calc     *float64
	turnID, subID  string
	purpose        string
	durationMS     int64
}

func f(v float64) *float64 { return &v }

func writeV1(t *testing.T, path string, rows []v1) {
	t.Helper()
	db, err := sqlite.OpenInit(path, v1Schema(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, r := range rows {
		var tIn, tOut, tCR, tCW any
		if r.turn != nil {
			tIn, tOut, tCR, tCW = r.turn[0], r.turn[1], r.turn[2], r.turn[3]
		}
		if _, err := db.Exec(`INSERT INTO api_calls (id, ts, provider, session, model,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, duration_ms,
			stop_reason, call_type, calculated_cost_usd,
			turn_input_tokens, turn_output_tokens, turn_cache_read_tokens, turn_cache_write_tokens,
			turn_id, agent_id, subagent_id, turn_web_searches, purpose)
			VALUES (?, ?, 'anthropic', ?, ?, ?, ?, ?, ?, ?, ?, 'end_turn', ?, ?, ?, ?, ?, ?, ?, 'agent', ?, ?, ?)`,
			r.id, r.ts.Format(time.RFC3339), r.session, r.model,
			r.fill[0], r.fill[1], r.fill[2], r.fill[3], r.cost, r.durationMS, r.callType, r.calc,
			tIn, tOut, tCR, tCW, nullIfEmpty(r.turnID), nullIfEmpty(r.subID), r.searches, nullIfEmpty(r.purpose)); err != nil {
			t.Fatal(err)
		}
	}
}

// priceSplit is what CC would have charged for counts whose cache writes
// split w5m at 5m and the rest at 1h.
func priceSplit(t *testing.T, model string, at time.Time, c [4]int, w5m int) float64 {
	t.Helper()
	usd, ok := modelinfo.CostAsOf(model, at, modelinfo.Tokens{
		modelinfo.ClassInput: c[0], modelinfo.ClassOutput: c[1], modelinfo.ClassCacheRead: c[2],
		modelinfo.ClassCacheWrite5m: w5m, modelinfo.ClassCacheWrite1h: c[3] - w5m,
	})
	if !ok {
		t.Fatalf("%s is not priced", model)
	}
	return usd
}

type migratedCall struct {
	backend, turn, actor, method string
	tokens                       modelinfo.Tokens
	calc                         sql.NullFloat64
}

func readCalls(t *testing.T, db *sql.DB) map[int64]migratedCall {
	t.Helper()
	rows, err := db.Query(`SELECT id, backend, turn_id, actor, class_method, legacy_calculated_cost_usd FROM api_calls`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]migratedCall{}
	for rows.Next() {
		var id int64
		var c migratedCall
		if err := rows.Scan(&id, &c.backend, &c.turn, &c.actor, &c.method, &c.calc); err != nil {
			t.Fatal(err)
		}
		out[id] = c
	}
	_ = rows.Close()
	for id, c := range out {
		tx, _ := db.Begin()
		c.tokens, err = (&Tx{tx: tx}).tokens(id)
		_ = tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		out[id] = c
	}
	return out
}

// TestMigrateV1 is T18 (#2111 §6): every v1 row becomes exactly one legacy call
// with its own id; context fill is copied into turns; the TTL split is solved
// from CC's reported cost only when it lands on an in-range integer, else
// falls back to the record kind; the direct API is always 1h; codex and
// opencode writes are TTL-unknown; backend-reported costs become reports; the
// old recorded figure survives as the audit column; and the report carries
// old-versus-new totals.
func TestMigrateV1(t *testing.T) {
	const opus = "claude/claude-opus-5"
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.FixedZone("BST", 3600))
	s := "helen/c1"
	a := [4]int{10, 500, 90_000, 4_000}
	b := [4]int{12, 800, 95_000, 6_000}
	c := [4]int{8, 300, 99_000, 2_000}
	d := [4]int{20, 900, 100_000, 3_000}
	sub := [4]int{3_000, 1_200, 40_000, 7_000}
	haikuSub := [4]int{100, 50, 1_000, 500}
	costA := 1.0
	costB := costA + priceSplit(t, opus, at, b, 1_234)         // B's writes: 1,234 at 5m — solvable
	costC := costB + priceSplit(t, opus, at, c, 0) - 0.0000123 // x = 3.28: in range, not an integer
	costD := costC + priceSplit(t, opus, at, d, 0) + priceSplit(t, opus, at, sub, sub[3])
	costE := costD + priceSplit(t, opus, at, a, 0) + 50 // x < 0: out of range

	rows := []v1{
		{id: 1, ts: at, session: "helen/api", model: "claude-opus-5", callType: "conversation",
			fill: [4]int{100, 20, 3_000, 700}, calc: f(0.5), durationMS: 1500},
		{id: 10, ts: at, session: s, model: opus, callType: "delegated_turn", fill: [4]int{2, 40, 80_000, 100},
			turn: &a, cost: f(costA), calc: f(0.9), turnID: s + "@1"},
		{id: 11, ts: at, session: s, model: opus, callType: "delegated_turn", fill: [4]int{2, 40, 85_000, 200},
			turn: &b, cost: f(costB), calc: f(1.1), turnID: s + "@2", durationMS: 60_000},
		{id: 12, ts: at, session: s, model: opus, callType: "delegated_turn", fill: [4]int{2, 40, 85_000, 200},
			turn: &c, cost: f(costC), calc: f(1.2), turnID: s + "@3"},
		{id: 13, ts: at, session: s, model: opus, callType: "delegated_turn", fill: [4]int{2, 40, 85_000, 200},
			turn: &d, cost: f(costD), calc: f(1.3), turnID: s + "@4"},
		{id: 14, ts: at, session: s, model: opus, callType: "subagent_turn", turn: &sub, calc: f(0.7),
			turnID: s + "@4", subID: "toolu_A"},
		{id: 15, ts: at, session: s, model: "claude/claude-haiku-4-5-20251001", callType: "subagent_turn",
			turn: &haikuSub, calc: f(0.01), turnID: s + "@4", subID: "toolu_B"},
		{id: 16, ts: at, session: s, model: opus, callType: "delegated_turn", fill: [4]int{2, 40, 85_000, 200},
			turn: &a, cost: f(costE), calc: f(0.8), turnID: s + "@5"},
		// No turn totals and no recorded figure: priced from the un-suffixed four.
		{id: 20, ts: at, session: "helen/c1/b1790000000", model: "claude-opus-4-8", callType: "delegated_turn",
			fill: [4]int{5, 60, 1_000, 300}, turnID: "helen/c1/b1790000000@9"},
		// No turn totals but a recorded figure: its counts are a context-fill
		// snapshot, so it keeps the recorded figure (option B).
		{id: 21, ts: at, session: "helen/c9", model: "claude-opus-4-8", callType: "delegated_turn",
			fill: [4]int{5, 60, 1_000, 300}, calc: f(0.4), turnID: "helen/c9@1"},
		{id: 30, ts: at, session: "arnix/c2", model: "glm-5.2", callType: "delegated_turn",
			fill: [4]int{1_000, 100, 5_000, 900}, cost: f(0), calc: f(0.02), turnID: "arnix/c2@1", purpose: "consolidation"},
		{id: 31, ts: at, session: "codex/c3", model: "codex/gpt-5.6-luna", callType: "delegated_turn",
			fill: [4]int{1_000, 100, 5_000, 900}, cost: f(0.05), calc: f(0.03), turnID: "codex/c3@1"},
		{id: 40, ts: at, session: "helen/api", model: "mystery-model-x", callType: "spawn",
			fill: [4]int{10, 10, 0, 0}, calc: f(0.001)},
	}
	path := filepath.Join(t.TempDir(), "api.db")
	writeV1(t, path, rows)

	l, rep, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if rep == nil {
		t.Fatal("Open on a v1 db ran no migration")
	}

	// ID-set equality, not a count.
	calls := readCalls(t, l.db)
	var ids []int64
	for id := range calls {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if want := []int64{1, 10, 11, 12, 13, 14, 15, 16, 20, 21, 30, 31, 40}; !slices.Equal(ids, want) {
		t.Fatalf("legacy call ids = %v, want the v1 ids %v", ids, want)
	}

	type expect struct {
		backend, method string
		w5m, w1h, wUnk  int
	}
	for id, e := range map[int64]expect{
		1:  {BackendAPI, ClassMethodKind, 0, 700, 0},       // direct API: always 1h
		10: {BackendCCStream, ClassMethodKind, 0, a[3], 0}, // first row of the session: no delta
		11: {BackendCCStream, ClassMethodSolved, 1_234, b[3] - 1_234, 0},
		12: {BackendCCStream, ClassMethodKind, 0, c[3], 0},   // not an integer: rejected
		13: {BackendCCStream, ClassMethodSolved, 0, d[3], 0}, // multi-row window confirms the kind rule
		14: {BackendCCStream, ClassMethodSolved, sub[3], 0, 0},
		15: {BackendCCStream, ClassMethodKind, haikuSub[3], 0, 0}, // other model: outside the window
		16: {BackendCCStream, ClassMethodKind, 0, a[3], 0},        // out of range: rejected
		20: {BackendCCStream, ClassMethodKind, 0, 300, 0},
		21: {BackendCCStream, ClassMethodKind, 0, 300, 0},
		30: {BackendOpencode, ClassMethodUnknown, 0, 0, 900},
		31: {BackendCodex, ClassMethodUnknown, 0, 0, 900},
		40: {BackendAPI, ClassMethodKind, 0, 0, 0},
	} {
		got := calls[id]
		if got.backend != e.backend || got.method != e.method ||
			got.tokens[modelinfo.ClassCacheWrite5m] != e.w5m || got.tokens[modelinfo.ClassCacheWrite1h] != e.w1h ||
			got.tokens[modelinfo.ClassCacheWrite] != e.wUnk {
			t.Errorf("call %d = %s %s %v; want %s %s 5m=%d 1h=%d unknown=%d",
				id, got.backend, got.method, got.tokens, e.backend, e.method, e.w5m, e.w1h, e.wUnk)
		}
	}
	// Priced from the turn totals where they exist, the un-suffixed four where not.
	if got := calls[11].tokens[modelinfo.ClassCacheRead]; got != b[2] {
		t.Errorf("row 11 cache_read = %d, want the turn total %d", got, b[2])
	}
	if got := calls[20].tokens[modelinfo.ClassOutput]; got != 60 {
		t.Errorf("row 20 output = %d, want the un-suffixed 60", got)
	}
	if calls[14].actor != "toolu_A" || calls[14].turn != s+"@4" || calls[1].turn != "legacy:1" {
		t.Errorf("attribution: row 14 actor=%q turn=%q, row 1 turn=%q", calls[14].actor, calls[14].turn, calls[1].turn)
	}
	if !calls[10].calc.Valid || calls[10].calc.Float64 != 0.9 || calls[20].calc.Valid {
		t.Errorf("audit column: row 10 %+v (want 0.9), row 20 %+v (want NULL)", calls[10].calc, calls[20].calc)
	}

	// Cost basis (Dick, 2026-09-29 12:08, option B), recorded per call and
	// honoured by the view: a delegated row with no turn totals keeps its
	// recorded figure; turn-total rows and direct-API rows are re-priced; a
	// no-turn-totals row with no recorded figure has only its counts.
	for id, want := range map[int64]struct {
		basis    string
		recorded float64 // the view's cost when basis is recorded
	}{
		// Direct API, solved rows (their counts reproduce CC's own delta), and a
		// delegated row with no recorded figure: counts.
		1: {CostBasisCounts, 0}, 11: {CostBasisCounts, 0}, 13: {CostBasisCounts, 0}, 14: {CostBasisCounts, 0},
		20: {CostBasisCounts, 0}, 40: {CostBasisCounts, 0},
		// Every other delegated row with a recorded figure — turn totals or
		// not — keeps it: its counts are not provably the bill.
		10: {CostBasisRecorded, 0.9}, 12: {CostBasisRecorded, 1.2}, 15: {CostBasisRecorded, 0.01},
		16: {CostBasisRecorded, 0.8},
		21: {CostBasisRecorded, 0.4}, 30: {CostBasisRecorded, 0.02}, 31: {CostBasisRecorded, 0.03},
	} {
		var basis string
		var cost sql.NullFloat64
		if err := l.db.QueryRow(`SELECT cost_basis, cost_usd FROM call_costs WHERE id = ?`, id).Scan(&basis, &cost); err != nil {
			t.Fatal(err)
		}
		if basis != want.basis {
			t.Errorf("call %d cost_basis = %q, want %q", id, basis, want.basis)
			continue
		}
		counts, priced := modelinfo.CostAsOf(rows[slices.IndexFunc(rows, func(r v1) bool { return r.id == id })].model, at, calls[id].tokens)
		switch {
		case want.basis == CostBasisRecorded && (!cost.Valid || cost.Float64 != want.recorded):
			t.Errorf("call %d (recorded) costs %+v, want its recorded $%v", id, cost, want.recorded)
		case want.basis == CostBasisCounts && priced && (!cost.Valid || math.Abs(cost.Float64-counts) > 1e-12):
			t.Errorf("call %d (counts) costs %+v, want its counts re-priced, $%v", id, cost, counts)
		}
	}
	// A recorded-basis call has no class split to offer.
	var classRows int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM turn_class_costs WHERE turn_id = 'helen/c9@1'`).Scan(&classRows)
	if classRows != 0 {
		t.Errorf("turn_class_costs split a recorded-basis call into %d classes", classRows)
	}
	if rep.Bases[CostBasisRecorded] != 7 || rep.Bases[CostBasisCounts] != 6 {
		t.Errorf("report bases = %v, want 7 recorded and 6 counts", rep.Bases)
	}

	// Turns: fill COPIED from the turn's own row, source inferred.
	type turnRow struct {
		source  string
		fill    int
		ended   string
		running bool
		calls   int
	}
	readTurn := func(id string) turnRow {
		t.Helper()
		var tr turnRow
		if err := l.db.QueryRow(`SELECT source, context_fill, COALESCE(ended_at, ''), still_running, calls
			FROM turn_costs WHERE turn_id = ?`, id).Scan(&tr.source, &tr.fill, &tr.ended, &tr.running, &tr.calls); err != nil {
			t.Fatalf("turn %q: %v", id, err)
		}
		return tr
	}
	if tr := readTurn(s + "@2"); tr.source != SourceUser || tr.fill != 2+85_000+200 || tr.running ||
		tr.ended != formatTime(at.Add(time.Minute)) {
		t.Errorf("turn @2 = %+v; want user, fill copied from its row (85202), closed at start+60s", tr)
	}
	if tr := readTurn(s + "@4"); tr.calls != 3 || tr.fill != 2+85_000+200 {
		t.Errorf("turn @4 = %+v; want 3 calls (parent + 2 subagents) and the parent row's fill", tr)
	}
	if tr := readTurn("helen/c1/b1790000000@9"); tr.source != SourceKeepalive {
		t.Errorf("keepalive fork turn source = %q", tr.source)
	}
	if tr := readTurn("arnix/c2@1"); tr.source != SourceBatch {
		t.Errorf("batch turn source = %q", tr.source)
	}
	if tr := readTurn("legacy:1"); tr.fill != 100+3_000+700 {
		t.Errorf("api turn fill = %d, want 3800", tr.fill)
	}

	// Reports: CC's cumulative figure per row, opencode's per call; none for
	// codex (it reports no cost) or the direct API.
	var ccReports, ocReports, other int
	if err := l.db.QueryRow(`SELECT
		SUM(backend = 'ccstream' AND grain = 'cumulative'), SUM(backend = 'opencode' AND grain = 'per_call'),
		SUM(backend NOT IN ('ccstream', 'opencode')) FROM backend_reports`).Scan(&ccReports, &ocReports, &other); err != nil {
		t.Fatal(err)
	}
	if ccReports != 5 || ocReports != 1 || other != 0 {
		t.Errorf("reports: %d CC cumulative, %d opencode per-call, %d other; want 5, 1, 0", ccReports, ocReports, other)
	}

	// The v1 table is gone; the backup is a v1 db with every row.
	var v1Left int
	_ = l.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'api_calls_v1'`).Scan(&v1Left)
	if v1Left != 0 {
		t.Error("api_calls_v1 survived the migration")
	}
	bk, err := sqlite.OpenReadOnly(rep.BackupPath)
	if err != nil {
		t.Fatalf("backup %q: %v", rep.BackupPath, err)
	}
	var bkRows int
	_ = bk.QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&bkRows)
	_ = bk.Close()
	if bkRows != len(rows) {
		t.Errorf("backup holds %d rows, want %d", bkRows, len(rows))
	}

	// The report: solve outcomes, alarms counted not delivered, old vs new.
	if s := rep.Solve; s.Windows != 4 || s.Accepted != 2 || s.Rejected != 2 {
		t.Errorf("solve = %+v; want 4 windows, 2 accepted, 2 rejected", s)
	}
	if rep.Alarms[InvModelNotInTable] != 1 {
		t.Errorf("alarms = %v, want the mystery model counted once", rep.Alarms)
	}
	wantOld := 0.5 + 0.9 + 1.1 + 1.2 + 1.3 + 0.7 + 0.01 + 0.8 + 0.4 + 0.02 + 0.03 + 0.001
	if math.Abs(rep.OldCalculated-wantOld) > 1e-9 {
		t.Errorf("old calculated = %v, want %v", rep.OldCalculated, wantOld)
	}
	var wantNew float64
	var unpriced int
	for _, id := range ids {
		var cost sql.NullFloat64
		_ = l.db.QueryRow(`SELECT cost_usd FROM call_costs WHERE id = ?`, id).Scan(&cost)
		if cost.Valid {
			wantNew += cost.Float64
		} else {
			unpriced++
		}
	}
	if math.Abs(rep.New-wantNew) > 1e-9 || rep.UnpricedCalls != unpriced {
		t.Errorf("report new = $%v with %d unpriced; want $%v with %d", rep.New, rep.UnpricedCalls, wantNew, unpriced)
	}
	var mystery sql.NullFloat64
	_ = l.db.QueryRow(`SELECT cost_usd FROM call_costs WHERE id = 40`).Scan(&mystery)
	if mystery.Valid {
		t.Errorf("the unknown model's legacy call costs $%v, want NULL", mystery.Float64)
	}
	var groups []string
	for _, g := range rep.Groups {
		groups = append(groups, fmt.Sprintf("%s/%v/%d", g.Backend, g.TurnTotals, g.Rows))
	}
	if want := []string{"api/false/2", "ccstream/false/2", "ccstream/true/7", "codex/false/1", "opencode/false/1"}; !slices.Equal(groups, want) {
		t.Errorf("report groups = %v, want %v", groups, want)
	}
	if len(rep.Days) != 1 || rep.Days[0].Day != "2026-09-20" {
		t.Errorf("days = %+v, want one UTC day", rep.Days)
	}
	if rep.String() == "" {
		t.Error("empty report text")
	}

	// Idempotent: reopening migrates nothing and changes nothing.
	_ = l.Close()
	l2, rep2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l2.Close() }()
	if rep2 != nil {
		t.Fatalf("second Open migrated again: %+v", rep2)
	}
	if again := readCalls(t, l2.db); len(again) != len(calls) {
		t.Errorf("second Open: %d calls, want %d", len(again), len(calls))
	}
}

// TestMigrateV1FailureChangesNothing: a migration that cannot complete rolls
// back whole — the v1 table is still there, untouched, and no ledger table
// exists.
func TestMigrateV1FailureChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	writeV1(t, path, []v1{
		{id: 1, ts: t0, session: "s", model: "claude-opus-5", callType: "conversation", fill: [4]int{1, 1, 1, 1}},
		{id: 2, ts: t0, session: "s", model: "claude-opus-5", callType: "conversation", fill: [4]int{1, 1, 1, 1}},
	})
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE api_calls SET ts = 'not a time' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if _, _, err := Open(path, Options{NoBackup: true}); err == nil {
		t.Fatal("Open succeeded on a v1 row with an unparseable timestamp")
	}
	db, err = sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if v1, err := isV1(db); err != nil || !v1 {
		t.Fatalf("after a failed migration api_calls is not v1 (v1=%v err=%v)", v1, err)
	}
	var n, ledgerTables int
	_ = db.QueryRow(`SELECT COUNT(*) FROM api_calls`).Scan(&n)
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('turns', 'call_tokens', 'api_calls_v1')`).Scan(&ledgerTables)
	if n != 2 || ledgerTables != 0 {
		t.Errorf("after a failed migration: %d v1 rows (want 2), %d ledger tables (want 0)", n, ledgerTables)
	}
	if backups, _ := filepath.Glob(path + ".pre-ledger-*"); len(backups) != 0 {
		t.Errorf("NoBackup wrote a backup: %v", backups)
	}
}

// TestMigrateV1HelperTurnsAndUnnamedShares: a pre-ledger summary or spawn
// row was a direct-API helper call, not a turn of the conversation, so its
// turn is a system turn (as a live one is); a subagent share whose usage
// arrived before anything named its subagent is still a subagent's call, not
// the session's own thread; and every legacy call records which counts it
// carries.
func TestMigrateV1HelperTurnsAndUnnamedShares(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.db")
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	writeV1(t, path, []v1{
		{id: 1, ts: at, session: "a/c1", model: "claude-opus-5", callType: "summary", fill: [4]int{10, 5, 0, 0}},
		{id: 2, ts: at, session: "a/c1", model: "claude-opus-5", callType: "spawn", fill: [4]int{10, 5, 0, 0}},
		{id: 3, ts: at, session: "a/c1", model: "claude-opus-5", callType: "delegated_turn", turnID: "a/c1@1",
			fill: [4]int{1, 2, 3, 4}, turn: &[4]int{5, 6, 7, 8}, calc: f(0.1)},
		{id: 4, ts: at, session: "a/c1", model: "claude-opus-5", callType: "subagent_turn", turnID: "a/c1@1",
			turn: &[4]int{1, 1, 1, 1}, calc: f(0.01)},
	})
	l, rep, err := Open(path, Options{NoBackup: true})
	if err != nil || rep == nil {
		t.Fatalf("Open: %v %v", rep, err)
	}
	defer func() { _ = l.Close() }()
	for _, tc := range []struct {
		id     int64
		source string
		actor  string
		totals bool
	}{
		{1, SourceSystem, "", false},
		{2, SourceSystem, "", false},
		{3, SourceUser, "", true},
		{4, SourceUser, UnnamedSubagent, true},
	} {
		var source, actor string
		var totals bool
		if err := l.db.QueryRow(`SELECT t.source, c.actor, json_extract(c.detail, '$.turn_totals')
			FROM api_calls c JOIN turns t ON t.turn_id = c.turn_id WHERE c.id = ?`, tc.id).Scan(&source, &actor, &totals); err != nil {
			t.Fatalf("row %d: %v", tc.id, err)
		}
		if source != tc.source || actor != tc.actor || totals != tc.totals {
			t.Errorf("row %d: source %q actor %q turn_totals %v, want %q %q %v",
				tc.id, source, actor, totals, tc.source, tc.actor, tc.totals)
		}
	}
}

package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"foci/internal/modelinfo"
)

// Options configures Open.
type Options struct {
	// BackupPath is where Open writes the pre-migration VACUUM INTO copy when
	// it finds a pre-ledger api.db. Empty means "<path>.pre-ledger-<stamp>".
	BackupPath string
	// NoBackup skips the backup. Only for migrating a copy that is itself the
	// backup (the dry-run tool).
	NoBackup bool
	// Shadow opens a SHADOW ledger: a scratch api.db an adapter books into
	// beside the live path while it is verified, never observed (no api.jsonl
	// line, no generation) and never read by the product (#2111 §12).
	Shadow bool
	// OnAlarm receives invariant violations from live bookings.
	OnAlarm func(Alarm)
}

// Open opens the ledger in the api.db at path, creating its schema and
// re-rendering its rate tables from modelinfo. A pre-ledger api.db (the v1
// api_calls table) is migrated first, under a backup: the returned report is
// non-nil exactly when a migration ran.
func Open(path string, opts Options) (*Ledger, *MigrationReport, error) {
	db, err := open(path)
	if err != nil {
		return nil, nil, err
	}
	l := &Ledger{db: db, OnAlarm: opts.OnAlarm, shadow: opts.Shadow}
	v1, err := isV1(db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	var report *MigrationReport
	if v1 {
		if report, err = l.migrateV1(path, opts); err != nil {
			_ = db.Close()
			return nil, nil, err
		}
	} else if err := l.Update(func(tx *Tx) error {
		if err := ensureSchema(tx.tx); err != nil {
			return err
		}
		return renderRates(tx.tx)
	}); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return l, report, nil
}

// isV1 reports whether db holds the pre-ledger api_calls table: one row per
// turn, with foci's priced figure in calculated_cost_usd.
func isV1(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('api_calls') WHERE name = 'calculated_cost_usd'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("ledger: inspect api_calls: %w", err)
	}
	return n > 0, nil
}

// v1Row is one pre-ledger api_calls row.
type v1Row struct {
	id          int64
	ts          time.Time
	provider    string
	session     string
	model       string
	fill        LegacyFill // the un-suffixed four: a delegated turn's final-cycle context fill
	costUSD     sql.NullFloat64
	calcUSD     sql.NullFloat64
	durationMS  int64
	stopReason  string
	callType    string
	sessionFile string
	sessionLine int
	preMessages int
	turnID      string
	agentID     string
	subagentID  string
	purpose     string

	// priced is what the row's cost was priced from — the turn totals where
	// the writer measured them, else the un-suffixed four (the old
	// PricedCounts rule) — with its cache writes not yet given a TTL.
	// turnTotals says which.
	priced     modelinfo.TokenCounts
	turnTotals bool

	backend string
	// writes5m/writes1h/writesUnknown split priced.CacheWrite by TTL class.
	writes5m, writes1h, writesUnknown int
	classMethod                       string
	costBasis                         string
}

// tokens is the row's counts with its cache writes classed.
func (r *v1Row) tokens() modelinfo.Tokens {
	return modelinfo.Tokens{
		modelinfo.ClassInput:        r.priced.Input,
		modelinfo.ClassOutput:       r.priced.Output,
		modelinfo.ClassCacheRead:    r.priced.CacheRead,
		modelinfo.ClassCacheWrite5m: r.writes5m,
		modelinfo.ClassCacheWrite1h: r.writes1h,
		modelinfo.ClassCacheWrite:   r.writesUnknown,
		modelinfo.ClassWebSearch:    r.priced.WebSearches,
	}
}

func (r *v1Row) isSubagent() bool { return r.callType == "subagent_turn" }

// turnKey is the ledger turn the row belongs to. A row with no turn id gets a
// turn of its own (R3: only overhead has no turn).
func (r *v1Row) turnKey() string {
	if r.turnID != "" {
		return r.turnID
	}
	return fmt.Sprintf("legacy:%d", r.id)
}

// oldCost is what the pre-ledger readers showed for the row: foci's recorded
// figure, else a live estimate from its counts (the old EffectiveCost).
func (r *v1Row) oldCost() float64 {
	if r.calcUSD.Valid {
		return r.calcUSD.Float64
	}
	return r.priced.CostAsOf(r.model, r.ts)
}

const v1Select = `SELECT id, ts, COALESCE(provider, ''), session, model,
	COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
	COALESCE(cache_read_tokens, 0), COALESCE(cache_write_tokens, 0),
	cost_usd, calculated_cost_usd, COALESCE(duration_ms, 0), COALESCE(stop_reason, ''), call_type,
	COALESCE(session_file, ''), COALESCE(session_line, 0), COALESCE(pre_messages, 0),
	turn_input_tokens, turn_cache_read_tokens, turn_cache_write_tokens, turn_output_tokens,
	COALESCE(turn_web_searches, 0),
	COALESCE(turn_id, ''), COALESCE(agent_id, ''), COALESCE(subagent_id, ''), COALESCE(purpose, '')
	FROM api_calls_v1 ORDER BY id`

// readV1 reads every pre-ledger row from api_calls_v1, in id order.
func readV1(tx *sql.Tx) ([]*v1Row, error) {
	rows, err := tx.Query(v1Select)
	if err != nil {
		return nil, fmt.Errorf("ledger migration: read api_calls_v1: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*v1Row
	for rows.Next() {
		r := &v1Row{}
		var ts string
		var tIn, tCR, tCW, tOut sql.NullInt64
		if err := rows.Scan(&r.id, &ts, &r.provider, &r.session, &r.model,
			&r.fill.Input, &r.fill.Output, &r.fill.CacheRead, &r.fill.CacheWrite,
			&r.costUSD, &r.calcUSD, &r.durationMS, &r.stopReason, &r.callType,
			&r.sessionFile, &r.sessionLine, &r.preMessages,
			&tIn, &tCR, &tCW, &tOut, &r.priced.WebSearches,
			&r.turnID, &r.agentID, &r.subagentID, &r.purpose); err != nil {
			return nil, fmt.Errorf("ledger migration: scan api_calls_v1: %w", err)
		}
		if r.ts, err = time.Parse(time.RFC3339, ts); err != nil {
			return nil, fmt.Errorf("ledger migration: row %d: timestamp %q: %w", r.id, ts, err)
		}
		// The PricedCounts rule: the turn totals when the writer measured them
		// (written as a group), else the un-suffixed four. Output falls back
		// to output_tokens for rows written before turn_output_tokens existed.
		if r.turnTotals = tIn.Valid; r.turnTotals {
			r.priced.Input, r.priced.CacheRead, r.priced.CacheWrite = int(tIn.Int64), int(tCR.Int64), int(tCW.Int64)
			r.priced.Output = r.fill.Output
			if tOut.Valid {
				r.priced.Output = int(tOut.Int64)
			}
		} else {
			r.priced.Input, r.priced.Output = r.fill.Input, r.fill.Output
			r.priced.CacheRead, r.priced.CacheWrite = r.fill.CacheRead, r.fill.CacheWrite
		}
		r.backend = inferBackend(r)
		out = append(out, r)
	}
	return out, rows.Err()
}

// inferBackend names the backend that wrote a pre-ledger row, which v1 never
// recorded. Every non-delegated call type is the direct API. A delegated row
// is codex when its model is a codex one (codex/-prefixed, a gpt- id, or the
// bare "luna" alias); CC when it is a Claude model, CC's
// synthetic sentinel, or empty (CC rows with no model yet); otherwise opencode,
// the only other delegated backend foci has run.
func inferBackend(r *v1Row) string {
	if r.callType != "delegated_turn" && r.callType != "subagent_turn" {
		return BackendAPI
	}
	m := strings.ToLower(r.model)
	bare := modelinfo.StripPrefix(m)
	switch {
	case strings.HasPrefix(m, "codex/"), strings.HasPrefix(bare, "gpt-"), bare == "luna":
		return BackendCodex
	case m == "", strings.Contains(m, "claude"), modelinfo.IsSynthetic(bare):
		return BackendCCStream
	}
	return BackendOpencode
}

// classifyByKind applies the record-kind TTL rule (#2111 §6 step 5.2): CC's
// main thread writes at 1h and its subagents at 5m; the direct API always
// wrote at 1h (Dick); every other backend's TTL is unknown.
func classifyByKind(r *v1Row) {
	w := r.priced.CacheWrite
	r.writes5m, r.writes1h, r.writesUnknown = 0, 0, 0
	switch {
	case r.backend == BackendCCStream && r.isSubagent():
		r.writes5m, r.classMethod = w, ClassMethodKind
	case r.backend == BackendCCStream, r.backend == BackendAPI:
		r.writes1h, r.classMethod = w, ClassMethodKind
	default:
		r.writesUnknown, r.classMethod = w, ClassMethodUnknown
	}
}

// SolveTolerance is how far from an integer a solved 5m write count may be
// (#2111 §6 step 5.1): float rounding only, so a solve is exact or refused.
// The migration and the CC adapter's remainder solve (#2130) share it.
const SolveTolerance = 1e-6

// solveStats counts the TTL solve's outcomes.
type solveStats struct {
	Windows     int // windows with a usable reported-cost delta
	Accepted    int // solved to an in-range integer and allocated
	Rejected    int // not an integer, or out of range
	Disagreed   int // multi-row window whose solution did not equal its subagent writes
	Unpriceable int // no rate group, or no distinct 5m and 1h rates
}

// solveTTL applies the reported-cost solve to CC rows (#2111 §6 step 5.1).
//
// CC's cost_usd on a v1 row is the process's CUMULATIVE cost on the row's
// model (#1674). Between two consecutive rows of one session on one model, a
// non-negative delta is therefore CC's own cost of one window: the later
// parent row plus the subagent rows written with it on that model. With every
// other class priced from its counts, the window's 5m cache-write count x is
// the one unknown:
//
//	x = (window priced with every write at 1h − delta) / (rate_1h − rate_5m)
//
// x is accepted only as an integer in [0, window writes]. A one-row window
// takes x as its own split. A multi-row window is accepted only when x equals
// its subagent rows' writes — which confirms the kind rule — since one unknown
// cannot be allocated across several rows. foci's own calculated figure is
// never used: solving from it would reproduce foci's own errors.
func solveTTL(rows []*v1Row) solveStats {
	var st solveStats
	bySession := map[string][]*v1Row{}
	for _, r := range rows {
		if r.backend == BackendCCStream {
			bySession[r.session] = append(bySession[r.session], r)
		}
	}
	for _, sess := range slices.Sorted(maps.Keys(bySession)) {
		rs := bySession[sess]
		var prev *v1Row
		for i, r := range rs {
			if r.isSubagent() || !r.costUSD.Valid {
				continue
			}
			window := []*v1Row{r}
			for _, s := range rs[i+1:] {
				if !s.isSubagent() {
					break
				}
				if s.model == r.model {
					window = append(window, s)
				}
			}
			if prev != nil && prev.model == r.model && r.costUSD.Float64 >= prev.costUSD.Float64 {
				st.solveWindow(window, r.costUSD.Float64-prev.costUSD.Float64)
			}
			prev = r
		}
	}
	return st
}

// solveWindow solves one window against CC's delta and, when accepted,
// allocates the solution.
func (st *solveStats) solveWindow(window []*v1Row, delta float64) {
	st.Windows++
	head := window[0]
	// A window is a sum of calls: base rates, never a prompt-size tier.
	rm, ok := modelinfo.ResolveRateModel(head.model, head.ts, 0)
	if !ok {
		st.Unpriceable++
		return
	}
	rates, _ := modelinfo.RatesAsOf(rm, head.ts)
	r5m, has5m := rates[modelinfo.ClassCacheWrite5m]
	r1h, has1h := rates[modelinfo.ClassCacheWrite1h]
	if !has5m || !has1h || r1h <= r5m {
		st.Unpriceable++
		return
	}
	var sum modelinfo.TokenCounts
	subWrites := 0
	for _, r := range window {
		sum = sum.Add(r.priced)
		if r.isSubagent() {
			subWrites += r.priced.CacheWrite
		}
	}
	all1h, priced := modelinfo.CostAsOfPrompt(head.model, head.ts, sum.Tokens(modelinfo.ClassCacheWrite1h), 0)
	if !priced {
		st.Unpriceable++
		return
	}
	x := (all1h - delta) / ((r1h - r5m) / 1_000_000)
	n := math.Round(x)
	if math.Abs(x-n) > SolveTolerance || n < 0 || int(n) > sum.CacheWrite {
		st.Rejected++
		return
	}
	x5m := int(n)
	if len(window) > 1 && x5m != subWrites {
		st.Disagreed++
		return
	}
	st.Accepted++
	if len(window) == 1 {
		head.writes5m, head.writes1h, head.writesUnknown = x5m, head.priced.CacheWrite-x5m, 0
		head.classMethod = ClassMethodSolved
		return
	}
	for _, r := range window {
		r.writes5m, r.writes1h, r.writesUnknown = 0, r.priced.CacheWrite, 0
		if r.isSubagent() {
			r.writes5m, r.writes1h = r.priced.CacheWrite, 0
		}
		r.classMethod = ClassMethodSolved
	}
}

// keepaliveSession matches a keepalive fork's session key: the parent key
// with a "/b<digits>" suffix.
var keepaliveSession = regexp.MustCompile(`/b\d+$`)

// legacyTurn builds the ledger turn for one pre-ledger turn's rows. Its facts
// come from the turn's own row (the first non-subagent row), falling back to
// the first row for a turn only subagent rows survive for.
func legacyTurn(key string, rows []*v1Row) Turn {
	head := rows[0]
	for _, r := range rows {
		if !r.isSubagent() {
			head = r
			break
		}
	}
	source := SourceUser
	switch {
	case keepaliveSession.MatchString(head.session):
		source = SourceKeepalive
	case head.purpose != "":
		source = SourceBatch
	case head.callType == "compaction":
		source = SourceCompaction
	case head.callType == "summary", head.callType == "spawn":
		// A direct-API helper call, not a turn of the conversation: the
		// ledger books these on turns of their own, marked system.
		source = SourceSystem
	}
	ended := head.ts.Add(time.Duration(head.durationMS) * time.Millisecond)
	t := Turn{
		TurnID: key, Session: head.session, AgentID: head.agentID, Backend: head.backend,
		Source: source, Purpose: head.purpose,
		StartedAt: head.ts, EndedAt: ended, ActivityClosedAt: ended,
		StopReason: head.stopReason, FinalModel: head.model,
	}
	// Context fill is COPIED, never derived (R4): a summed row is not a
	// context size. A subagent row has no fill of its own.
	if !head.isSubagent() {
		fill := head.fill
		t.Legacy = &fill
	}
	return t
}

// legacyCostBasis decides where the views take a legacy row's cost from
// (Dick, 2026-09-29 12:08, option B: "any row whose tokens aren't the billed
// figure" keeps its recorded figure). Run AFTER the TTL solve.
//
// A direct-API row is one call, so its counts are its bill: re-priced. A
// delegated row's counts are the billed figure only where CC's own reported
// cost proves it — a SOLVED row, whose counts at the solved split reproduce
// CC's delta exactly. Every other delegated row keeps its recorded figure,
// because its counts demonstrably are not the bill:
//   - no turn totals (ccstream before #1854, every codex and opencode row):
//     the un-suffixed columns are the final cycle's context fill;
//   - turn totals written before 2026-09-26: turn_output_tokens was wiped on
//     every restart, so output falls back to output_tokens, which on 59 opus-5
//     rows alone carries $108 of output CC never billed (CC's own delta
//     matches the recorded figure, not the counts);
//   - turn totals are cross-model sums (#1866 P3) with an unobserved TTL
//     split, re-priced here at the row's single model and the kind rule — the
//     recorded figure priced each model at its own rate and each write at its
//     observed TTL. Against CC's own delta on subagent-free windows the
//     recorded figure is the closer one (see clutch notes/2113.md).
//
// A delegated row with no recorded figure has nothing else to price from and
// stays on its counts, as the old EffectiveCost did.
func legacyCostBasis(r *v1Row) string {
	if r.backend == BackendAPI || !r.calcUSD.Valid || r.classMethod == ClassMethodSolved {
		return CostBasisCounts
	}
	return CostBasisRecorded
}

// UnnamedSubagent is the actor of a subagent's call whose subagent was never
// named (usage that arrived before its task_started).
const UnnamedSubagent = "(unnamed)"

// legacyCall builds the ledger call for one pre-ledger row.
func legacyCall(r *v1Row) Call {
	// turn_totals records which counts the row carries: the turn_* group the
	// writer measured, or the un-suffixed four (a delegated turn's final-cycle
	// context fill before #1854).
	detail := map[string]any{"v1_call_type": r.callType, "turn_totals": r.turnTotals}
	if r.durationMS > 0 {
		detail["duration_ms"] = r.durationMS
	}
	if r.preMessages > 0 {
		detail["pre_messages"] = r.preMessages
	}
	var calc *float64
	if r.calcUSD.Valid {
		v := r.calcUSD.Float64
		calc = &v
	}
	// A subagent share whose usage arrived before anything named its
	// subagent still is one: actor "" would make it the session's own thread.
	actor := r.subagentID
	if r.isSubagent() && actor == "" {
		actor = UnnamedSubagent
	}
	return Call{
		Backend: r.backend, Provider: r.provider, Model: r.model,
		Session: r.session, AgentID: r.agentID, TurnID: r.turnKey(), Actor: actor,
		Kind: KindLegacy, Finality: FinalityLegacy, ClassMethod: r.classMethod,
		BilledAt: r.ts, Tokens: r.tokens(), StopReason: r.stopReason,
		SessionFile: r.sessionFile, SessionLine: r.sessionLine,
		Detail: detail, LegacyID: r.id, LegacyCalculatedCostUSD: calc,
		CostBasis: r.costBasis,
	}
}

// legacyReport is the backend's own figure a pre-ledger row carried, if any:
// CC's cumulative per-process cost on the row's model, or opencode's cost for
// the turn. codex reports no cost (#2112 P0-c) and the direct API none at all.
func legacyReport(r *v1Row) (Report, bool) {
	if !r.costUSD.Valid || r.isSubagent() {
		return Report{}, false
	}
	cost := r.costUSD.Float64
	switch r.backend {
	case BackendCCStream:
		return Report{Backend: r.backend, Session: r.session, ScopeKey: r.session, Model: r.model,
			Grain: GrainCumulative, At: r.ts, CostUSD: &cost}, true
	case BackendOpencode:
		return Report{Backend: r.backend, Session: r.session, ScopeKey: r.turnKey(), Model: r.model,
			Grain: GrainPerCall, At: r.ts, CostUSD: &cost, Tokens: r.priced.Tokens(modelinfo.ClassCacheWrite)}, true
	}
	return Report{}, false
}

// migrateV1 converts a pre-ledger api.db in place (#2111 §6): each v1 row
// becomes one legacy call, each v1 turn one turn, each backend-reported cost
// one report. It runs in ONE transaction and verifies by id set and token
// totals before dropping the v1 table, so it either completes or leaves the
// database exactly as it was.
func (l *Ledger) migrateV1(path string, opts Options) (*MigrationReport, error) {
	started := time.Now()
	rep := &MigrationReport{}
	if !opts.NoBackup {
		rep.BackupPath = opts.BackupPath
		if rep.BackupPath == "" {
			rep.BackupPath = path + ".pre-ledger-" + started.Format("20060102-150405")
		}
		if _, err := l.db.Exec(`VACUUM INTO ?`, rep.BackupPath); err != nil {
			return nil, fmt.Errorf("ledger migration: backup to %s: %w", rep.BackupPath, err)
		}
	}

	var v1 []*v1Row
	err := l.Update(func(tx *Tx) error {
		tx.quiet = true
		if _, err := tx.tx.Exec(`ALTER TABLE api_calls RENAME TO api_calls_v1`); err != nil {
			return fmt.Errorf("ledger migration: rename v1: %w", err)
		}
		var err error
		if v1, err = readV1(tx.tx); err != nil {
			return err
		}
		if err := ensureSchema(tx.tx); err != nil {
			return err
		}
		if err := renderRates(tx.tx); err != nil {
			return err
		}
		for _, r := range v1 {
			classifyByKind(r)
		}
		rep.Solve = solveTTL(v1)
		for _, r := range v1 {
			r.costBasis = legacyCostBasis(r)
		}

		byTurn := map[string][]*v1Row{}
		for _, r := range v1 {
			byTurn[r.turnKey()] = append(byTurn[r.turnKey()], r)
		}
		for _, key := range slices.Sorted(maps.Keys(byTurn)) {
			if err := tx.RecordTurn(legacyTurn(key, byTurn[key])); err != nil {
				return err
			}
		}
		for _, r := range v1 {
			if _, err := tx.Book(legacyCall(r)); err != nil {
				return err
			}
			if rp, ok := legacyReport(r); ok {
				if err := tx.Report(rp); err != nil {
					return err
				}
			}
		}
		// History is reported, not alarmed: a pre-ledger model with no rate
		// group is a fact about the past, and 50k of them in operator chat
		// would bury every live alarm.
		rep.Alarms = map[string]int{}
		for _, a := range tx.alarms {
			rep.Alarms[a.Invariant]++
		}
		tx.alarms = nil

		if err := verifyMigration(tx.tx, v1); err != nil {
			return err
		}
		if _, err := tx.tx.Exec(`DROP TABLE api_calls_v1`); err != nil {
			return fmt.Errorf("ledger migration: drop v1: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := rep.tally(l.db, v1); err != nil {
		return nil, err
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// verifyMigration checks the conversion inside its transaction: the legacy
// calls' ids are exactly the v1 ids (a set comparison, not a count), and every
// token class and the audit figure sum to what the v1 rows held.
func verifyMigration(tx *sql.Tx, v1 []*v1Row) error {
	var missing, extra int
	if err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM (SELECT id FROM api_calls_v1 EXCEPT SELECT id FROM api_calls WHERE kind = 'legacy')),
		(SELECT COUNT(*) FROM (SELECT id FROM api_calls WHERE kind = 'legacy' EXCEPT SELECT id FROM api_calls_v1))`).
		Scan(&missing, &extra); err != nil {
		return fmt.Errorf("ledger migration: verify ids: %w", err)
	}
	var errs []error
	if missing != 0 || extra != 0 {
		errs = append(errs, fmt.Errorf("id sets differ: %d v1 rows not carried over, %d legacy calls with no v1 row", missing, extra))
	}

	want := modelinfo.Tokens{}
	var wantCalc float64
	for _, r := range v1 {
		for c, n := range r.tokens() {
			want[c] += n
		}
		if r.calcUSD.Valid {
			wantCalc += r.calcUSD.Float64
		}
	}
	got := modelinfo.Tokens{}
	rows, err := tx.Query(`SELECT class, SUM(count) FROM call_tokens GROUP BY class`)
	if err != nil {
		return fmt.Errorf("ledger migration: verify tokens: %w", err)
	}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			_ = rows.Close()
			return fmt.Errorf("ledger migration: verify tokens: %w", err)
		}
		got[modelinfo.Class(c)] = n
	}
	_ = rows.Close()
	if !maps.Equal(got, nonZero(want)) {
		errs = append(errs, fmt.Errorf("token totals differ: v1 %v, ledger %v", nonZero(want), got))
	}
	var gotCalc float64
	if err := tx.QueryRow(`SELECT TOTAL(legacy_calculated_cost_usd) FROM api_calls`).Scan(&gotCalc); err != nil {
		return fmt.Errorf("ledger migration: verify audit column: %w", err)
	}
	if math.Abs(gotCalc-wantCalc) > 1e-6 {
		errs = append(errs, fmt.Errorf("audit column total $%.6f, v1 calculated_cost_usd total $%.6f", gotCalc, wantCalc))
	}
	if len(errs) > 0 {
		return fmt.Errorf("ledger migration: verification failed, nothing changed: %w", errors.Join(errs...))
	}
	return nil
}

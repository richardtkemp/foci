package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"foci/internal/delegator/ccstream"
	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// runLedgerShadow is `foci-gw ledger-shadow`: it compares a shadow ledger (the
// Claude Code adapter booking beside the live path, logging.api_shadow_db,
// #2111 §12) with the live one over the same window, read-only, so the switch
// is decided on evidence. Three comparisons:
//
//   - per turn: the live turn-level rows against the adapter's per-call rows,
//     on every turn both hold;
//   - per CC process: CC's OWN cumulative cost (a report − the baseline it
//     started from) against the adapter's price of the calls that report
//     counts — the design's divergence check, interrupted calls excluded
//     because CC counts them nowhere (shadowProcesses);
//   - totals and per UTC day, with the adapter's remainder (overhead and
//     compaction) and interrupted spend broken out.
func runLedgerShadow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ledger-shadow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	live := fs.String("live", "", "the live api.db (read-only)")
	shadow := fs.String("shadow", "", "the shadow api.db (read-only)")
	since := fs.String("since", "", "window start: RFC3339 or a duration ago (e.g. 36h); default: the shadow's first call")
	top := fs.Int("top", 15, "how many of the most divergent turns and processes to list")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `foci-gw ledger-shadow — compare the shadow cost ledger with the live one (#2111)

Usage: foci-gw ledger-shadow -live <api.db> -shadow <api-shadow.db> [-since <time|duration>] [-top N]

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *live == "" || *shadow == "" {
		fs.Usage()
		return 2
	}
	db, err := sqlite.OpenReadOnly(*live)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-shadow: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // the ATTACH belongs to one connection
	if _, err := db.Exec(`ATTACH DATABASE ? AS shadow`, "file:"+*shadow+"?mode=ro"); err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-shadow: attach %s: %v\n", *shadow, err)
		return 1
	}
	from, err := shadowSince(db, *since)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-shadow: %v\n", err)
		return 1
	}
	if err := writeShadowReport(db, from, *top, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger-shadow: %v\n", err)
		return 1
	}
	return 0
}

// shadowSince resolves -since: a timestamp, a duration ago, or the shadow's
// first booked call.
func shadowSince(db *sql.DB, since string) (string, error) {
	const layout = "2006-01-02T15:04:05.000000000Z"
	switch {
	case since == "":
		var first sql.NullString
		if err := db.QueryRow(`SELECT MIN(billed_at) FROM shadow.api_calls`).Scan(&first); err != nil {
			return "", fmt.Errorf("read the shadow: %w", err)
		}
		if !first.Valid {
			return "", fmt.Errorf("the shadow ledger holds no calls")
		}
		return first.String, nil
	default:
		if d, err := time.ParseDuration(since); err == nil {
			return time.Now().Add(-d).UTC().Format(layout), nil
		}
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return "", fmt.Errorf("-since %q: want RFC3339 or a duration", since)
		}
		return t.UTC().Format(layout), nil
	}
}

type shadowPair struct {
	key          string
	live, shadow float64
}

func (p shadowPair) diff() float64 { return p.shadow - p.live }

func writeShadowReport(db *sql.DB, from string, top int, w io.Writer) error {
	var liveTotal, shadowTotal, remainder, interrupted float64
	var liveN, shadowN, unpricedN int
	if err := db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0), COUNT(*) FROM main.call_costs
		WHERE backend = 'ccstream' AND billed_at >= ?`, from).Scan(&liveTotal, &liveN); err != nil {
		return fmt.Errorf("live totals: %w", err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0), COUNT(*),
			COALESCE(SUM(CASE WHEN finality = 'derived' THEN cost_usd END), 0),
			COALESCE(SUM(CASE WHEN finality = 'interrupted' THEN cost_usd END), 0),
			COALESCE(SUM(cost_usd IS NULL), 0)
		FROM shadow.call_costs WHERE backend = 'ccstream' AND billed_at >= ?`, from).
		Scan(&shadowTotal, &shadowN, &remainder, &interrupted, &unpricedN); err != nil {
		return fmt.Errorf("shadow totals: %w", err)
	}
	_, _ = fmt.Fprintf(w, "window: from %s\n", from)
	_, _ = fmt.Fprintf(w, "live ccstream rows:   %6d  $%10.4f\n", liveN, liveTotal)
	_, _ = fmt.Fprintf(w, "shadow ccstream calls: %6d  $%10.4f  (remainder rows $%.4f, interrupted $%.4f, unpriced calls %d)\n",
		shadowN, shadowTotal, remainder, interrupted, unpricedN)
	if liveTotal > 0 {
		_, _ = fmt.Fprintf(w, "shadow − live: $%+.4f (%+.2f%%)\n", shadowTotal-liveTotal, 100*(shadowTotal-liveTotal)/liveTotal)
	}

	// Per turn, over turns both ledgers hold.
	turns, err := shadowPairs(db, `SELECT l.turn_id, l.cost, s.cost FROM
			(SELECT turn_id, SUM(cost_usd) AS cost FROM main.call_costs
			 WHERE backend = 'ccstream' AND turn_id IS NOT NULL AND billed_at >= ? GROUP BY turn_id) l
		JOIN (SELECT turn_id, SUM(cost_usd) AS cost FROM shadow.call_costs
			 WHERE backend = 'ccstream' AND turn_id IS NOT NULL GROUP BY turn_id) s USING (turn_id)`, from)
	if err != nil {
		return fmt.Errorf("per turn: %w", err)
	}
	var onlyShadow, onlyLive int
	_ = db.QueryRow(`SELECT COUNT(DISTINCT turn_id) FROM shadow.api_calls WHERE backend = 'ccstream' AND turn_id IS NOT NULL
		AND billed_at >= ? AND turn_id NOT IN (SELECT turn_id FROM main.api_calls WHERE turn_id IS NOT NULL)`, from).Scan(&onlyShadow)
	_ = db.QueryRow(`SELECT COUNT(DISTINCT turn_id) FROM main.api_calls WHERE backend = 'ccstream' AND turn_id IS NOT NULL
		AND billed_at >= ? AND turn_id NOT IN (SELECT turn_id FROM shadow.api_calls WHERE turn_id IS NOT NULL)`, from).Scan(&onlyLive)
	_, _ = fmt.Fprintf(w, "\nturns in both: %d (shadow only %d, live only %d)\n", len(turns), onlyShadow, onlyLive)
	writePairSummary(w, turns, "turns", top)

	// Per CC process: CC's own cost against the adapter's.
	procs, unmatched, err := shadowProcesses(db, from)
	if err != nil {
		return fmt.Errorf("per process: %w", err)
	}
	_, _ = fmt.Fprintf(w, "\nCC processes: %d compared, %d unmatched — CC's own cost (live=CC) from the totals it restored at launch\n"+
		"(else 0) to its latest report that counts exactly the calls the adapter booked (equal tokens), against the\n"+
		"adapter's price of those calls (shadow); interrupted excluded; [i/n] = compared through report i of n\n",
		len(procs), len(unmatched))
	writePairSummary(w, procs, "processes", top)
	if len(unmatched) > 0 {
		_, _ = fmt.Fprintf(w, "  unmatched — no report counts the booked calls (a subagent still running, a remainder not yet\n"+
			"  settled, or a booking error); CC's latest report against the adapter's every call:\n")
		sort.Slice(unmatched, func(i, j int) bool { return unmatched[i].key < unmatched[j].key })
		for _, p := range unmatched {
			_, _ = fmt.Fprintf(w, "  %-60s live %9.4f shadow %9.4f diff %+9.4f\n", p.key, p.live, p.shadow, p.diff())
		}
	}

	// Per UTC day.
	days, err := shadowPairs(db, `SELECT d, SUM(lc), SUM(sc) FROM (
			SELECT substr(billed_at, 1, 10) AS d, cost_usd AS lc, 0 AS sc FROM main.call_costs WHERE backend = 'ccstream' AND billed_at >= ?
			UNION ALL
			SELECT substr(billed_at, 1, 10), 0, cost_usd FROM shadow.call_costs WHERE backend = 'ccstream' AND billed_at >= ?)
		GROUP BY d ORDER BY d`, from, from)
	if err != nil {
		return fmt.Errorf("per day: %w", err)
	}
	_, _ = fmt.Fprintf(w, "\nday (UTC)          live      shadow        diff\n")
	for _, d := range days {
		_, _ = fmt.Fprintf(w, "%s  %10.4f  %10.4f  %+10.4f\n", d.key, d.live, d.shadow, d.diff())
	}
	return nil
}

func shadowPairs(db *sql.DB, query string, args ...any) ([]shadowPair, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []shadowPair
	for rows.Next() {
		var p shadowPair
		var l, s sql.NullFloat64
		if err := rows.Scan(&p.key, &l, &s); err != nil {
			return nil, err
		}
		p.live, p.shadow = l.Float64, s.Float64
		out = append(out, p)
	}
	return out, rows.Err()
}

// writePairSummary prints the totals, how many pairs agree within 3% (the
// divergence tolerance), and the most divergent.
func writePairSummary(w io.Writer, pairs []shadowPair, what string, top int) {
	var l, s float64
	within := 0
	for _, p := range pairs {
		l += p.live
		s += p.shadow
		if math.Abs(p.diff()) <= 0.03*math.Max(math.Abs(p.live), 0.01) {
			within++
		}
	}
	_, _ = fmt.Fprintf(w, "  live $%.4f  shadow $%.4f  diff $%+.4f; %d of %d %s within 3%%\n", l, s, s-l, within, len(pairs), what)
	sort.Slice(pairs, func(i, j int) bool { return math.Abs(pairs[i].diff()) > math.Abs(pairs[j].diff()) })
	for i, p := range pairs {
		if i >= top || math.Abs(p.diff()) < 1e-9 {
			break
		}
		_, _ = fmt.Fprintf(w, "  %-60s live %9.4f shadow %9.4f diff %+9.4f\n", strings.TrimSpace(p.key), p.live, p.shadow, p.diff())
	}
}

// shadowReport is one CC report of a process: its cumulative figure per model.
type shadowReport map[string]shadowUsage

type shadowUsage struct {
	cost   float64
	tokens modelinfo.Tokens
}

// shadowCall is one booked, non-interrupted call of a process.
type shadowCall struct {
	model  string
	window int64 // the result window it was booked in; a remainder's through_window
	cost   float64
	tokens modelinfo.Tokens // in the reports' classes
}

// shadowProcesses compares, per CC process with a report in the window, CC's
// own cost with the adapter's price over ONE interval (#2122):
//
//   - it starts at the totals CC restored at launch — the process's baseline
//     report, stamped at the launch its scope names — else at zero: a fresh
//     process's first report already counts its first turn;
//   - it ends at the latest report whose token counts, less the baseline,
//     equal the booked calls of windows up to some window — every class of
//     every model, exactly. That is the set CC counted: a report is taken at
//     a result, which closes a window, and a settled quiet result's remainder
//     makes the equality exact by construction. A report with a subagent
//     still running, or an unsettled remainder, matches no window prefix and
//     is passed over — no billed_at cut, which lines landing ~2ms after the
//     result would make racy (#2112 b).
//
// A process no report matches is returned in unmatched, as its latest report
// against every call the adapter booked for it.
func shadowProcesses(db *sql.DB, from string) (compared, unmatched []shadowPair, err error) {
	reports, order, err := shadowProcessReports(db, from)
	if err != nil {
		return nil, nil, err
	}
	calls, err := shadowProcessCalls(db, reports)
	if err != nil {
		return nil, nil, err
	}
	for _, scope := range order {
		rs := reports[scope]
		var base shadowReport
		if launch, ok := ccstream.ScopeLaunch(scope); ok && rs[0].at.Equal(launch) {
			base, rs = rs[0].byModel, rs[1:]
		}
		if len(rs) == 0 {
			continue // only the restored totals: no interval yet
		}
		prefixes := callPrefixes(calls[scope])
		matched := false
		for i := len(rs) - 1; i >= 0 && !matched; i-- {
			want := reportDelta(rs[i].byModel, base)
			for j := len(prefixes) - 1; j >= 0; j-- {
				if sameTokens(prefixes[j].tokens, want.tokens) {
					key := scope
					if i < len(rs)-1 {
						key = fmt.Sprintf("%s [%d/%d]", scope, i+1, len(rs))
					}
					compared = append(compared, shadowPair{key: key, live: want.cost, shadow: prefixes[j].cost})
					matched = true
					break
				}
			}
		}
		if !matched {
			all := prefixes[len(prefixes)-1]
			for _, c := range calls[scope] {
				if c.window == math.MaxInt64 {
					all.cost += c.cost // booked outside any window: in no prefix
				}
			}
			unmatched = append(unmatched, shadowPair{key: scope, live: reportDelta(rs[len(rs)-1].byModel, base).cost, shadow: all.cost})
		}
	}
	return compared, unmatched, nil
}

type shadowReportAt struct {
	at      time.Time
	byModel shadowReport
}

// shadowProcessReports reads every report of each ccstream process with one
// in the window, oldest first, one entry per report time.
func shadowProcessReports(db *sql.DB, from string) (map[string][]shadowReportAt, []string, error) {
	rows, err := db.Query(`SELECT scope_key, at, model, COALESCE(cost_usd, 0), tokens FROM shadow.backend_reports
		WHERE backend = 'ccstream' AND scope_key IN (
			SELECT scope_key FROM shadow.backend_reports WHERE backend = 'ccstream' AND at >= ?)
		ORDER BY scope_key, at, id`, from)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]shadowReportAt{}
	var order []string
	for rows.Next() {
		var scope, atText, model, tokensJSON string
		var cost float64
		if err := rows.Scan(&scope, &atText, &model, &cost, &tokensJSON); err != nil {
			return nil, nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, atText)
		if err != nil {
			return nil, nil, fmt.Errorf("report %s at %q: %w", scope, atText, err)
		}
		var tokens modelinfo.Tokens
		if err := json.Unmarshal([]byte(tokensJSON), &tokens); err != nil {
			return nil, nil, fmt.Errorf("report %s tokens: %w", scope, err)
		}
		rs := out[scope]
		if rs == nil {
			order = append(order, scope)
		}
		if len(rs) == 0 || !rs[len(rs)-1].at.Equal(at) {
			rs = append(rs, shadowReportAt{at: at, byModel: shadowReport{}})
		}
		rs[len(rs)-1].byModel[model] = shadowUsage{cost: cost, tokens: tokens}
		out[scope] = rs
	}
	return out, order, rows.Err()
}

// shadowProcessCalls reads the booked, non-interrupted calls of the given
// processes, with their counts folded onto the reports' classes.
func shadowProcessCalls(db *sql.DB, scopes map[string][]shadowReportAt) (map[string][]shadowCall, error) {
	rows, err := db.Query(`SELECT a.id, json_extract(a.detail, '$.scope'), a.model,
			COALESCE(json_extract(a.detail, '$.window'), json_extract(a.detail, '$.through_window')),
			c.cost_usd, t.class, t.count
		FROM shadow.api_calls a JOIN shadow.call_costs c ON c.id = a.id
		LEFT JOIN shadow.call_tokens t ON t.call_id = a.id
		WHERE a.backend = 'ccstream' AND a.finality <> 'interrupted' AND json_extract(a.detail, '$.scope') IS NOT NULL
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byID := map[int64]*shadowCall{}
	scopeOf := map[int64]string{}
	var ids []int64
	for rows.Next() {
		var id int64
		var scope, model string
		var window sql.NullInt64
		var cost sql.NullFloat64
		var class sql.NullString
		var count sql.NullInt64
		if err := rows.Scan(&id, &scope, &model, &window, &cost, &class, &count); err != nil {
			return nil, err
		}
		if _, ok := scopes[scope]; !ok {
			continue
		}
		c := byID[id]
		if c == nil {
			c = &shadowCall{model: model, window: math.MaxInt64, cost: cost.Float64, tokens: modelinfo.Tokens{}}
			if window.Valid {
				c.window = window.Int64
			}
			byID[id], scopeOf[id] = c, scope
			ids = append(ids, id)
		}
		if class.Valid {
			c.tokens[modelinfo.Class(class.String)] += int(count.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string][]shadowCall{}
	for _, id := range ids {
		c := byID[id]
		c.tokens = ccstream.ReportClasses(c.tokens)
		out[scopeOf[id]] = append(out[scopeOf[id]], *c)
	}
	return out, nil
}

// shadowTotal is a set of calls' counts per model and their price.
type shadowTotal struct {
	tokens map[string]modelinfo.Tokens
	cost   float64
}

// callPrefixes is the running total of calls by window: entry 0 is no calls,
// entry i every call of the i lowest windows. A call booked outside any window
// is in none.
func callPrefixes(calls []shadowCall) []shadowTotal {
	byWindow := map[int64][]shadowCall{}
	for _, c := range calls {
		if c.window != math.MaxInt64 {
			byWindow[c.window] = append(byWindow[c.window], c)
		}
	}
	out := []shadowTotal{{tokens: map[string]modelinfo.Tokens{}}}
	for _, w := range slices.Sorted(maps.Keys(byWindow)) {
		prev := out[len(out)-1]
		next := shadowTotal{tokens: map[string]modelinfo.Tokens{}, cost: prev.cost}
		for m, t := range prev.tokens {
			next.tokens[m] = maps.Clone(t)
		}
		for _, c := range byWindow[w] {
			if next.tokens[c.model] == nil {
				next.tokens[c.model] = modelinfo.Tokens{}
			}
			for class, n := range c.tokens {
				next.tokens[c.model][class] += n
			}
			next.cost += c.cost
		}
		out = append(out, next)
	}
	return out
}

// reportDelta is what a report counts beyond the baseline. A model the report
// omits has not moved.
func reportDelta(r, base shadowReport) shadowTotal {
	out := shadowTotal{tokens: map[string]modelinfo.Tokens{}}
	for m, u := range r {
		d := modelinfo.Tokens{}
		for class, n := range u.tokens {
			d[class] += n
		}
		for class, n := range base[m].tokens {
			d[class] -= n
		}
		out.tokens[m] = d
		out.cost += u.cost - base[m].cost
	}
	return out
}

// sameTokens reports whether a and b hold the same non-zero counts.
func sameTokens(a, b map[string]modelinfo.Tokens) bool {
	return tokensWithin(a, b) && tokensWithin(b, a)
}

func tokensWithin(a, b map[string]modelinfo.Tokens) bool {
	for m, t := range a {
		for class, n := range t {
			if n != 0 && b[m][class] != n {
				return false
			}
		}
	}
	return true
}

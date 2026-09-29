package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"foci/internal/sqlite"
)

// runLedgerShadow is `foci-gw ledger-shadow`: it compares a shadow ledger (the
// Claude Code adapter booking beside the live path, logging.api_shadow_db,
// #2111 §12) with the live one over the same window, read-only, so the switch
// is decided on evidence. Three comparisons:
//
//   - per turn: the live turn-level rows against the adapter's per-call rows,
//     on every turn both hold;
//   - per CC process: CC's OWN cumulative cost (last report − the baseline it
//     started from) against the adapter's price of everything it booked in
//     that process — the design's divergence check, interrupted calls excluded
//     because CC counts them nowhere;
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
	procs, err := shadowPairs(db, `SELECT r.scope_key, r.cc, COALESCE(c.priced, 0) FROM
			(SELECT scope_key, SUM(last - first) AS cc FROM (
				SELECT scope_key, model,
					(SELECT cost_usd FROM shadow.backend_reports b WHERE b.scope_key = a.scope_key AND b.model = a.model ORDER BY at DESC, id DESC LIMIT 1) AS last,
					(SELECT cost_usd FROM shadow.backend_reports b WHERE b.scope_key = a.scope_key AND b.model = a.model ORDER BY at, id LIMIT 1) AS first
				FROM shadow.backend_reports a WHERE backend = 'ccstream' AND at >= ? GROUP BY scope_key, model)
			 GROUP BY scope_key) r
		LEFT JOIN (SELECT json_extract(x.detail, '$.scope') AS scope_key, SUM(cc.cost_usd) AS priced
			FROM shadow.api_calls x JOIN shadow.call_costs cc ON cc.id = x.id
			WHERE x.finality <> 'interrupted' GROUP BY 1) c USING (scope_key)`, from)
	if err != nil {
		return fmt.Errorf("per process: %w", err)
	}
	_, _ = fmt.Fprintf(w, "\nCC processes: %d — CC's own cost (live=CC) against the adapter's price (shadow), interrupted excluded\n", len(procs))
	writePairSummary(w, procs, "processes", top)

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

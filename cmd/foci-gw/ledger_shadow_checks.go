package main

import (
	"database/sql"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	"foci/internal/modelinfo"
)

// The dollar checks of `foci-gw ledger-shadow` (#2131). The token checks
// cannot see a call priced on the wrong TTL — CC and the adapter count the
// same tokens and disagree only on their price — nor a remainder that holds
// a whole call source nobody tailed (#2130). Every threshold here comes from
// the data under test: the known residual from the processes no TTL guess
// touches, and the overhead bound from each process's own calls.

// shadowFloatNoise is the rounding of a sum of per-class prices: a
// difference below it is no difference. It is not a tolerance.
const shadowFloatNoise = 1e-9

// shadowTTLWrite is cache writes whose TTL was not reported (class
// cache_write, class_method=unknown): their count, their price as booked
// (the rate cache_write carries, 1h) and their price at the 5m rate. The
// difference is what rests on the TTL guess.
type shadowTTLWrite struct {
	tokens, calls int
	booked, at5m  float64
}

func (t *shadowTTLWrite) add(o shadowTTLWrite) {
	t.tokens += o.tokens
	t.calls += o.calls
	t.booked += o.booked
	t.at5m += o.at5m
}

// exposure is how much of the price the TTL guess decides.
func (t shadowTTLWrite) exposure() float64 { return t.booked - t.at5m }

// shadowTTLCall is one call's unknown-TTL writes, with what the report
// groups them by.
type shadowTTLCall struct {
	model, billedAt string
	shadowTTLWrite
}

// shadowTTLWrites reads every ccstream call's unknown-TTL cache writes,
// priced as the call_class_costs view prices them and again at the 5m rate
// the same way. A write with either price missing is left out: it is
// unpriced, which the class-method section reports.
func shadowTTLWrites(db *sql.DB) (map[int64]shadowTTLCall, error) {
	rows, err := db.Query(`SELECT c.id, c.model, c.billed_at, x.count, x.cost_usd,
			CAST(x.count AS REAL) / k.per_units * (
				SELECT r.usd_per_unit FROM shadow.token_rates r
				WHERE r.rate_model = c.rate_model AND r.class = ?
				  AND r.effective_from <= substr(c.billed_at, 1, 10)
				ORDER BY r.effective_from DESC LIMIT 1)
		FROM shadow.call_class_costs x
		JOIN shadow.api_calls c ON c.id = x.call_id
		JOIN shadow.token_classes k ON k.class = x.class
		WHERE c.backend = 'ccstream' AND x.class = ?`,
		string(modelinfo.ClassCacheWrite5m), string(modelinfo.ClassCacheWrite))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]shadowTTLCall{}
	for rows.Next() {
		var id int64
		var c shadowTTLCall
		var booked, at5m sql.NullFloat64
		if err := rows.Scan(&id, &c.model, &c.billedAt, &c.tokens, &booked, &at5m); err != nil {
			return nil, err
		}
		if !booked.Valid || !at5m.Valid {
			continue
		}
		c.calls, c.booked, c.at5m = 1, booked.Float64, at5m.Float64
		out[id] = c
	}
	return out, rows.Err()
}

// shadowResidual is the known residual between CC's price and modelinfo's on
// the same tokens: the median of |CC − adapter| / CC over the compared
// processes that carry no unknown-TTL write, the only ones whose difference
// no TTL guess can cause. The median, so that one broken process cannot
// widen it. n is how many processes it was taken over.
func shadowResidual(procs []shadowProcess) (residual float64, n int) {
	var rel []float64
	for _, p := range procs {
		if p.ttl.tokens == 0 && math.Abs(p.live) > shadowFloatNoise {
			rel = append(rel, math.Abs(p.diff())/math.Abs(p.live))
		}
	}
	if len(rel) == 0 {
		return 0, 0
	}
	sort.Float64s(rel)
	mid := len(rel) / 2
	if len(rel)%2 == 1 {
		return rel[mid], len(rel)
	}
	return (rel[mid-1] + rel[mid]) / 2, len(rel)
}

// flag says what is wrong with a compared process, or "" if nothing: CC's
// cost must lie in the range the adapter's price spans over every TTL its
// unknown-TTL writes could have had (all 5m .. all 1h, as booked), widened by
// the known residual. Outside it, the gap is unexplained; inside it but away
// from the booked price, the gap rests on the TTL guess, and CC's cost says
// what share of those writes were 5m.
func (p shadowProcess) flag(residual float64) string {
	bound := residual*math.Abs(p.live) + shadowFloatNoise
	lo, hi := p.shadow-p.ttl.exposure(), p.shadow
	if lo > hi {
		lo, hi = hi, lo
	}
	head := fmt.Sprintf("%s: diff %+.4f (CC %.4f, adapter %.4f)", p.key, p.diff(), p.live, p.shadow)
	var unexplained float64
	switch {
	case p.live < lo:
		unexplained = lo - p.live
	case p.live > hi:
		unexplained = hi - p.live
	}
	switch {
	case math.Abs(unexplained) > bound:
		if p.ttl.tokens == 0 {
			return head + " — unexplained: no unknown-TTL write to account for it"
		}
		return fmt.Sprintf("%s — %+.4f unexplained even with its %d unknown-TTL cache-write tokens at any TTL (range %.4f..%.4f)",
			head, unexplained, p.ttl.tokens, lo, hi)
	case math.Abs(p.diff()) > bound:
		return fmt.Sprintf("%s — rests on %d unknown-TTL cache-write tokens priced at the 1h rate: CC's cost implies %.0f%% at 5m",
			head, p.ttl.tokens, 100*p.diff()/p.ttl.exposure())
	}
	return ""
}

// writeProcessDollars prints the known residual and each compared process
// whose dollars disagree beyond it, and returns those as flags.
func writeProcessDollars(w io.Writer, procs []shadowProcess) []string {
	residual, n := shadowResidual(procs)
	_, _ = fmt.Fprintf(w, "  dollars: known residual %.4f%% (median |CC − adapter| / CC over the %d processes with no unknown-TTL write);\n"+
		"  a process is flagged when CC's cost lies beyond it from the adapter's price, or from the range its\n"+
		"  unknown-TTL writes span (all 5m .. all 1h as booked)\n", 100*residual, n)
	var flags []string
	for _, p := range procs {
		if f := p.flag(residual); f != "" {
			flags = append(flags, f)
			_, _ = fmt.Fprintf(w, "  FLAG %s\n", f)
		}
	}
	return flags
}

// writeShadowRemainder prints the remainder's share per UTC day and per
// process, with the models and classes it carries, and flags every
// turn-less remainder row that costs more than the largest call its process
// booked. Overhead by design is CC's own utility calls (a title, a summary),
// each smaller than the agent's real calls; a remainder outweighing every
// one of them holds a call source the adapter never read — a transcript not
// tailed (#2130) — which the design's invOverheadBounded is there to catch.
func writeShadowRemainder(db *sql.DB, from string, w io.Writer) ([]string, error) {
	_, _ = fmt.Fprintf(w, "\nremainder (finality=derived) — overhead has no turn, a compaction is on its own turn; share of spend\n")

	type dayShare struct {
		day            string
		derived, total float64
		byModelClass   map[string]map[modelinfo.Class]float64
	}
	var days []*dayShare
	byDay := map[string]*dayShare{}
	rows, err := db.Query(`SELECT substr(billed_at, 1, 10), TOTAL(CASE WHEN finality = 'derived' THEN cost_usd END), TOTAL(cost_usd)
		FROM shadow.call_costs WHERE backend = 'ccstream' AND billed_at >= ? GROUP BY 1 ORDER BY 1`, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		d := &dayShare{byModelClass: map[string]map[modelinfo.Class]float64{}}
		if err := rows.Scan(&d.day, &d.derived, &d.total); err != nil {
			_ = rows.Close()
			return nil, err
		}
		days = append(days, d)
		byDay[d.day] = d
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	derived, err := shadowDerivedRows(db, from)
	if err != nil {
		return nil, err
	}
	for _, r := range derived {
		d := byDay[r.day]
		if d == nil {
			continue
		}
		if d.byModelClass[r.model] == nil {
			d.byModelClass[r.model] = map[modelinfo.Class]float64{}
		}
		for class, usd := range r.classCost {
			d.byModelClass[r.model][class] += usd
		}
	}
	for _, d := range days {
		_, _ = fmt.Fprintf(w, "  %s  $%.4f of $%.4f (%s)\n", d.day, d.derived, d.total, sharePct(d.derived, d.total))
		for _, m := range slices.Sorted(maps.Keys(d.byModelClass)) {
			_, _ = fmt.Fprintf(w, "    %-20s %s\n", m, formatClassCosts(d.byModelClass[m]))
		}
	}

	// Per process: each derived row with its classes, against the process's
	// spend and its largest booked call.
	scopes := map[string][]shadowDerivedRow{}
	for _, r := range derived {
		scopes[r.scope] = append(scopes[r.scope], r)
	}
	var flags []string
	for _, scope := range slices.Sorted(maps.Keys(scopes)) {
		var total float64
		var largest sql.NullFloat64
		if err := db.QueryRow(`SELECT TOTAL(c.cost_usd), MAX(CASE WHEN a.finality <> 'derived' THEN c.cost_usd END)
			FROM shadow.api_calls a JOIN shadow.call_costs c ON c.id = a.id
			WHERE a.backend = 'ccstream' AND json_extract(a.detail, '$.scope') = ?`, scope).Scan(&total, &largest); err != nil {
			return nil, err
		}
		var rem float64
		for _, r := range scopes[scope] {
			rem += r.cost.Float64
		}
		_, _ = fmt.Fprintf(w, "  %s  $%.4f of $%.4f (%s); largest booked call $%.4f\n",
			scope, rem, total, sharePct(rem, total), largest.Float64)
		for _, r := range scopes[scope] {
			cost := "unpriced"
			if r.cost.Valid {
				cost = fmt.Sprintf("$%.4f", r.cost.Float64)
			}
			_, _ = fmt.Fprintf(w, "    w%-3d %-10s %-20s %9s  %s\n", r.window, r.kind, r.model, cost, formatClassCounts(r.counts))
			if r.turnless && r.cost.Float64 > largest.Float64+shadowFloatNoise {
				flags = append(flags, fmt.Sprintf("%s: turn-less remainder through window %d, %s $%.4f (%s), costs more than the "+
					"largest call the process booked ($%.4f) — not overhead by design: a call source the adapter never read?",
					scope, r.window, r.model, r.cost.Float64, formatClassCounts(r.counts), largest.Float64))
			}
		}
	}
	return flags, nil
}

// shadowDerivedRow is one remainder row in the window.
type shadowDerivedRow struct {
	scope, kind, model, day string
	window                  int64
	turnless                bool
	cost                    sql.NullFloat64
	counts                  modelinfo.Tokens
	classCost               map[modelinfo.Class]float64
}

func shadowDerivedRows(db *sql.DB, from string) ([]shadowDerivedRow, error) {
	rows, err := db.Query(`SELECT a.id, COALESCE(json_extract(a.detail, '$.scope'), a.session),
			COALESCE(json_extract(a.detail, '$.through_window'), -1), a.kind, a.turn_id IS NULL, a.model,
			substr(a.billed_at, 1, 10), c.cost_usd, x.class, x.count, x.cost_usd
		FROM shadow.api_calls a JOIN shadow.call_costs c ON c.id = a.id
		LEFT JOIN shadow.call_class_costs x ON x.call_id = a.id
		WHERE a.backend = 'ccstream' AND a.finality = 'derived' AND a.billed_at >= ?
		ORDER BY a.id`, from)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []shadowDerivedRow
	last := int64(-1)
	for rows.Next() {
		var id int64
		var r shadowDerivedRow
		var class sql.NullString
		var count sql.NullInt64
		var classCost sql.NullFloat64
		if err := rows.Scan(&id, &r.scope, &r.window, &r.kind, &r.turnless, &r.model, &r.day, &r.cost,
			&class, &count, &classCost); err != nil {
			return nil, err
		}
		if id != last {
			r.counts, r.classCost = modelinfo.Tokens{}, map[modelinfo.Class]float64{}
			out = append(out, r)
			last = id
		}
		if class.Valid {
			cur := &out[len(out)-1]
			cur.counts[modelinfo.Class(class.String)] += int(count.Int64)
			cur.classCost[modelinfo.Class(class.String)] += classCost.Float64
		}
	}
	return out, rows.Err()
}

// writeShadowClassMethods prints the window's calls per class_method, the
// unknown-TTL cache writes priced as booked and at 5m per model, and the
// unpriced calls with the model and class lacking a rate — each unpriced
// call a flag, since the views read it as NULL and every sum above skips it.
func writeShadowClassMethods(db *sql.DB, from string, ttl map[int64]shadowTTLCall, w io.Writer) ([]string, error) {
	_, _ = fmt.Fprintf(w, "\nclass methods (how each call's cache-write TTL was decided)\n")
	rows, err := db.Query(`SELECT class_method, COUNT(*), TOTAL(cost_usd) FROM shadow.call_costs
		WHERE backend = 'ccstream' AND billed_at >= ? GROUP BY 1 ORDER BY 1`, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var method string
		var n int
		var cost float64
		if err := rows.Scan(&method, &n, &cost); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_, _ = fmt.Fprintf(w, "  %-12s %6d calls  $%.4f\n", method, n, cost)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var all shadowTTLWrite
	byModel := map[string]*shadowTTLWrite{}
	for _, c := range ttl {
		if c.billedAt < from {
			continue
		}
		all.add(c.shadowTTLWrite)
		if byModel[c.model] == nil {
			byModel[c.model] = &shadowTTLWrite{}
		}
		byModel[c.model].add(c.shadowTTLWrite)
	}
	_, _ = fmt.Fprintf(w, "  unknown TTL: %d cache-write tokens on %d calls: $%.4f as booked (1h rate), $%.4f at 5m; $%.4f rests on the TTL guess\n",
		all.tokens, all.calls, all.booked, all.at5m, all.exposure())
	for _, m := range slices.Sorted(maps.Keys(byModel)) {
		t := byModel[m]
		_, _ = fmt.Fprintf(w, "    %-20s %10d tokens on %d calls: $%.4f as booked, $%.4f at 5m\n", m, t.tokens, t.calls, t.booked, t.at5m)
	}

	rows, err = db.Query(`SELECT c.model, x.class, COUNT(DISTINCT c.id)
		FROM shadow.call_class_costs x JOIN shadow.api_calls c ON c.id = x.call_id
		WHERE c.backend = 'ccstream' AND c.billed_at >= ? AND x.cost_usd IS NULL
		GROUP BY 1, 2 ORDER BY 1, 2`, from)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var flags []string
	for rows.Next() {
		var model, class string
		var n int
		if err := rows.Scan(&model, &class, &n); err != nil {
			return nil, err
		}
		f := fmt.Sprintf("%d calls unpriced: %s has no %s rate (the views read their cost as NULL; no sum counts them)", n, model, class)
		_, _ = fmt.Fprintf(w, "  FLAG %s\n", f)
		flags = append(flags, f)
	}
	return flags, rows.Err()
}

func sharePct(part, whole float64) string {
	if whole == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", 100*part/whole)
}

func formatClassCounts(t modelinfo.Tokens) string {
	var parts []string
	for _, c := range slices.Sorted(maps.Keys(t)) {
		parts = append(parts, fmt.Sprintf("%s=%d", c, t[c]))
	}
	return strings.Join(parts, " ")
}

func formatClassCosts(t map[modelinfo.Class]float64) string {
	var parts []string
	for _, c := range slices.Sorted(maps.Keys(t)) {
		parts = append(parts, fmt.Sprintf("%s $%.4f", c, t[c]))
	}
	return strings.Join(parts, ", ")
}

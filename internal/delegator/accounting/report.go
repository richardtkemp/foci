package accounting

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// MigrationReport is what the pre-ledger migration did (#2111 §6 step 6). The
// old-versus-new totals are the point: the views re-price every legacy row
// with its inferred TTL split at dated rates, so historical totals move, and
// this says by how much, overall and per day.
type MigrationReport struct {
	BackupPath string
	Duration   time.Duration

	Rows     int
	Turns    int
	Reports  int
	Backends map[string]int

	// Methods counts legacy calls per class_method; MethodsWithWrites counts
	// only those with any cache write, where the method decided something.
	Methods           map[string]int
	MethodsWithWrites map[string]int
	// Bases counts legacy calls per cost_basis: re-priced from their counts,
	// or carrying their recorded figure.
	Bases map[string]int
	Solve solveStats

	// Alarms counts the invariant violations the conversion found (a model
	// with no rate group, a class with no rate). Counted, not delivered.
	Alarms map[string]int

	// OldCalculated is SUM(calculated_cost_usd) over the v1 rows that had one;
	// OldEffective is what the pre-ledger readers showed (that figure, else a
	// live estimate); New is SUM(call_costs.cost_usd) over the priced calls.
	OldCalculated, OldEffective, New float64
	UnpricedCalls                    int
	// UnpricedOldEffective is OldEffective's share on the calls New leaves
	// unpriced, so the two totals can be compared like for like.
	UnpricedOldEffective float64

	// Groups splits the totals by backend and by what the counts were: a
	// row's turn totals, or (rows written before #1854, and backends that
	// never measured them) the un-suffixed columns, which for a delegated
	// turn are its final cycle's context fill rather than what was billed.
	Groups []GroupTotals

	Days []DayTotals
}

// GroupTotals is one backend's old and new totals over rows whose counts were,
// or were not, turn totals.
type GroupTotals struct {
	Backend       string
	TurnTotals    bool
	Rows          int
	Old, New      float64
	UnpricedCalls int
}

// DayTotals is one UTC day's old and new totals.
type DayTotals struct {
	Day                              string
	OldCalculated, OldEffective, New float64
	UnpricedCalls                    int
}

// tally fills the report's counts and totals from the converted rows and the
// committed ledger.
func (rep *MigrationReport) tally(db *sql.DB, v1 []*v1Row) error {
	rep.Rows = len(v1)
	rep.Backends, rep.Methods, rep.MethodsWithWrites = map[string]int{}, map[string]int{}, map[string]int{}
	rep.Bases = map[string]int{}
	days := map[string]*DayTotals{}
	day := func(d string) *DayTotals {
		if days[d] == nil {
			days[d] = &DayTotals{Day: d}
		}
		return days[d]
	}
	oldByID := make(map[int64]float64, len(v1))
	type groupKey struct {
		backend string
		turn    bool
	}
	groups := map[groupKey]*GroupTotals{}
	groupOf := map[int64]*GroupTotals{}
	for _, r := range v1 {
		k := groupKey{r.backend, r.turnTotals}
		if groups[k] == nil {
			groups[k] = &GroupTotals{Backend: r.backend, TurnTotals: r.turnTotals}
		}
		g := groups[k]
		g.Rows++
		g.Old += r.oldCost()
		groupOf[r.id] = g
		rep.Backends[r.backend]++
		rep.Methods[r.classMethod]++
		rep.Bases[r.costBasis]++
		if r.priced.CacheWrite > 0 {
			rep.MethodsWithWrites[r.classMethod]++
		}
		d := day(r.ts.UTC().Format("2006-01-02"))
		if r.calcUSD.Valid {
			rep.OldCalculated += r.calcUSD.Float64
			d.OldCalculated += r.calcUSD.Float64
		}
		old := r.oldCost()
		oldByID[r.id] = old
		rep.OldEffective += old
		d.OldEffective += old
	}

	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM turns), (SELECT COUNT(*) FROM backend_reports)`).
		Scan(&rep.Turns, &rep.Reports); err != nil {
		return fmt.Errorf("ledger migration report: %w", err)
	}
	rows, err := db.Query(`SELECT id, substr(billed_at, 1, 10), cost_usd FROM call_costs WHERE kind = 'legacy'`)
	if err != nil {
		return fmt.Errorf("ledger migration report: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var d string
		var cost sql.NullFloat64
		if err := rows.Scan(&id, &d, &cost); err != nil {
			return fmt.Errorf("ledger migration report: %w", err)
		}
		if !cost.Valid {
			rep.UnpricedCalls++
			rep.UnpricedOldEffective += oldByID[id]
			day(d).UnpricedCalls++
			groupOf[id].UnpricedCalls++
			continue
		}
		rep.New += cost.Float64
		day(d).New += cost.Float64
		groupOf[id].New += cost.Float64
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger migration report: %w", err)
	}
	for _, d := range slices.Sorted(maps.Keys(days)) {
		rep.Days = append(rep.Days, *days[d])
	}
	for _, g := range groups {
		rep.Groups = append(rep.Groups, *g)
	}
	slices.SortFunc(rep.Groups, func(a, b GroupTotals) int {
		if c := strings.Compare(a.Backend, b.Backend); c != 0 {
			return c
		}
		if a.TurnTotals == b.TurnTotals {
			return 0
		}
		if a.TurnTotals {
			return 1
		}
		return -1
	})
	return nil
}

// String renders the report for an operator.
func (rep *MigrationReport) String() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	p("ledger migration: %d v1 rows -> %d legacy calls, %d turns, %d backend reports in %s",
		rep.Rows, rep.Rows, rep.Turns, rep.Reports, rep.Duration.Round(time.Millisecond))
	if rep.BackupPath != "" {
		p("backup: %s", rep.BackupPath)
	}
	p("backends (inferred): %s", counts(rep.Backends))
	p("cost_basis: %s", counts(rep.Bases))
	p("class_method, all calls: %s", counts(rep.Methods))
	p("class_method, calls with cache writes: %s", counts(rep.MethodsWithWrites))
	s := rep.Solve
	rate := 0.0
	if s.Windows > 0 {
		rate = 100 * float64(s.Accepted) / float64(s.Windows)
	}
	p("TTL solve from CC's reported cost: %d windows, %d accepted (%.1f%%), %d rejected (not an in-range integer), %d disagreed with the kind rule, %d unpriceable",
		s.Windows, s.Accepted, rate, s.Rejected, s.Disagreed, s.Unpriceable)
	if len(rep.Alarms) > 0 {
		p("invariants found in history (counted, not alarmed): %s", counts(rep.Alarms))
	}
	p("totals: old calculated $%.2f | old effective $%.2f | new $%.2f priced + %d unpriced calls (old effective $%.2f)",
		rep.OldCalculated, rep.OldEffective, rep.New, rep.UnpricedCalls, rep.UnpricedOldEffective)
	p("new - old effective (priced calls only): $%.2f", rep.New-(rep.OldEffective-rep.UnpricedOldEffective))
	p("%-9s %-12s %7s %12s %12s %9s", "backend", "counts", "rows", "old eff", "new", "unpriced")
	for _, g := range rep.Groups {
		what := "un-suffixed"
		if g.TurnTotals {
			what = "turn totals"
		}
		p("%-9s %-12s %7d %12.2f %12.2f %9d", g.Backend, what, g.Rows, g.Old, g.New, g.UnpricedCalls)
	}
	p("%-10s %12s %12s %12s %9s", "day (UTC)", "old calc", "old eff", "new", "unpriced")
	for _, d := range rep.Days {
		p("%-10s %12.4f %12.4f %12.4f %9d", d.Day, d.OldCalculated, d.OldEffective, d.New, d.UnpricedCalls)
	}
	return b.String()
}

func counts(m map[string]int) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

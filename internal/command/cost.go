package command

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/timeutil"
)

// costUsage returns the help text for /cost subcommands.
func costUsage() string {
	return "/cost session — this session's cost so far\n" +
		"/cost today — today's costs by session\n" +
		"/cost 24h — last 24 hours with category breakdown\n" +
		"/cost week — 7-day summary with daily breakdown\n" +
		"/cost <days> — total for last N days\n" +
		"add `breakdown` to any of the above (e.g. /cost week breakdown) — period total split by session type"
}

// breakdownRequested reports whether the trailing args ask for the by-type
// breakdown view (a modifier available on every /cost subcommand).
func breakdownRequested(args string) bool {
	for _, f := range strings.Fields(args) {
		if strings.EqualFold(f, "breakdown") {
			return true
		}
	}
	return false
}

// costSession shows the total cost for the current session only.
func costSession(entries []log.APIEntry, sessionKey string, idx *session.SessionIndex, breakdown bool) string {
	if breakdown && idx != nil {
		return costSessionBreakdown(entries, sessionKey, idx)
	}
	filtered := filterEntries(entries, func(e log.APIEntry) bool {
		return e.Session == sessionKey
	})
	total, count := sumCosts(filtered)

	var b strings.Builder
	if count == 0 {
		b.WriteString("💰 This session: no API calls logged yet.")
	} else {
		fmt.Fprintf(&b, "💰 This session: $%.4f (%s calls)", total, display.FormatCommas(count))
	}
	if line := sessionStartLine(idx, sessionKey); line != "" {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	if count == 0 {
		return b.String()
	}
	cr, cw, inp, out := categoryCosts(filtered)
	cols := []display.Column{
		{Header: "Category"},
		{Header: "Cost", Align: display.AlignRight},
	}
	labels := []string{"Cache reads", "Cache writes", "Input", "Output", "Total"}
	costCells := moneyCol([]float64{cr, cw, inp, out, total}, 4)
	tableRows := make([][]string, len(labels))
	for i, l := range labels {
		tableRows[i] = []string{l, costCells[i]}
	}
	b.WriteString("\n\n")
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// costSessionBreakdown sums the cost across the whole session family — the
// current session's root ancestor plus every branch/child descendant (spawns,
// reflections, keepalives, facets…) — grouped by session type. Session keys
// never fork on compaction, so "previous versions" collapse into the same key
// set; the family is resolved via the parent_session_key tree, not a string
// prefix, so cross-chat independent spawns are captured correctly.
func costSessionBreakdown(entries []log.APIEntry, sessionKey string, idx *session.SessionIndex) string {
	family, start := sessionFamily(idx, sessionKey)
	filtered := filterEntries(entries, func(e log.APIEntry) bool {
		_, ok := family[e.Session]
		return ok
	})
	header := "This session (family)"
	if line := startLine(start); line != "" {
		header += "\n" + line
	}
	return renderTypeBreakdown(filtered, buildSessionTypeMap(idx), header)
}

// costToday shows today's total with per-session breakdown.
func costToday(entries []log.APIEntry, idx *session.SessionIndex, breakdown bool) string {
	today := timeutil.Now().Format("2006-01-02")
	pred := func(e log.APIEntry) bool {
		return e.Timestamp.Local().Format("2006-01-02") == today
	}
	if breakdown && idx != nil {
		return renderTypeBreakdown(filterEntries(entries, pred), buildSessionTypeMap(idx), "Today")
	}
	filtered := filterEntries(entries, pred)
	total, count := sumCosts(filtered)

	var b strings.Builder
	fmt.Fprintf(&b, "💰 Today: $%.2f eq. (%s calls)\n", total, display.FormatCommas(count))

	costs := make(map[string]float64)
	counts := make(map[string]int)
	for _, e := range filtered {
		costs[e.Session] += e.CostUSD
		counts[e.Session]++
	}

	if len(costs) > 0 {
		type sessionCost struct {
			name  string
			cost  float64
			calls int
		}
		sorted := make([]sessionCost, 0, len(costs))
		for s, c := range costs {
			sorted = append(sorted, sessionCost{s, c, counts[s]})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].cost > sorted[j].cost
		})

		shown := sorted
		extra := 0
		if len(sorted) > 10 {
			shown = sorted[:10]
			extra = len(sorted) - 10
		}

		cols := []display.Column{
			{Header: "Session"},
			{Header: "Cost", Align: display.AlignRight},
			{Header: "Calls", Align: display.AlignRight},
		}
		costVals := make([]float64, 0, len(shown)+1)
		for _, sc := range shown {
			costVals = append(costVals, sc.cost)
		}
		costVals = append(costVals, total)
		costCells := moneyCol(costVals, 2)
		tableRows := make([][]string, 0, len(shown)+2)
		for i, sc := range shown {
			tableRows = append(tableRows, []string{
				sc.name,
				costCells[i],
				display.FormatCommas(sc.calls),
			})
		}
		if extra > 0 {
			tableRows = append(tableRows, []string{fmt.Sprintf("  +%d more", extra), "", ""})
		}
		tableRows = append(tableRows, []string{"Total", costCells[len(shown)], display.FormatCommas(count)})
		b.WriteByte('\n')
		b.WriteString(display.MarkdownTable(cols, tableRows))
	}
	return b.String()
}

// cost24h shows the last 24 hours with category breakdown.
func cost24h(entries []log.APIEntry, idx *session.SessionIndex, breakdown bool) string {
	cutoff := time.Now().Add(-24 * time.Hour)
	pred := func(e log.APIEntry) bool {
		return e.Timestamp.After(cutoff)
	}
	if breakdown && idx != nil {
		return renderTypeBreakdown(filterEntries(entries, pred), buildSessionTypeMap(idx), "Last 24h")
	}
	filtered := filterEntries(entries, pred)
	total, _ := sumCosts(filtered)
	cr, cw, inp, out := categoryCosts(filtered)

	var b strings.Builder
	fmt.Fprintf(&b, "API cost (last 24h): $%.2f eq.\n", total)

	cols := []display.Column{
		{Header: "Category"},
		{Header: "Cost", Align: display.AlignRight},
	}
	labels := []string{"Cache reads", "Cache writes", "Input", "Output", "Total"}
	costCells := moneyCol([]float64{cr, cw, inp, out, total}, 2)
	tableRows := make([][]string, len(labels))
	for i, l := range labels {
		tableRows[i] = []string{l, costCells[i]}
	}
	b.WriteByte('\n')
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// costWeek shows a 7-day summary with daily breakdown.
func costWeek(entries []log.APIEntry, idx *session.SessionIndex, breakdown bool) string {
	now := timeutil.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff := startOfToday.AddDate(0, 0, -6)
	pred := func(e log.APIEntry) bool {
		return !e.Timestamp.Before(cutoff)
	}
	if breakdown && idx != nil {
		return renderTypeBreakdown(filterEntries(entries, pred), buildSessionTypeMap(idx), "Last 7 days")
	}
	filtered := filterEntries(entries, pred)

	dayCosts := make(map[string]float64)
	var total float64
	for _, e := range filtered {
		day := e.Timestamp.Local().Format("2006-01-02")
		dayCosts[day] += e.CostUSD
		total += e.CostUSD
	}
	mean := total / 7.0

	var b strings.Builder
	fmt.Fprintf(&b, "API cost (7-day summary): $%.2f eq. (mean $%.2f/day)\n", total, mean)

	cols := []display.Column{
		{Header: "Date"},
		{Header: "Cost", Align: display.AlignRight},
	}
	days := make([]string, 7)
	costVals := make([]float64, 0, 9)
	for i := 0; i < 7; i++ {
		day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
		days[i] = day
		costVals = append(costVals, dayCosts[day])
	}
	costVals = append(costVals, total, mean)
	costCells := moneyCol(costVals, 2)
	tableRows := make([][]string, 0, 9)
	for i := 0; i < 7; i++ {
		tableRows = append(tableRows, []string{days[i], costCells[i]})
	}
	tableRows = append(tableRows, []string{"Total", costCells[7]})
	tableRows = append(tableRows, []string{"Mean/day", costCells[8]})
	b.WriteByte('\n')
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// costDays shows the total cost for the last N days. The scope arg carries the
// day count and may be followed by the `breakdown` modifier.
func costDays(entries []log.APIEntry, args string, idx *session.SessionIndex) string {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "Usage: /cost [today|24h|week|<days>] [breakdown]"
	}
	days, err := strconv.Atoi(fields[0])
	if err != nil {
		return "Usage: /cost [today|24h|week|<days>] [breakdown]"
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	pred := func(e log.APIEntry) bool {
		return e.Timestamp.After(cutoff)
	}
	if breakdownRequested(args) && idx != nil {
		return renderTypeBreakdown(filterEntries(entries, pred), buildSessionTypeMap(idx), fmt.Sprintf("Last %d days", days))
	}
	filtered := filterEntries(entries, pred)
	total, count := sumCosts(filtered)
	return fmt.Sprintf("Last %d days: $%.4f (%d API calls)", days, total, count)
}

// renderTypeBreakdown groups the given entries by session type and renders a
// period total split by type. A period total only — no read/write category
// split and no sub-period rows. Keys absent from the index show as "(untyped)".
func renderTypeBreakdown(filtered []log.APIEntry, typeMap map[string]string, header string) string {
	type agg struct {
		cost     float64
		calls    int
		sessions map[string]struct{}
	}
	aggs := make(map[string]*agg)
	var total float64
	var totalCalls int
	for _, e := range filtered {
		t := typeMap[e.Session]
		if t == "" {
			t = "(untyped)"
		}
		a := aggs[t]
		if a == nil {
			a = &agg{sessions: make(map[string]struct{})}
			aggs[t] = a
		}
		a.cost += e.CostUSD
		a.calls++
		a.sessions[e.Session] = struct{}{}
		total += e.CostUSD
		totalCalls++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "💰 %s by type: $%.2f eq. (%s calls)", header, total, display.FormatCommas(totalCalls))
	if len(aggs) == 0 {
		return b.String()
	}

	types := make([]string, 0, len(aggs))
	for t := range aggs {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		if aggs[types[i]].cost != aggs[types[j]].cost {
			return aggs[types[i]].cost > aggs[types[j]].cost
		}
		return types[i] < types[j]
	})

	cols := []display.Column{
		{Header: "Type"},
		{Header: "Sessions", Align: display.AlignRight},
		{Header: "Calls", Align: display.AlignRight},
		{Header: "Cost", Align: display.AlignRight},
		{Header: "Mean/sess", Align: display.AlignRight},
	}
	costVals := make([]float64, 0, len(types)+1)
	meanVals := make([]float64, 0, len(types))
	for _, t := range types {
		a := aggs[t]
		var mean float64
		if ns := len(a.sessions); ns > 0 {
			mean = a.cost / float64(ns)
		}
		costVals = append(costVals, a.cost)
		meanVals = append(meanVals, mean)
	}
	costVals = append(costVals, total)
	costCells := moneyCol(costVals, 2)
	meanCells := moneyCol(meanVals, 4)

	rows := make([][]string, 0, len(types)+1)
	for i, t := range types {
		a := aggs[t]
		rows = append(rows, []string{
			t,
			display.FormatCommas(len(a.sessions)),
			display.FormatCommas(a.calls),
			costCells[i],
			meanCells[i],
		})
	}
	rows = append(rows, []string{"Total", "", display.FormatCommas(totalCalls), costCells[len(types)], ""})
	b.WriteString("\n\n")
	b.WriteString(display.MarkdownTable(cols, rows))
	return b.String()
}

// buildSessionTypeMap returns a session_key → session_type map across all
// agents (keys are globally unique, so no agent scoping is needed).
func buildSessionTypeMap(idx *session.SessionIndex) map[string]string {
	entries, err := idx.Query(session.QueryOptions{})
	if err != nil {
		return map[string]string{}
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.SessionKey] = string(e.SessionType)
	}
	return m
}

// sessionFamily resolves the full family of a session: its root ancestor plus
// every transitive branch/child (walked via parent_session_key), returned as a
// set of session keys. The second return is the earliest CreatedAt in the
// family (when the conversation began). The requested key is always included.
func sessionFamily(idx *session.SessionIndex, key string) (map[string]struct{}, time.Time) {
	family := map[string]struct{}{key: {}}
	var start time.Time
	entries, err := idx.Query(session.QueryOptions{})
	if err != nil {
		return family, start
	}
	byKey := make(map[string]session.SessionIndexEntry, len(entries))
	children := make(map[string][]string)
	for _, e := range entries {
		byKey[e.SessionKey] = e
		if e.ParentSessionKey != "" {
			children[e.ParentSessionKey] = append(children[e.ParentSessionKey], e.SessionKey)
		}
	}

	// Walk up to the root ancestor (guard against cycles).
	root := key
	seen := map[string]bool{}
	for {
		e, ok := byKey[root]
		if !ok || e.ParentSessionKey == "" || seen[root] {
			break
		}
		seen[root] = true
		root = e.ParentSessionKey
	}

	// Collect the whole subtree rooted at the root ancestor.
	queue := []string{root}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if _, dup := family[k]; dup && k != root {
			continue
		}
		family[k] = struct{}{}
		queue = append(queue, children[k]...)
	}

	for k := range family {
		if e, ok := byKey[k]; ok && !e.CreatedAt.IsZero() {
			if start.IsZero() || e.CreatedAt.Before(start) {
				start = e.CreatedAt
			}
		}
	}
	return family, start
}

// sessionStartLine returns a "Started …" line for a single session key, or ""
// if the index is unavailable or the session has no recorded start.
func sessionStartLine(idx *session.SessionIndex, key string) string {
	if idx == nil {
		return ""
	}
	e, err := idx.Get(key)
	if err != nil {
		return ""
	}
	return startLine(e.CreatedAt)
}

// startLine formats a start timestamp as "Started <local> (<relative>)".
func startLine(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("Started %s (%s)", t.Local().Format("2006-01-02 15:04"), display.RelativeTime(t))
}

// moneyCol renders a column of dollar amounts as equal-width, backtick-wrapped
// cells so the decimals line up under right-alignment (accounting style). The
// pad spaces sit inside the inline-code span, which the app renders monospace
// and preserves verbatim.
func moneyCol(vals []float64, decimals int) []string {
	nums := make([]string, len(vals))
	width := 0
	for i, v := range vals {
		nums[i] = strconv.FormatFloat(v, 'f', decimals, 64)
		if len(nums[i]) > width {
			width = len(nums[i])
		}
	}
	cells := make([]string, len(vals))
	for i, n := range nums {
		cells[i] = "`$" + strings.Repeat(" ", width-len(n)) + n + "`"
	}
	return cells
}

// filterEntries returns entries matching the predicate.
func filterEntries(entries []log.APIEntry, pred func(log.APIEntry) bool) []log.APIEntry {
	var result []log.APIEntry
	for _, e := range entries {
		if pred(e) {
			result = append(result, e)
		}
	}
	return result
}

// sumCosts returns total cost and call count.
func sumCosts(entries []log.APIEntry) (total float64, count int) {
	for _, e := range entries {
		total += e.CostUSD
		count++
	}
	return
}

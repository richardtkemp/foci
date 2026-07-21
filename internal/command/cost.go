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

// costUsage returns the help text for /cost.
func costUsage() string {
	return "Usage: /cost [duration] [scope…] [breakdown]\n" +
		"\n" +
		"Durations (0-1, default: all time):\n" +
		"  today            since midnight\n" +
		"  24h              last 24 hours\n" +
		"  week             last 7 days (calendar-aligned)\n" +
		"  4h / 30m         Go duration notation\n" +
		"  3                last N days\n" +
		"\n" +
		"Scopes (any number; multiple intersect):\n" +
		"  session / self   this session and all descendants\n" +
		"  strict-self      only this session (no descendants)\n" +
		"  descendants      only descendant sessions\n" +
		"  agent            all sessions owned by this agent\n" +
		"  facet reflection chat independent spawn keepalive background-task\n" +
		"\n" +
		"breakdown          split by session type instead of default view"
}

// costRender dispatches to the appropriate renderer based on the parsed
// args. Entries are already filtered by time and scope. The rendering
// priority is:
//  1. breakdown → type breakdown table
//  2. session-family scope → category detail view
//  3. duration = today → per-session table
//  4. duration = week → daily table
//  5. default → summary with category breakdown
func costRender(entries []log.APIEntry, args costArgs, scopeLabel, sessionKey string, idx *session.SessionIndex) string {
	header := costHeader(args, scopeLabel)

	// 1. Breakdown — group by session type
	if args.breakdown && idx != nil {
		breakdownHeader := header
		if hasSessionScope(args.scopes) {
			if _, start := sessionFamily(idx, sessionKey); !start.IsZero() {
				if line := startLine(start); line != "" {
					breakdownHeader += "\n" + line
				}
			}
		}
		return renderTypeBreakdown(entries, buildSessionTypeMap(idx), breakdownHeader)
	}

	// 2. Session-family scope → category detail
	if hasSessionScope(args.scopes) {
		return costCategoryView(entries, header, sessionKey, idx, args.scopes)
	}

	// 3. Today → per-session table
	if args.durKind == durToday {
		return costPerSessionView(entries, header)
	}

	// 4. Week → daily table
	if args.durKind == durWindow && args.durLabel == "7 days" {
		return costDailyView(entries, header)
	}

	// 5. Default → summary with category breakdown
	return costSummaryView(entries, header)
}

// costHeader builds the header label from the duration and scope.
func costHeader(args costArgs, scopeLabel string) string {
	var parts []string
	switch args.durKind {
	case durToday:
		parts = append(parts, "Today")
	case durWindow:
		parts = append(parts, "Last "+args.durLabel)
	}
	if scopeLabel != "" {
		parts = append(parts, scopeLabel)
	}
	if len(parts) == 0 {
		return "All time"
	}
	return strings.Join(parts, " · ")
}

// --- Renderers (all accept pre-filtered entries) ---

// costCategoryView shows total + category breakdown (cache reads/writes/
// input/output/total). Used when scope narrows to the session family.
func costCategoryView(entries []log.APIEntry, header, sessionKey string, idx *session.SessionIndex, scopes []string) string {
	total, count := sumCosts(entries)

	var b strings.Builder
	if count == 0 {
		fmt.Fprintf(&b, "💰 %s: no API calls logged.", header)
	} else {
		fmt.Fprintf(&b, "💰 %s: $%.4f (%s calls)", header, total, display.FormatCommas(count))
	}

	// Show family start time if a session scope is active.
	if hasSessionScope(scopes) && idx != nil {
		if _, start := sessionFamily(idx, sessionKey); !start.IsZero() {
			if line := startLine(start); line != "" {
				b.WriteByte('\n')
				b.WriteString(line)
			}
		}
	}

	if count == 0 {
		return b.String()
	}

	cr, cw, inp, out := categoryCosts(entries)
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

// costPerSessionView shows a per-session breakdown table sorted by cost.
func costPerSessionView(entries []log.APIEntry, header string) string {
	total, count := sumCosts(entries)

	var b strings.Builder
	fmt.Fprintf(&b, "💰 %s: $%.2f eq. (%s calls)", header, total, display.FormatCommas(count))

	costs := make(map[string]float64)
	counts := make(map[string]int)
	for _, e := range entries {
		costs[e.Session] += e.EffectiveCost()
		counts[e.Session]++
	}

	if len(costs) == 0 {
		return b.String()
	}

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
	return b.String()
}

// costDailyView shows a daily cost breakdown for the last 7 days.
func costDailyView(entries []log.APIEntry, header string) string {
	now := timeutil.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	dayCosts := make(map[string]float64)
	var total float64
	for _, e := range entries {
		day := e.Timestamp.Local().Format("2006-01-02")
		dayCosts[day] += e.EffectiveCost()
		total += e.EffectiveCost()
	}
	mean := total / 7.0

	var b strings.Builder
	fmt.Fprintf(&b, "💰 %s: $%.2f eq. (mean $%.2f/day)", header, total, mean)

	cols := []display.Column{
		{Header: "Date"},
		{Header: "Cost", Align: display.AlignRight},
	}
	costVals := make([]float64, 0, 9)
	for i := 0; i < 7; i++ {
		day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
		costVals = append(costVals, dayCosts[day])
	}
	costVals = append(costVals, total, mean)
	costCells := moneyCol(costVals, 2)
	tableRows := make([][]string, 0, 9)
	for i := 0; i < 7; i++ {
		day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
		tableRows = append(tableRows, []string{day, costCells[i]})
	}
	tableRows = append(tableRows, []string{"Total", costCells[7]})
	tableRows = append(tableRows, []string{"Mean/day", costCells[8]})
	b.WriteByte('\n')
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// costSummaryView shows a total + category breakdown table for the
// filtered entries. Used when no special view applies.
func costSummaryView(entries []log.APIEntry, header string) string {
	total, count := sumCosts(entries)

	var b strings.Builder
	fmt.Fprintf(&b, "💰 %s: $%.2f eq. (%s calls)", header, total, display.FormatCommas(count))
	if count == 0 {
		return b.String()
	}

	cr, cw, inp, out := categoryCosts(entries)
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

// --- Shared helpers ---

// renderTypeBreakdown groups the given entries by session type and renders a
// period total split by type. Keys absent from the index show as "(untyped)".
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
		a.cost += e.EffectiveCost()
		a.calls++
		a.sessions[e.Session] = struct{}{}
		total += e.EffectiveCost()
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
	if idx == nil {
		return family, start
	}
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

// startLine formats a start timestamp as "Started <local> (<relative>)".
func startLine(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("Started %s (%s)", t.Local().Format("2006-01-02 15:04"), display.RelativeTime(t))
}

// moneyCol renders a column of dollar amounts as equal-width, backtick-wrapped
// cells so the decimals line up under right-alignment (accounting style).
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
		total += e.EffectiveCost()
		count++
	}
	return
}

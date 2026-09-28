package command

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/linkwalk"
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
		"  agent            all sessions owned by this agent (default)\n" +
		"  all              every agent in the household\n" +
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
	// Appended to whichever view runs below, rather than added to each, so one
	// place owns it and no view can silently miss it.
	suffix := subagentBreakdown(entries)
	header := costHeader(args, scopeLabel)

	// 1. Breakdown — group by session type
	if args.breakdown && idx != nil {
		breakdownHeader := header
		if hasSessionScope(args.scopes) {
			if _, start, _ := sessionFamily(idx, sessionKey); !start.IsZero() {
				if line := startLine(start); line != "" {
					breakdownHeader += "\n" + line
				}
			}
		}
		typeMap, err := buildSessionTypeMap(idx)
		if err != nil {
			breakdownHeader += fmt.Sprintf("\n⚠️ %v — session types unavailable, rows show as (untyped)", err)
		}
		return renderTypeBreakdown(entries, typeMap, breakdownHeader) + suffix
	}

	// 2. Session-family scope → category detail
	if hasSessionScope(args.scopes) {
		return costCategoryView(entries, header, sessionKey, idx, args.scopes) + suffix
	}

	// 3. Today → per-session table
	if args.durKind == durToday {
		return costPerSessionView(entries, header) + suffix
	}

	// 4. Week → daily table
	if args.durKind == durWindow && args.durLabel == "7 days" {
		return costDailyView(entries, header) + suffix
	}

	// 5. Default → summary with category breakdown
	return costSummaryView(entries, header) + suffix
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
		if _, start, _ := sessionFamily(idx, sessionKey); !start.IsZero() {
			if line := startLine(start); line != "" {
				b.WriteByte('\n')
				b.WriteString(line)
			}
		}
	}

	if count == 0 {
		return b.String()
	}

	labels, vals := categoryRows(entries, total)
	cols := []display.Column{
		{Header: "Category"},
		{Header: "Cost", Align: display.AlignRight},
	}
	costCells := moneyCol(vals, 4)
	tableRows := make([][]string, len(labels))
	for i, l := range labels {
		tableRows[i] = []string{l, costCells[i]}
	}
	b.WriteString("\n\n")
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// categoryRows is the per-category table's labels and values, ending in Total.
// Web search gets a row only when the entries made any: it is absent from
// nearly every window, and a permanent $0.0000 line would be noise.
func categoryRows(entries []log.APIEntry, total float64) ([]string, []float64) {
	cr, cw, inp, out, search := categoryCosts(entries)
	labels := []string{"Cache reads", "Cache writes", "Input", "Output"}
	vals := []float64{cr, cw, inp, out}
	if search > 0 {
		labels = append(labels, "Web search")
		vals = append(vals, search)
	}
	return append(labels, "Total"), append(vals, total)
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
		if !e.IsSubagent() {
			counts[e.Session]++
		}
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

	labels, vals := categoryRows(entries, total)
	cols := []display.Column{
		{Header: "Category"},
		{Header: "Cost", Align: display.AlignRight},
	}
	costCells := moneyCol(vals, 2)
	tableRows := make([][]string, len(labels))
	for i, l := range labels {
		tableRows[i] = []string{l, costCells[i]}
	}
	b.WriteByte('\n')
	b.WriteString(display.MarkdownTable(cols, tableRows))
	return b.String()
}

// subagentBreakdown renders per-subagent spend, or "" when the window contains
// no subagent rows.
//
// This is the question #1863 was opened for and that nothing could answer: the
// delegate skill spawns subagents constantly and their spend was attributed to
// whatever parent turn happened to be open, so "what did that delegation cost"
// had no answer at all. One subagent measured $4.2578 across 29 calls — more
// than the parent turn it was charged to.
//
// Grouped by agent rather than by (agent, model) because the agent is the unit
// a reader is asking about; a subagent that used more than one model shows them
// joined, which is rare and worth seeing when it happens.
func subagentBreakdown(entries []log.APIEntry) string {
	type sub struct {
		cost   float64
		models map[string]struct{}
		rows   int
	}
	subs := make(map[string]*sub)
	var total float64
	for _, e := range entries {
		if !e.IsSubagent() {
			continue
		}
		// SubagentID (NOT AgentID, which #1946 made the OWNING agent — the
		// same value for every row of a delegation) is what distinguishes
		// one subagent from another here.
		id := e.SubagentID
		if id == "" {
			// Usage that arrived before its task_started named the agent. Kept
			// rather than dropped: the money is real and belongs to SOME
			// subagent, and hiding it would leave this table short against the
			// total above it.
			id = "(unnamed)"
		}
		x := subs[id]
		if x == nil {
			x = &sub{models: make(map[string]struct{})}
			subs[id] = x
		}
		x.cost += e.EffectiveCost()
		x.models[e.Model] = struct{}{}
		x.rows++
		total += e.EffectiveCost()
	}
	if len(subs) == 0 {
		return ""
	}

	ids := make([]string, 0, len(subs))
	for id := range subs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return subs[ids[i]].cost > subs[ids[j]].cost })

	cols := []display.Column{
		{Header: "Subagent"},
		{Header: "Model"},
		{Header: "Turns", Align: display.AlignRight},
		{Header: "Cost", Align: display.AlignRight},
	}
	rows := make([][]string, 0, len(ids))
	costs := make([]float64, 0, len(ids))
	for _, id := range ids {
		costs = append(costs, subs[id].cost)
	}
	cells := moneyCol(costs, 4)
	for i, id := range ids {
		x := subs[id]
		ms := make([]string, 0, len(x.models))
		for m := range x.models {
			ms = append(ms, strings.TrimPrefix(m, "claude/"))
		}
		sort.Strings(ms)
		rows = append(rows, []string{id, strings.Join(ms, ", "), strconv.Itoa(x.rows), cells[i]})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n\n🔻 Subagent spend: $%.4f across %d", total, len(ids))
	if len(ids) == 1 {
		b.WriteString(" subagent")
	} else {
		b.WriteString(" subagents")
	}
	b.WriteString("\n")
	b.WriteString(display.MarkdownTable(cols, rows))
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
		a.sessions[e.Session] = struct{}{}
		total += e.EffectiveCost()
		// Cost counts every row, calls do not — see sumCosts.
		if !e.IsSubagent() {
			a.calls++
			totalCalls++
		}
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
// agents (keys are globally unique, so no agent scoping is needed). On a query
// error the map is empty and the error is returned so the caller can say so —
// otherwise every row silently renders as (untyped).
func buildSessionTypeMap(idx *session.SessionIndex) (map[string]string, error) {
	entries, err := idx.Query(session.QueryOptions{})
	if err != nil {
		return map[string]string{}, fmt.Errorf("session index query: %w", err)
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.SessionKey] = string(e.SessionType)
	}
	return m, nil
}

// sessionFamily resolves the full family of a session: its root ancestor plus
// every transitive branch/child (walked via parent_session_key), returned as a
// set of session keys. The second return is the earliest CreatedAt in the
// family (when the conversation began). The requested key is always included.
// On a query error the family is just the requested key and the error is
// returned, so the caller can say the family is incomplete rather than
// quietly under-reporting.
func sessionFamily(idx *session.SessionIndex, key string) (map[string]struct{}, time.Time, error) {
	family := map[string]struct{}{key: {}}
	var start time.Time
	if idx == nil {
		return family, start, nil
	}
	entries, err := idx.Query(session.QueryOptions{})
	if err != nil {
		return family, start, fmt.Errorf("session index query: %w", err)
	}
	byKey := make(map[string]session.SessionIndexEntry, len(entries))
	children := make(map[string][]string)
	for _, e := range entries {
		byKey[e.SessionKey] = e
		if e.ParentSessionKey != "" {
			children[e.ParentSessionKey] = append(children[e.ParentSessionKey], e.SessionKey)
		}
	}

	// Both walks go through linkwalk, which stops at the first repeated key. A
	// session_index row can name itself as its parent, or two rows can name each
	// other; the downward walk once looped forever on the first shape (#1581).
	// On a cycle, root is a key on the cycle, and the downward walk from it
	// still reaches every key that hangs off the cycle.
	root, _ := linkwalk.Up(key, func(k string) (string, bool) {
		e, ok := byKey[k]
		return e.ParentSessionKey, ok && e.ParentSessionKey != ""
	})
	for k := range linkwalk.Down(root, func(k string) []string { return children[k] }) {
		family[k] = struct{}{}
	}

	for k := range family {
		if e, ok := byKey[k]; ok && !e.CreatedAt.IsZero() {
			if start.IsZero() || e.CreatedAt.Before(start) {
				start = e.CreatedAt
			}
		}
	}
	return family, start, nil
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
//
// The two halves treat subagent rows differently on purpose. Cost sums EVERY
// row: a subagent row's cost was SUBTRACTED from the parent row beside it, so
// skipping it would under-report the session. The count skips them, because
// they are not calls the user made — one turn that spawned three subagents is
// one call and four rows, and counting four would inflate every "N calls"
// figure the moment #1880 phase C shipped.
func sumCosts(entries []log.APIEntry) (total float64, count int) {
	for _, e := range entries {
		total += e.EffectiveCost()
		if !e.IsSubagent() {
			count++
		}
	}
	return
}

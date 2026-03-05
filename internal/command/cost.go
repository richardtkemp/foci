package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/table"
)

// costUsage returns the help text for /cost subcommands.
func costUsage() string {
	return "/cost today — today's costs by session\n/cost 24h — last 24 hours with category breakdown\n/cost week — 7-day summary with daily breakdown\n/cost <days> — total for last N days"
}

// costToday shows today's total with per-session breakdown.
func costToday(entries []apiEntry, ctx context.Context) (string, error) {
	today := time.Now().UTC().Format("2006-01-02")
	filtered := filterEntries(entries, func(e apiEntry) bool {
		return e.Timestamp.Format("2006-01-02") == today
	})
	total, count := sumCosts(filtered)

	var b strings.Builder
	fmt.Fprintf(&b, "💰 Today: $%.2f eq. (%s calls)\n", total, formatCommas(count))

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

		cols := []table.Column{
			{Header: "Session"},
			{Header: "Cost", Align: table.AlignRight},
			{Header: "Calls", Align: table.AlignRight},
		}
		tableRows := make([][]string, 0, len(shown)+1)
		for _, sc := range shown {
			tableRows = append(tableRows, []string{
				sc.name,
				fmt.Sprintf("$%.2f", sc.cost),
				formatCommas(sc.calls),
			})
		}
		if extra > 0 {
			tableRows = append(tableRows, []string{fmt.Sprintf("  +%d more", extra), "", ""})
		}
		tableRows = append(tableRows, []string{"Total", fmt.Sprintf("$%.2f", total), formatCommas(count)})
		b.WriteString("\n```\n")
		b.WriteString(table.FormatWidth(cols, tableRows, displayWidth(ctx)))
		b.WriteString("\n```")
	}
	return b.String(), nil
}

// cost24h shows the last 24 hours with category breakdown.
func cost24h(entries []apiEntry, ctx context.Context) (string, error) {
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	filtered := filterEntries(entries, func(e apiEntry) bool {
		return e.Timestamp.After(cutoff)
	})
	total, _ := sumCosts(filtered)
	cr, cw, inp, out := categoryCosts(filtered)

	var b strings.Builder
	fmt.Fprintf(&b, "API cost (last 24h): $%.2f eq.\n", total)
	b.WriteString("\n```\n")

	cols := []table.Column{
		{Header: "Category"},
		{Header: "Cost", Align: table.AlignRight},
	}
	tableRows := [][]string{
		{"Cache reads", fmt.Sprintf("$%.2f", cr)},
		{"Cache writes", fmt.Sprintf("$%.2f", cw)},
		{"Input", fmt.Sprintf("$%.2f", inp)},
		{"Output", fmt.Sprintf("$%.2f", out)},
	}
	tableRows = append(tableRows, []string{"Total", fmt.Sprintf("$%.2f", total)})
	b.WriteString(table.FormatWidth(cols, tableRows, displayWidth(ctx)))
	b.WriteString("\n```")
	return b.String(), nil
}

// costWeek shows a 7-day summary with daily breakdown.
func costWeek(entries []apiEntry, ctx context.Context) (string, error) {
	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := startOfToday.AddDate(0, 0, -6)
	filtered := filterEntries(entries, func(e apiEntry) bool {
		return !e.Timestamp.Before(cutoff)
	})

	dayCosts := make(map[string]float64)
	var total float64
	for _, e := range filtered {
		day := e.Timestamp.Format("2006-01-02")
		dayCosts[day] += e.CostUSD
		total += e.CostUSD
	}
	mean := total / 7.0

	var b strings.Builder
	fmt.Fprintf(&b, "API cost (7-day summary): $%.2f eq. (mean $%.2f/day)\n", total, mean)
	b.WriteString("\n```\n")

	cols := []table.Column{
		{Header: "Date"},
		{Header: "Cost", Align: table.AlignRight},
	}
	tableRows := make([][]string, 0, 9)
	for i := 0; i < 7; i++ {
		day := startOfToday.AddDate(0, 0, -i).Format("2006-01-02")
		tableRows = append(tableRows, []string{day, fmt.Sprintf("$%.2f", dayCosts[day])})
	}
	tableRows = append(tableRows, []string{"Total", fmt.Sprintf("$%.2f", total)})
	tableRows = append(tableRows, []string{"Mean/day", fmt.Sprintf("$%.2f", mean)})
	b.WriteString(table.FormatWidth(cols, tableRows, displayWidth(ctx)))
	b.WriteString("\n```")
	return b.String(), nil
}

// costDays shows the total cost for the last N days.
func costDays(entries []apiEntry, scope string) (string, error) {
	days, err := strconv.Atoi(scope)
	if err != nil {
		return "Usage: /cost [today|24h|week|<days>]", nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	filtered := filterEntries(entries, func(e apiEntry) bool {
		return e.Timestamp.After(cutoff)
	})
	total, count := sumCosts(filtered)
	return fmt.Sprintf("Last %d days: $%.4f (%d API calls)", days, total, count), nil
}

// filterEntries returns entries matching the predicate.
func filterEntries(entries []apiEntry, pred func(apiEntry) bool) []apiEntry {
	var result []apiEntry
	for _, e := range entries {
		if pred(e) {
			result = append(result, e)
		}
	}
	return result
}

// sumCosts returns total cost and call count.
func sumCosts(entries []apiEntry) (total float64, count int) {
	for _, e := range entries {
		total += e.CostUSD
		count++
	}
	return
}

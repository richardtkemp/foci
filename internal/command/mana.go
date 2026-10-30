package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
)

// ManaCommand creates a /mana command that reports the usage allowance of the
// account agentID's backend runs under, via the backend's query
// (Spec.UsageQuery). Only a backend with a usage source has one, so only its
// agents get the command, and each reports only its own account (#1543):
//
//   - Claude Code (ccstream.QueryUsage): session (5h) and weekly (7d)
//     percentages, cost, and what's contributing. It spawns its own throwaway
//     CC process and never touches or waits behind a live session.
//   - Codex (codex.QueryUsage): each metered bucket's windows, credits and
//     limit-reached state for this agent's codex account.
//
// /usage is a Command.Aliases entry, not a separate Command — Registry.All()
// (feeding /help and the app command palette) dedupes by *Command pointer
// identity precisely so an alias never gets its own listing: both "mana" and
// "usage" map to this same struct, so it surfaces exactly once, under its
// canonical Name ("mana").
func ManaCommand(query delegator.UsageQuery, agentID string) *Command {
	return &Command{
		Name:        "mana",
		Aliases:     []string{"usage"},
		Description: "Show the backend's plan usage (windows used %, resets, limits)",
		Category:    "observability",
		Execute: func(ctx context.Context, _ Request, _ CommandContext) (Response, error) {
			info, err := query(ctx, agentID)
			if err != nil {
				return Response{}, fmt.Errorf("query usage: %w", err)
			}
			return Response{Text: formatUsage(info)}, nil
		},
	}
}

// formatUsage renders a UsageInfo as the /mana reply text, headed by the
// backend it describes. Each shape shows only the fields its backend has.
func formatUsage(info *delegator.UsageInfo) string {
	if info.Allowance != nil {
		return formatAllowance(info.Backend, info.Allowance)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📊 *%s usage* (%s)\n\n", info.Backend, info.SubscriptionType)
	fmt.Fprintf(&b, "Session (5h): %d%% · resets %s\n", info.FiveHour.Percent, formatResetTime(info.FiveHour))
	fmt.Fprintf(&b, "Week (7d): %d%% · resets %s\n", info.SevenDay.Percent, formatResetTime(info.SevenDay))
	if info.SessionCostUSD > 0 {
		fmt.Fprintf(&b, "\nThis check's session cost so far: $%.4f\n", info.SessionCostUSD)
	}
	if len(info.Day.Top) > 0 || info.Day.RequestCount > 0 {
		fmt.Fprintf(&b, "\nLast 24h · %d requests · %d sessions\n", info.Day.RequestCount, info.Day.SessionCount)
		for _, it := range info.Day.Top {
			fmt.Fprintf(&b, "  %d%% %s\n", it.Pct, it.Key)
		}
	}
	if len(info.Week.Top) > 0 || info.Week.RequestCount > 0 {
		fmt.Fprintf(&b, "\nLast 7d · %d requests · %d sessions\n", info.Week.RequestCount, info.Week.SessionCount)
		for _, it := range info.Week.Top {
			fmt.Fprintf(&b, "  %d%% %s\n", it.Pct, it.Key)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatResetTime(w delegator.UsageWindow) string {
	return formatInstant(w.ResetsAt)
}

func formatInstant(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("Mon 2 Jan 15:04 MST")
}

// formatAllowance renders a per-bucket allowance (Codex), e.g.
//
//	📊 *Codex usage* (team)
//
//	5h: 34% · resets Thu 30 Oct 15:04 GMT
//	7d: 12% · resets Tue 4 Nov 10:07 GMT
//
//	premium: credits 0 · limit reached: workspace out of credits
//
//	Read from Codex just now.
func formatAllowance(backend string, a *delegator.Allowance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 *%s usage*", backend)
	if a.PlanType != "" {
		fmt.Fprintf(&b, " (%s)", a.PlanType)
	}
	b.WriteString("\n")
	if len(a.Buckets) == 0 {
		fmt.Fprintf(&b, "\n%s has not reported an allowance since foci started.\n", backend)
	}
	for _, bk := range a.Buckets {
		prefix := ""
		if bk.Name != "" {
			prefix = bk.Name + " "
		}
		b.WriteString("\n")
		for _, w := range bk.Windows {
			fmt.Fprintf(&b, "%s%s: %d%% · resets %s\n", prefix, w.Label, w.Percent, formatInstant(w.ResetsAt))
		}
		var extra []string
		if bk.Credits != "" {
			extra = append(extra, "credits "+bk.Credits)
		}
		if bk.LimitReached != "" {
			extra = append(extra, "🛑 limit reached: "+bk.LimitReached)
		}
		if len(extra) > 0 {
			label := strings.TrimSpace(prefix)
			if label == "" {
				label = "limits"
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(extra, " · "))
		}
	}
	b.WriteString("\n")
	switch {
	case a.Fresh:
		fmt.Fprintf(&b, "Read from %s just now.", backend)
	case a.AsOf.IsZero():
		if a.Note != "" {
			fmt.Fprintf(&b, "No fresh read: %s.", a.Note)
		}
	default:
		fmt.Fprintf(&b, "Last reported by %s at %s", backend, formatInstant(a.AsOf))
		if a.Note != "" {
			fmt.Fprintf(&b, " (no fresh read: %s)", a.Note)
		}
		b.WriteString(".")
	}
	return strings.TrimRight(b.String(), "\n")
}

package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/delegator/ccstream"
)

// ManaCommand creates a /mana command that reports the account's Claude Code
// plan/rate-limit usage — session (5h) and weekly (7d) percentages, cost, and
// what's contributing to the usage — via ccstream.QueryUsage. Unlike /context
// (which queries a session's already-running backend), this spawns its own
// independent, throwaway CC process (see QueryUsage's doc comment) — it never
// touches or waits behind a live session, and works regardless of which
// transport (API or delegated) the requesting agent itself uses.
//
// /usage is a Command.Aliases entry, not a separate Command — Registry.All()
// (feeding /help and the app command palette) dedupes by *Command pointer
// identity precisely so an alias never gets its own listing: both "mana" and
// "usage" map to this same struct, so it surfaces exactly once, under its
// canonical Name ("mana").
func ManaCommand() *Command {
	return &Command{
		Name:        "mana",
		Aliases:     []string{"usage"},
		Description: "Show Claude Code plan usage (session/weekly %, cost, contributing behaviors)",
		Category:    "observability",
		Execute: func(ctx context.Context, _ Request, _ CommandContext) (Response, error) {
			info, err := ccstream.QueryUsage(ctx)
			if err != nil {
				return Response{}, fmt.Errorf("query usage: %w", err)
			}
			return Response{Text: formatUsage(info)}, nil
		},
	}
}

// formatUsage renders a UsageInfo as the /mana reply text.
func formatUsage(info *ccstream.UsageInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 *Claude Code usage* (%s)\n\n", info.SubscriptionType)
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

func formatResetTime(w ccstream.UsageWindow) string {
	if w.ResetsAt.IsZero() {
		return "unknown"
	}
	return w.ResetsAt.Local().Format("Mon 2 Jan 15:04 MST")
}

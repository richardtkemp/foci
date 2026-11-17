package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/timeutil"
	"foci/internal/usagelog"
)

// Usage warnings carry a table of the account's windows (#3154, Dick
// 2026-11-17: "the usage warnings which show in chat should show all
// relevant windows. If one is severe (>90%) show it bold. Use a table,
// columns window, percent, reset date time, reset until").
//
// A usage warning (CC's rate_limit_event notice, a scheduled usage read's
// notice, codex's rate-limit notice) is sent with a markdown table of every
// window its account's usage history holds (#3103) that is relevant now:
//
//   - a time window (it has a length: not overage, not a bucket's bare
//     "limit" entry, which have no reset period to show);
//   - with a figure for its current period, or whose limit is reached;
//   - whose reset, when known, is still ahead (a figure from before the
//     reset says nothing about the new period).
//
// The backend stores each report before it raises the notice, so the
// window that triggered the warning is in the table with its new figure.
// A row above usageSevereAbove percent, or whose limit is reached, is bold.
// With no relevant window (no usage history, a failed read) the notice is
// sent as before, without a table.

// usageSevereAbove is the percent above which a window's row is bold.
const usageSevereAbove = 90

// withUsageTable is notice followed by the usage table of hist's account
// (usageNoticeTable), or notice alone when there is none.
func withUsageTable(notice string, hist *usagelog.Binding) string {
	if hist == nil {
		return notice
	}
	cur, now, err := hist.Current()
	if err != nil {
		mainLog.Debugf("usage table for %s: %v", hist.AgentID, err)
		return notice
	}
	table := usageNoticeTable(cur, now)
	if table == "" {
		return notice
	}
	return dropTabledLines(notice) + "\n\n" + table
}

// dropTabledLines removes the Claude Code notice's own "Usage:" and
// "Resets:" lines (ccstream.FormatRateLimitNotice), which repeat the table's
// row for the window that raised the notice. Dick 2026-11-17 (Q97): with a
// table attached the message is the headline plus the table. Every other
// line (the headline, "Overage:", a codex one-line notice) is kept.
func dropTabledLines(notice string) string {
	lines := strings.Split(notice, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "Usage: ") || strings.HasPrefix(l, "Resets: ") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// usageNoticeTable renders cur's relevant windows (see the file comment) at
// now as a markdown table, in Current's order (shortest window first), or ""
// when none is relevant.
func usageNoticeTable(cur usagelog.Current, now time.Time) string {
	var rows []string
	for _, w := range cur.Windows {
		if w.Length <= 0 || (!w.HasPercent && !w.LimitReached) {
			continue
		}
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			continue
		}
		pct := "—"
		severe := w.LimitReached
		if w.HasPercent {
			p := int(math.Round(math.Min(math.Max(w.Percent, 0), 100)))
			pct = fmt.Sprintf("%d%%", p)
			severe = severe || p > usageSevereAbove
		}
		if w.LimitReached {
			pct += " (limit reached)"
		}
		at, in := "unknown", "unknown"
		if !w.ResetsAt.IsZero() {
			at = w.ResetsAt.In(timeutil.Location()).Format("Mon 2 Jan 15:04")
			in = untilReset(w.ResetsAt.Sub(now))
		}
		cells := []string{delegator.UsageWindowLabel(w.Window), pct, at, in}
		if severe {
			for i, c := range cells {
				cells[i] = "**" + c + "**"
			}
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
	}
	if len(rows) == 0 {
		return ""
	}
	return "| Window | Percent | Reset at | Reset in |\n|---|---|---|---|\n" + strings.Join(rows, "\n")
}

// untilReset renders a positive duration compactly: "2d 5h", "3h 12m",
// "42m", "<1m".
func untilReset(d time.Duration) string {
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hrs)
	case hrs > 0:
		return fmt.Sprintf("%dh %dm", hrs, mins)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return "<1m"
	}
}

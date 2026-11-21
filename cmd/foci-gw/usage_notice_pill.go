package main

import (
	"math"
	"sort"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/usagelog"
)

// Usage warnings as a pill in the app (#3249, Dick 2026-11-21 16:09, mockup
// B: "one-line pill"). A usage warning goes to the app with its account's
// windows as structured data (platform.UsageSnapshot), so the app draws the
// agent's avatar, a mini ring per window and the warned window's reset on one
// line. The windows are the ones the text's table shows (usageNoticeTable's
// rule: time windows with a figure for the current period, or a reached
// limit, whose reset is still ahead), so the pill and the text never differ.

// usageSnapshotOf is the windows of hist's account for notice, or nil when
// there is no history or no relevant window (the app then shows the text).
func usageSnapshotOf(notice string, hist *usagelog.Binding) *platform.UsageSnapshot {
	if hist == nil {
		return nil
	}
	cur, now, err := hist.Current()
	if err != nil {
		mainLog.Debugf("usage pill for %s: %v", hist.AgentID, err)
		return nil
	}
	return usageSnapshotFrom(notice, cur, now)
}

// usageSnapshotFrom is usageSnapshotOf on cur at now.
func usageSnapshotFrom(notice string, cur usagelog.Current, now time.Time) *platform.UsageSnapshot {
	var ws []platform.UsageSnapshotWindow
	for _, w := range cur.Windows {
		if w.Length <= 0 || (!w.HasPercent && !w.LimitReached) {
			continue
		}
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			continue
		}
		pct := 100
		if w.HasPercent {
			pct = int(math.Round(math.Min(math.Max(w.Percent, 0), 100)))
		}
		ws = append(ws, platform.UsageSnapshotWindow{
			Label:        delegator.UsageWindowLabel(w.Window),
			Length:       w.Length,
			UsedPct:      pct,
			ResetsAt:     w.ResetsAt,
			LimitReached: w.LimitReached,
		})
	}
	if len(ws) == 0 {
		return nil
	}
	return &platform.UsageSnapshot{Windows: ws, Warned: warnedWindow(notice, ws)}
}

// warnedWindow is the Label of the window notice is about: a window its
// headline names (Claude Code: "Approaching Anthropic 7-day rate limit",
// "requests on the 5-hour window"; codex: "Codex 7d usage at 91%"), the most
// severe when it names several (a read's joined notices). With none named it
// is the most severe window: the pill still shows the reset that matters most.
func warnedWindow(notice string, ws []platform.UsageSnapshotWindow) string {
	text := " " + strings.Join(headlineParts(notice), " ") + " "
	// Longest label first, each match blanked out, so "7d" does not also
	// match inside "premium 7d".
	order := make([]int, len(ws))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(ws[order[a]].Label) > len(ws[order[b]].Label) })
	best := -1
	for _, i := range order {
		matched := false
		for _, n := range windowNeedles(ws[i].Label) {
			if strings.Contains(text, n) {
				matched = true
				text = strings.ReplaceAll(text, n, " ")
			}
		}
		if matched && (best < 0 || moreSevere(ws[i], ws[best])) {
			best = i
		}
	}
	if best < 0 {
		for i := range ws {
			if best < 0 || moreSevere(ws[i], ws[best]) {
				best = i
			}
		}
	}
	return ws[best].Label
}

// headlineParts is notice without the parts that carry durations rather than
// window names: Claude Code's "Usage:", "Resets:" and "Overage:" lines and a
// codex line's " Resets …" tail ("Resets at Thu 15:04 (in 2d 5h)").
func headlineParts(notice string) []string {
	var out []string
	for _, l := range strings.Split(notice, "\n") {
		if strings.HasPrefix(l, "Usage: ") || strings.HasPrefix(l, "Resets: ") || strings.HasPrefix(l, "Overage: ") {
			continue
		}
		if i := strings.Index(l, " Resets "); i >= 0 {
			l = l[:i]
		}
		if i := strings.Index(l, " Included usage resets "); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return out
}

// windowNeedles are the space-delimited ways a notice names the window
// labelled label: the label itself (codex: "7d", "premium 5h"), Claude Code's
// long form of a plain 5h or 7d window ("5-hour", "7-day"), and Claude Code's
// raw window type for any other ("seven_day_opus" for "7d opus"), which its
// notice prints as is.
func windowNeedles(label string) []string {
	out := []string{" " + label + " "}
	for _, p := range []struct{ short, long, raw string }{{"5h", "5-hour", "five_hour"}, {"7d", "7-day", "seven_day"}} {
		rest, ok := strings.CutPrefix(label, p.short)
		switch {
		case !ok:
		case rest == "":
			out = append(out, " "+p.long+" ")
		case strings.HasPrefix(rest, " "):
			out = append(out, " "+p.raw+strings.ReplaceAll(rest, " ", "_")+" ")
		}
	}
	return out
}

// moreSevere reports whether a is nearer its limit than b: a reached limit
// first, then the higher percent.
func moreSevere(a, b platform.UsageSnapshotWindow) bool {
	if a.LimitReached != b.LimitReached {
		return a.LimitReached
	}
	return a.UsedPct > b.UsedPct
}

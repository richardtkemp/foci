package command

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagelog"
)

// /mana cached (#3103): the usage figures foci already holds, from the usage
// history store only (usagelog). It never starts or waits on a backend read,
// so it answers at once even while a read runs or hangs, and it never lifts
// the rate-limit gate or the usage hold (only a fresh read does, #3043).
//
// "/mana cached json" gives the same figures as one JSON object for a script
// (the keys are in manaCachedHelp, shown by /help mana).

// manaCachedHelp documents /mana cached and its JSON keys (/help mana).
const manaCachedHelp = "**/mana cached** answers at once from the usage figures foci already holds: " +
	"each limit window's newest percent used, its reset time, and how old the figure is. " +
	"It never makes a fresh read and never lifts a limit. A figure from any source counts: " +
	"the scheduled reads, a spawn's read, a fresh /mana read, and the backend's own usage events.\n\n" +
	"**/mana cached json** prints one JSON object for a script. Keys (times are Unix seconds, UTC; null = unknown):\n" +
	"- `account`: the usage account the figures describe\n" +
	"- `account_name`: its name for a person (\"\" when none)\n" +
	"- `backend`: the backend that reported them (claude-code, codex)\n" +
	"- `now`: when the reply was made\n" +
	"- `has_figures`: false when foci holds no figure for the account\n" +
	"- `windows`: one object per limit window:\n" +
	"  - `window`: the window's name (Claude Code: five_hour, seven_day, ...; codex: <bucket>/<length>, e.g. codex/5h)\n" +
	"  - `length_seconds`: the window's length (18000 = 5 hours, 604800 = 7 days; 0 = unknown, or a bucket's limit state)\n" +
	"  - `used_percent`: the newest percent used held for the current window, 0-100; null when none is held\n" +
	"  - `used_received_at`, `used_age_seconds`, `used_source`: when foci received that percent figure, its age, and its source\n" +
	"  - `resets_at`: when the window resets, from the newest report of the window\n" +
	"  - `received_at`, `age_seconds`, `source`: when foci received the newest report of the window, its age, and its source\n" +
	"  - `status`: the backend's word for the window's state in that report (\"\" when none)\n" +
	"  - `limit_reached`: true when that report says the window's limit is reached\n\n" +
	"Sources: scheduled, spawn, mana, cc_rate_limit_event, codex_rate_limits_updated, codex_startup_read."

// manaCachedReply is the JSON form of /mana cached.
type manaCachedReply struct {
	Account     string              `json:"account"`
	AccountName string              `json:"account_name"`
	Backend     string              `json:"backend"`
	Now         int64               `json:"now"`
	HasFigures  bool                `json:"has_figures"`
	Windows     []manaCachedWindowJ `json:"windows"`
}

type manaCachedWindowJ struct {
	Window         string   `json:"window"`
	LengthSeconds  int64    `json:"length_seconds"`
	UsedPercent    *float64 `json:"used_percent"`
	UsedReceivedAt *int64   `json:"used_received_at"`
	UsedAgeSeconds *int64   `json:"used_age_seconds"`
	UsedSource     *string  `json:"used_source"`
	ResetsAt       *int64   `json:"resets_at"`
	ReceivedAt     int64    `json:"received_at"`
	AgeSeconds     int64    `json:"age_seconds"`
	Source         string   `json:"source"`
	Status         string   `json:"status"`
	LimitReached   bool     `json:"limit_reached"`
}

// manaCached answers /mana cached [json] from hist.
func manaCached(hist *usagelog.Binding, args string, cc CommandContext) Response {
	cur, now, err := hist.Current()
	if err != nil {
		return Response{Text: "Could not read the stored usage figures: " + err.Error()}
	}
	name := ""
	if cc.Agent != nil {
		name = cc.Agent.UsageAccountName()
	}
	if strings.EqualFold(strings.TrimSpace(args), "json") {
		return Response{Text: manaCachedJSON(cur, name, now)}
	}
	return Response{Text: manaCachedText(cur, name, now)}
}

func manaCachedJSON(cur usagelog.Current, name string, now time.Time) string {
	out := manaCachedReply{
		Account: cur.Account, AccountName: name, Backend: cur.Backend, Now: now.Unix(),
		HasFigures: len(cur.Windows) > 0, Windows: []manaCachedWindowJ{},
	}
	for _, w := range cur.Windows {
		j := manaCachedWindowJ{
			Window: w.Window, LengthSeconds: int64(w.Length / time.Second),
			ReceivedAt: w.ReceivedAt.Unix(), AgeSeconds: ageSeconds(now, w.ReceivedAt),
			Source: w.Source, Status: w.Status, LimitReached: w.LimitReached,
		}
		if !w.ResetsAt.IsZero() {
			r := w.ResetsAt.Unix()
			j.ResetsAt = &r
		}
		if w.HasPercent {
			p, at, age, src := w.Percent, w.PercentReceivedAt.Unix(), ageSeconds(now, w.PercentReceivedAt), w.PercentSource
			j.UsedPercent, j.UsedReceivedAt, j.UsedAgeSeconds, j.UsedSource = &p, &at, &age, &src
		}
		out.Windows = append(out.Windows, j)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(b)
}

func ageSeconds(now, at time.Time) int64 {
	if d := now.Sub(at); d > 0 {
		return int64(d / time.Second)
	}
	return 0
}

func manaCachedText(cur usagelog.Current, name string, now time.Time) string {
	var b strings.Builder
	backend := cur.Backend
	if s, ok := delegator.SpecFor(backend); ok && s.DisplayName != "" {
		backend = s.DisplayName
	}
	if backend == "" {
		backend = "Plan"
	}
	fmt.Fprintf(&b, "📊 *%s usage*: stored figures (no fresh read)\n\n", backend)
	if len(cur.Windows) == 0 {
		b.WriteString("foci holds no usage figures for this account yet. Plain /mana makes a fresh read.\n")
	}
	for _, w := range cur.Windows {
		fmt.Fprintf(&b, "%s: ", windowTitle(w.Window, w.Length))
		if w.HasPercent {
			fmt.Fprintf(&b, "%s%% (received %s, %s)", formatPct(w.Percent), agoPhrase(now, w.PercentReceivedAt), w.PercentSource)
		} else {
			b.WriteString("no percent figure held for the current window")
		}
		if !w.ResetsAt.IsZero() {
			fmt.Fprintf(&b, " · resets %s", formatInstant(w.ResetsAt))
		}
		if !w.HasPercent || !w.ReceivedAt.Equal(w.PercentReceivedAt) {
			fmt.Fprintf(&b, " · last report %s (%s)", agoPhrase(now, w.ReceivedAt), w.Source)
		}
		b.WriteString("\n")
		if w.LimitReached {
			status := ""
			if w.Status != "" {
				status = " (" + w.Status + ")"
			}
			fmt.Fprintf(&b, "  🛑 limit reached%s\n", status)
		}
	}
	if name != "" {
		fmt.Fprintf(&b, "\nAccount: %s\n", name)
	}
	b.WriteString("\nFor a script: /mana cached json (keys: /help mana)")
	return b.String()
}

// windowTitle names a window for a person, its length first: "5h
// (five_hour)", "7d (codex/7d)"; a window of unknown length by name only.
func windowTitle(window string, length time.Duration) string {
	if window == "" {
		window = "(unnamed)"
	}
	if length <= 0 {
		return window
	}
	return lengthLabel(length) + " (" + window + ")"
}

func lengthLabel(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	default:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
}

func formatPct(p float64) string {
	if p == math.Trunc(p) {
		return fmt.Sprintf("%.0f", p)
	}
	return fmt.Sprintf("%.1f", p)
}

// agoPhrase is how long before now at was: "just now", "12m ago", "3h 5m
// ago", "2d 4h ago".
func agoPhrase(now, at time.Time) string {
	d := now.Sub(at)
	if d < time.Minute {
		return "just now"
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh ago", days, hrs)
	case hrs > 0:
		return fmt.Sprintf("%dh %dm ago", hrs, mins)
	default:
		return fmt.Sprintf("%dm ago", mins)
	}
}

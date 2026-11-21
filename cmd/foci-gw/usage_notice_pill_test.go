package main

import (
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/usagelog"
)

// #3249: a usage warning reaches the app with its windows, for the pill.

func pillWindows(spec ...any) []platform.UsageSnapshotWindow {
	var ws []platform.UsageSnapshotWindow
	for i := 0; i < len(spec); i += 2 {
		ws = append(ws, platform.UsageSnapshotWindow{Label: spec[i].(string), UsedPct: spec[i+1].(int)})
	}
	return ws
}

func TestWarnedWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		notice string
		ws     []platform.UsageSnapshotWindow
		want   string
	}{
		{"CC approaching names the 7-day window, not the more severe 5h",
			"⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 50%\nResets: Thu 27 Nov 09:00 (in 5d 1h)",
			pillWindows("5h", 95, "7d", 50), "7d"},
		{"CC rejection names the 5-hour window",
			"🛑 Anthropic rate limit reached — requests on the 5-hour window are being rejected.",
			pillWindows("5h", 100, "7d", 99), "5h"},
		{"CC raw window type for an extra window",
			"⚠️ Approaching Anthropic seven_day_opus rate limit.",
			pillWindows("5h", 95, "7d", 60, "7d opus", 50), "7d opus"},
		{"codex bucket label beats the bare label it contains",
			"⚠️ Codex premium 7d usage at 91%. Resets at Thu 15:04 (in 2d 5h).",
			pillWindows("5h", 10, "7d", 95, "premium 7d", 91), "premium 7d"},
		{"a duration in codex's reset tail is not a window name",
			"⚠️ Codex 7d usage at 50%. Resets at Thu 15:04 (in 5h 3m).",
			pillWindows("5h", 95, "7d", 50), "7d"},
		{"a read's joined notices: the most severe named window",
			"⚠️ Approaching Anthropic 5-hour rate limit.\nUsage: 50%\n\n⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 91%",
			pillWindows("5h", 50, "7d", 91, "7d opus", 99), "7d"},
		{"no window named: the most severe",
			"⚠️ Approaching Anthropic usage rate limit.",
			pillWindows("5h", 40, "7d", 91), "7d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := warnedWindow(tc.notice, tc.ws); got != tc.want {
				t.Errorf("warnedWindow = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsageSnapshotFrom(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cur := usagelog.Current{Windows: []usagelog.CurrentWindow{
		{Window: "five_hour", Length: 5 * time.Hour, HasPercent: true, Percent: 33.6, ResetsAt: now.Add(2 * time.Hour)},
		{Window: "seven_day", Length: 7 * 24 * time.Hour, LimitReached: true, ResetsAt: now.Add(48 * time.Hour)},
		{Window: "seven_day_opus", Length: 7 * 24 * time.Hour, HasPercent: true, Percent: 80, ResetsAt: now.Add(-time.Minute)}, // reset since
		{Window: "overage", HasPercent: true, Percent: 10},                                                                     // not a time window
	}}
	got := usageSnapshotFrom("⚠️ Approaching Anthropic 5-hour rate limit.", cur, now)
	if got == nil || len(got.Windows) != 2 {
		t.Fatalf("snapshot = %+v, want the 5h and 7d windows", got)
	}
	if w := got.Windows[0]; w.Label != "5h" || w.UsedPct != 34 || !w.ResetsAt.Equal(now.Add(2*time.Hour)) || w.Length != 5*time.Hour {
		t.Errorf("5h window = %+v", w)
	}
	if w := got.Windows[1]; w.Label != "7d" || w.UsedPct != 100 || !w.LimitReached {
		t.Errorf("7d window = %+v, want a reached limit at 100%%", w)
	}
	if got.Warned != "5h" {
		t.Errorf("Warned = %q, want 5h", got.Warned)
	}
	if usageSnapshotFrom("x", usagelog.Current{}, now) != nil {
		t.Errorf("no windows should give no snapshot")
	}
}

// pillRLConn is an app-like connection: it draws usage warnings.
type pillRLConn struct {
	*rlConn
	usage []*platform.UsageSnapshot
}

func (c *pillRLConn) SendUsageNotice(text string, u *platform.UsageSnapshot) {
	c.notices = append(c.notices, text)
	c.usage = append(c.usage, u)
}

func TestDeliverRateLimitNoticeCarriesUsageToPillConn(t *testing.T) {
	t.Parallel()
	m := newRLMgr()
	app := &pillRLConn{rlConn: newRLConn("app", m.sessionKey)}
	mgr := &latestRLMgr{rlConnMgr: m, latest: map[string]platform.Connection{m.sessionKey: app}}
	snap := &platform.UsageSnapshot{Windows: pillWindows("5h", 34), Warned: "5h"}

	deliverRateLimitNotice(mgr, "clutch", m.sessionKey, rlNotice, snap, config.RateLimitNotifySession, nil, nil)

	if len(app.usage) != 1 || app.usage[0] != snap || app.notices[0] != rlNotice {
		t.Errorf("pill conn got usage %v notices %v, want the snapshot with the text", app.usage, app.notices)
	}
}

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream" // backend-cap:ignore: the test drives the real Claude Code notices through the gateway delivery
	"foci/internal/timeutil"
	"foci/internal/usagelog"
)

// #3154: the table lists every time window with a current-period figure (or
// a reached limit) whose reset is ahead, shortest first; a row above 90%, or
// at a reached limit, is bold in every cell.
func TestUsageNoticeTable(t *testing.T) {
	now := time.Date(2026, 11, 17, 14, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).In(timeutil.Location()).Format("Mon 2 Jan 15:04") }
	cur := usagelog.Current{Windows: []usagelog.CurrentWindow{
		{Window: "five_hour", Length: 5 * time.Hour, HasPercent: true, Percent: 34.4, ResetsAt: now.Add(2*time.Hour + 5*time.Minute)},
		{Window: "seven_day", Length: 7 * 24 * time.Hour, HasPercent: true, Percent: 93, ResetsAt: now.Add(53 * time.Hour)},
		{Window: "seven_day_opus", Length: 7 * 24 * time.Hour, HasPercent: true, Percent: 90.2, ResetsAt: now.Add(53 * time.Hour)}, // 90%: not above 90
		{Window: "seven_day_sonnet", Length: 7 * 24 * time.Hour},                                                                   // no figure this period
		{Window: "codex/5h", Length: 5 * time.Hour, HasPercent: true, Percent: 99, ResetsAt: now.Add(-time.Minute)},                // reset since
		{Window: "premium/7d", Length: 7 * 24 * time.Hour, LimitReached: true, ResetsAt: now.Add(30 * time.Minute)},                // reached, no figure
		{Window: "premium/limit", LimitReached: true},                                                                              // not a time window
		{Window: "overage", HasPercent: true, Percent: 95},                                                                         // not a time window
	}}
	want := strings.Join([]string{
		"| Window | Percent | Reset at | Reset in |",
		"|---|---|---|---|",
		"| 5h | 34% | " + at(2*time.Hour+5*time.Minute) + " | 2h 5m |",
		"| **7d** | **93%** | **" + at(53*time.Hour) + "** | **2d 5h** |",
		"| 7d opus | 90% | " + at(53*time.Hour) + " | 2d 5h |",
		"| **premium 7d** | **— (limit reached)** | **" + at(30*time.Minute) + "** | **30m** |",
	}, "\n")
	if got := usageNoticeTable(cur, now); got != want {
		t.Fatalf("usageNoticeTable =\n%s\nwant\n%s", got, want)
	}
	if got := usageNoticeTable(usagelog.Current{}, now); got != "" {
		t.Errorf("no windows: table %q, want none", got)
	}
}

// #3154 end to end over a real usage history: a notice gets the table of the
// windows stored for its account; with no history it is sent unchanged.
func TestWithUsageTable(t *testing.T) {
	b := testUsageBinding(t)
	now := time.Now()
	b.RecordRead(&delegator.UsageInfo{
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: 34, ResetsAt: now.Add(2 * time.Hour), Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: 95, ResetsAt: now.Add(72 * time.Hour), Reported: true},
		Raw:                 []byte(`{}`),
	}, delegator.UsageSourceScheduled)
	got := withUsageTable("⚠️ notice", b)
	if !strings.HasPrefix(got, "⚠️ notice\n\n| Window | Percent |") || !strings.Contains(got, "\n| 5h | 34% |") || !strings.Contains(got, "\n| **7d** | **95%** |") {
		t.Fatalf("withUsageTable = %q, want the notice then a table with 5h 34%% and a bold 7d 95%%", got)
	}
	// Q97: with a table, the CC notice's Usage/Resets lines go; the
	// headline and Overage stay.
	cc := "⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 95%\nResets: Fri 20 Nov 21:01 (in 3d 4h)\nOverage: rejected"
	if got := withUsageTable(cc, b); !strings.HasPrefix(got, "⚠️ Approaching Anthropic 7-day rate limit.\nOverage: rejected\n\n| Window |") {
		t.Errorf("CC notice with table = %q, want the headline and Overage line, no Usage/Resets lines, then the table", got)
	}
	usageLine := "⚠️ x\nUsage: 95%"
	if got := withUsageTable(usageLine, nil); got != usageLine {
		t.Errorf("no table: %q, want the notice unchanged (Usage line kept)", got)
	}
	if got := withUsageTable("⚠️ notice", nil); got != "⚠️ notice" {
		t.Errorf("no usage history: %q, want the notice unchanged", got)
	}
	if got := withUsageTable("⚠️ notice", testUsageBinding(t)); got != "⚠️ notice" {
		t.Errorf("nothing stored: %q, want the notice unchanged", got)
	}
}

// #3154: a usage read that crosses a level in both windows reaches each open
// chat as ONE message: both notices, then one table of the account's
// windows as stored by that read.
func TestDeliverUsageReadNotices_OneMessageWithTable(t *testing.T) {
	t.Parallel()
	const agentID = "t3154-gw" // its own process-wide throttle
	m, _ := newRLAllMgr()
	m.session = nil
	open := func(id string) []string {
		if id != agentID {
			return nil
		}
		return []string{"clutch/app-a", "clutch/app-b"}
	}
	b := testUsageBinding(t)
	now := timeutil.Now()
	read := &delegator.UsageInfo{
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: 92, ResetsAt: now.Add(time.Hour), Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: 61, ResetsAt: now.Add(48 * time.Hour), Reported: true},
		Raw:                 []byte(`{}`),
	}
	b.RecordRead(read, delegator.UsageSourceScheduled) // the cache stores a read before its listeners hear it

	deliverUsageReadNotices(m, agentID, ccstream.AnnounceUsageRead, read, delegator.UsageSourceScheduled, config.RateLimitNotifyAll, delegator.DefaultUsageWarningLevels, open, b)
	for k, c := range m.others {
		if len(c.notices) != 1 {
			t.Fatalf("chat %s got %d messages %q, want one", k, len(c.notices), c.notices)
		}
		n := c.notices[0]
		if !strings.Contains(n, "5-hour") || !strings.Contains(n, "7-day") ||
			!strings.Contains(n, "\n| **5h** | **92%** |") || !strings.Contains(n, "\n| 7d | 61% |") ||
			strings.Count(n, "| Window |") != 1 {
			t.Errorf("chat %s got %q, want both notices and one table (5h bold)", k, n)
		}
	}
}

func testUsageBinding(t *testing.T) *usagelog.Binding {
	t.Helper()
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &usagelog.Binding{Store: st, Backend: "claude-code", Account: "acct", AgentID: "clutch"}
}

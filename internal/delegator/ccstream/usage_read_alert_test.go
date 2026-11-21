package ccstream

import (
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// #3144: a scheduled usage read announces each usage warning level (#3145:
// default 50/60/70/80/90/95-99) once per
// window, and shares the per-agent throttle with the stream's
// rate_limit_event notices, so neither source repeats a level the other
// already announced.

var readNow = time.Date(2026, 11, 16, 22, 0, 0, 0, time.UTC)

func readInfo(fivePct, sevenPct int, fiveReset, sevenReset time.Time) *delegator.UsageInfo {
	return &delegator.UsageInfo{
		Backend:             "Claude Code",
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: fivePct, ResetsAt: fiveReset, Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: sevenPct, ResetsAt: sevenReset, Reported: true},
	}
}

func TestUsageReadNotices_Levels(t *testing.T) {
	th := NewRateLimitThrottle()
	five := readNow.Add(3 * time.Hour)
	seven := readNow.Add(4 * 24 * time.Hour)

	if got := th.usageReadNotices(readInfo(10, 49, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("under 50%%: got %q, want none", got)
	}
	// 83% weekly: one notice, for the highest level reached (80), not 50..80.
	got := th.usageReadNotices(readInfo(10, 83, five, seven), delegator.DefaultUsageWarningLevels, readNow)
	if len(got) != 1 || !strings.Contains(got[0], "7-day") || !strings.Contains(got[0], "Usage: 83%") {
		t.Fatalf("83%% weekly: got %q, want one 7-day notice at 83%%", got)
	}
	// Still at level 80 (reset 1 s off, as get_usage reports it): no repeat.
	if got := th.usageReadNotices(readInfo(10, 89, five, seven.Add(-time.Second)), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("89%% weekly after 80 announced: got %q, want none", got)
	}
	// 90 is a new level.
	if got := th.usageReadNotices(readInfo(10, 90, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
		t.Fatalf("90%% weekly: got %q, want one", got)
	}
	// #3145: 95-99 are default levels too; 99% announces 99 once.
	if got := th.usageReadNotices(readInfo(10, 99, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
		t.Fatalf("99%% weekly after 90: got %q, want one", got)
	}
	if got := th.usageReadNotices(readInfo(10, 99, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("99%% weekly again: got %q, want none", got)
	}
	// The 5-hour window is independent: 50% fires there.
	got = th.usageReadNotices(readInfo(50, 99, five, seven), delegator.DefaultUsageWarningLevels, readNow)
	if len(got) != 1 || !strings.Contains(got[0], "5-hour") {
		t.Fatalf("50%% 5-hour: got %q, want one 5-hour notice", got)
	}
	// A new weekly window re-arms its levels.
	if got := th.usageReadNotices(readInfo(50, 60, five, seven.Add(7*24*time.Hour)), delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
		t.Fatalf("60%% in a new weekly window: got %q, want one", got)
	}
}

func TestUsageReadNotices_SkipsUnreportedAndPastWindows(t *testing.T) {
	th := NewRateLimitThrottle()
	info := readInfo(80, 80, readNow.Add(-time.Minute), readNow.Add(time.Hour))
	info.SevenDay.Reported = false
	if got := th.usageReadNotices(info, delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("past 5-hour window and unreported weekly: got %q, want none", got)
	}
	if got := th.usageReadNotices(nil, delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("nil read: got %q", got)
	}
	// A codex read (Allowance) is not this backend's: nothing.
	codex := readInfo(80, 80, readNow.Add(time.Hour), readNow.Add(time.Hour))
	codex.Allowance = &delegator.Allowance{}
	if got := th.usageReadNotices(codex, delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("allowance read: got %q", got)
	}
}

func TestUsageReadNotices_SharedWithStream(t *testing.T) {
	five := readNow.Add(3 * time.Hour)
	seven := readNow.Add(4 * 24 * time.Hour)
	sevenEpoch := float64(seven.Unix())
	warn := func(util float64) *RateLimitEvent {
		u := util
		return &RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "allowed_warning", RateLimitType: "seven_day", ResetsAt: &sevenEpoch, Utilization: &u,
		}}
	}

	t.Run("stream first", func(t *testing.T) {
		var fires []string
		b := &Backend{rlThrottle: NewRateLimitThrottle()}
		b.onRateLimited = func(_, d string) { fires = append(fires, d) }
		b.OnRateLimit(warn(0.80))
		if len(fires) != 1 {
			t.Fatalf("stream 80 fired %d, want 1", len(fires))
		}
		// get_usage's reset is a second off the stream's.
		if got := b.rlThrottle.usageReadNotices(readInfo(0, 84, five, seven.Add(-time.Second)), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
			t.Fatalf("read at 84%% after stream announced 80: got %q, want none", got)
		}
		b.OnRateLimit(warn(0.85))
		if len(fires) != 1 {
			t.Fatalf("stream 85 (still level 80) fired %d, want 1", len(fires))
		}
		if got := b.rlThrottle.usageReadNotices(readInfo(0, 88, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
			t.Fatalf("read at 88%% after stream announced 80: got %q, want none", got)
		}
		if got := b.rlThrottle.usageReadNotices(readInfo(0, 90, five, seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
			t.Fatalf("read at 90%% after stream 85: got %q, want one", got)
		}
	})

	t.Run("read first", func(t *testing.T) {
		var fires []string
		b := &Backend{rlThrottle: NewRateLimitThrottle()}
		b.onRateLimited = func(_, d string) { fires = append(fires, d) }
		if got := b.rlThrottle.usageReadNotices(readInfo(0, 81, five, seven.Add(-time.Second)), delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
			t.Fatalf("read at 81%%: got %q, want one", got)
		}
		b.OnRateLimit(warn(0.80))
		if len(fires) != 0 {
			t.Fatalf("stream 80 after read announced 80 fired %d, want 0", len(fires))
		}
		b.OnRateLimit(warn(0.85))
		if len(fires) != 0 {
			t.Fatalf("stream 85 (level 80) fired %d, want 0", len(fires))
		}
		b.OnRateLimit(warn(0.90))
		if len(fires) != 1 {
			t.Fatalf("stream 90 fired %d, want 1", len(fires))
		}
		// A rejection is never swallowed by an earlier level.
		b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "rejected", RateLimitType: "seven_day", ResetsAt: &sevenEpoch,
		}})
		if len(fires) != 2 {
			t.Fatalf("rejected fired %d total, want 2", len(fires))
		}
	})
}

// TestUsageLevels_NoDuplicateOfCCWarning (#3145, Dick 2026-11-16: "Nb
// mustn't duplicate cc's own warnings 95-99"): CC's own rate_limit_event
// warning at a 9x% level and foci's read at the same level and window are
// ONE message, whichever arrives first; the next 9x% level is again one.
func TestUsageLevels_NoDuplicateOfCCWarning(t *testing.T) {
	five := readNow.Add(3 * time.Hour)
	seven := readNow.Add(4 * 24 * time.Hour)
	sevenEpoch := float64(seven.Unix())
	warn := func(util float64) *RateLimitEvent {
		u := util
		return &RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: "allowed_warning", RateLimitType: "seven_day", ResetsAt: &sevenEpoch, Utilization: &u,
		}}
	}
	var fires []string
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	b.onRateLimited = func(_, d string) { fires = append(fires, d) }
	read := func(pct int) []string {
		// get_usage's reset is a second off the stream's.
		return b.rlThrottle.usageReadNotices(readInfo(0, pct, five, seven.Add(-time.Second)), delegator.DefaultUsageWarningLevels, readNow)
	}

	// CC first at 96%, then foci's read at 96%: one message.
	b.OnRateLimit(warn(0.96))
	got := read(96)
	if total := len(fires) + len(got); total != 1 {
		t.Fatalf("CC 96%% then read 96%%: stream %q + read %q = %d messages, want 1", fires, got, total)
	}
	// foci's read first at 97%, then CC at 97%: one message.
	got = read(97)
	b.OnRateLimit(warn(0.97))
	if len(got) != 1 || len(fires) != 1 {
		t.Fatalf("read 97%% then CC 97%%: read %q, stream %q; want the read's one message and no new stream message", got, fires)
	}
	// Both again at 98%, CC first: one message.
	b.OnRateLimit(warn(0.98))
	if got := read(98); len(got) != 0 || len(fires) != 2 {
		t.Fatalf("CC 98%% then read 98%%: read %q, stream %q; want one message (the stream's)", got, fires)
	}
}

func TestAnnounceUsageRead_PerAgent(t *testing.T) {
	five := readNow.Add(3 * time.Hour)
	seven := readNow.Add(4 * 24 * time.Hour)
	info := readInfo(0, 70, five, seven)
	if got := AnnounceUsageRead("t3144-a", "", info, delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
		t.Fatalf("agent a first: got %q, want one", got)
	}
	if got := AnnounceUsageRead("t3144-a", "", info, delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("agent a again: got %q, want none", got)
	}
	// Another agent on the same account is told in its own chats.
	if got := AnnounceUsageRead("t3144-b", "", info, delegator.DefaultUsageWarningLevels, readNow); len(got) != 1 {
		t.Fatalf("agent b: got %q, want one", got)
	}
}

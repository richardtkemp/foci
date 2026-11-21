package ccstream

import (
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/timeutil"
)

// #3242: a usage warning goes out only when the window's use is ahead of the
// time gone in it (Dick 2026-11-21: "Only send these alerts if percent
// exceeds progress-through-window"). A level not sent for that reason is
// not marked told: a later reading that is ahead still announces it. A
// rejection always fires. Each account of an agent has its own throttle.

func TestOnRateLimit_NotAheadOfWindowIsNotSent(t *testing.T) {
	var fires []string
	b := &Backend{rlThrottle: NewRateLimitThrottle()}
	b.onRateLimited = func(_, d string) { fires = append(fires, d) }
	ev := func(status string, util float64, resetIn time.Duration) *RateLimitEvent {
		u := util
		r := float64(timeutil.Now().Add(resetIn).Unix())
		return &RateLimitEvent{RateLimitInfo: RateLimitInfo{
			Status: status, RateLimitType: "five_hour", Utilization: &u, ResetsAt: &r,
		}}
	}
	// 50% with 54 min left of 5 h: 82% of the window gone.
	b.OnRateLimit(ev("allowed_warning", 0.50, 54*time.Minute))
	if len(fires) != 0 {
		t.Fatalf("50%% at 82%% gone fired %q, want nothing", fires)
	}
	// Level 50 is not marked told, so a later event that is ahead can still
	// announce it.
	if len(b.rlThrottle.state) != 0 || len(b.rlThrottle.windows) != 0 {
		t.Fatalf("an unsent warning marked levels told: state=%v windows=%v", b.rlThrottle.state, b.rlThrottle.windows)
	}
	b.OnRateLimit(ev("allowed_warning", 0.55, 3*time.Hour))
	if len(fires) != 1 {
		t.Fatalf("55%% at 40%% gone fired %d, want 1 (level 50 still unsent)", len(fires))
	}
	// A rejection is never suppressed, however much of the window is gone.
	b.OnRateLimit(ev("rejected", 0.10, 10*time.Minute))
	if len(fires) != 2 {
		t.Fatalf("rejection fired %d in total, want 2", len(fires))
	}
}

func TestUsageReadNotices_NotAheadOfWindowIsNotClaimed(t *testing.T) {
	th := NewRateLimitThrottle()
	seven := readNow.Add(4 * 24 * time.Hour) // 3 of 7 days gone: 43%
	// 50% with 82% of the 5-hour window gone: nothing.
	if got := th.usageReadNotices(readInfo(50, 10, readNow.Add(54*time.Minute), seven), delegator.DefaultUsageWarningLevels, readNow); len(got) != 0 {
		t.Fatalf("50%% at 82%% gone: got %q, want none", got)
	}
	// The same level in the same window, read when ahead: announced.
	if got := th.usageReadNotices(readInfo(52, 10, readNow.Add(54*time.Minute), seven), delegator.DefaultUsageWarningLevels, readNow.Add(-2*time.Hour)); len(got) != 1 {
		t.Fatalf("52%% at 42%% gone after an unsent 50: got %q, want one", got)
	}
}

// Two logins of one agent (#3186) have their own windows: a level told on
// one does not hide the other's, even when their resets are minutes apart
// (the work Mac on 2026-11-21: 12:10 vs 11:59).
func TestAnnounceUsageRead_PerAccount(t *testing.T) {
	const agent = "t3242-acct"
	now := readNow
	seven := now.Add(4 * 24 * time.Hour)
	a := readInfo(97, 17, now.Add(155*time.Minute), seven)
	b := readInfo(60, 17, now.Add(144*time.Minute), seven)
	if got := AnnounceUsageRead(agent, "acct-a", a, delegator.DefaultUsageWarningLevels, now); len(got) != 1 {
		t.Fatalf("account a at 97%%: got %q, want one", got)
	}
	if got := AnnounceUsageRead(agent, "acct-b", b, delegator.DefaultUsageWarningLevels, now); len(got) != 1 {
		t.Fatalf("account b at 60%% after a's 97%%: got %q, want one (its own window)", got)
	}
}

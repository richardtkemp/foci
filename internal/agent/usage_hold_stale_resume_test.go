package agent

import (
	"strings"
	"testing"
	"time"

	"foci/internal/usagehold"
)

// TestUsageHold_StaleResumeAsks is #3155: at lift, a chat session the hold
// interrupted gets the hidden resume line only when it has been idle no longer
// than usage_hold.resume_max_idle. Idle longer, it gets the hidden ask line
// instead (Dick 2026-11-17: "Instead agent should ask what to do"). A user
// message held during the hold is activity; 0 means always resume.
func TestUsageHold_StaleResumeAsks(t *testing.T) {
	cases := []struct {
		name    string
		maxIdle time.Duration
		// lateMsg: a user message arrives just before the lift.
		lateMsg bool
		wantAsk bool
	}{
		{name: "idle past the limit asks", maxIdle: 50 * time.Millisecond, wantAsk: true},
		{name: "idle within the limit resumes", maxIdle: time.Hour},
		{name: "a recent held message is activity", maxIdle: 50 * time.Millisecond, lateMsg: true},
		{name: "zero means always resume", maxIdle: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, cancel := startedAgent(t)
			defer cancel()
			reg := usagehold.New()
			reg.SetResumeMaxIdle(tc.maxIdle)
			a.UsageHoldRegistry = reg
			a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
			a.StartUsageHold()
			defer a.StopUsageHold()
			d := newBlockingDriver()
			a.SetTurnObserver(d.record)
			a.UsageHeldDriver = func(string) Driver { return d }

			const sk = "test/c1"
			a.Enqueue(Envelope{SessionKey: sk, Text: "M0", Driver: d})
			select {
			case <-d.started:
			case <-time.After(2 * time.Second):
				t.Fatal("the first turn never started")
			}
			a.ObserveUsage(weekly(0.98))
			select {
			case <-d.ended:
			case <-time.After(2 * time.Second):
				t.Fatal("the turn in flight was not interrupted by the hold")
			}
			// Let the resume line park, then idle past the short limit.
			if !waitFor(2*time.Second, func() bool { return a.UsageHeldInput(sk) }) {
				t.Fatal("no resume line parked")
			}
			time.Sleep(200 * time.Millisecond)
			want := 1
			if tc.lateMsg {
				a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d, ReceivedAt: time.Now()})
				want = 2
				if !waitFor(2*time.Second, func() bool { return heldCount(a, sk) == 2 }) {
					t.Fatal("M1 was not held")
				}
			}
			a.ObserveUsage(weekly(0.10))
			if !waitFor(2*time.Second, func() bool { return len(d.Calls()) == 2 }) {
				t.Fatalf("turns = %d, want the interrupted one and one at lift", len(d.Calls()))
			}
			got := d.Calls()[1]
			if len(got) != want || !got[0].usageResume {
				t.Fatalf("lift turn = %+v, want %d input(s) headed by a hidden line", got, want)
			}
			ask := strings.HasPrefix(got[0].Text, usageResumeAskPrefix)
			if ask != tc.wantAsk {
				t.Errorf("lift line = %q, want ask=%v", got[0].Text, tc.wantAsk)
			}
			if !tc.wantAsk && got[0].Text != UsageResumeLine {
				t.Errorf("lift line = %q, want the resume line", got[0].Text)
			}
		})
	}
}

// heldCount is how many inputs the usage hold holds on sk now.
func heldCount(a *Agent, sk string) int {
	a.usageHold.mirrorMu.Lock()
	defer a.usageHold.mirrorMu.Unlock()
	return a.usageHold.heldBySession[sk]
}

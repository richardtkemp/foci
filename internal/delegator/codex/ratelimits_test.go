package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/timeutil"
)

// Payloads captured from codex 0.164.0 on 2026-10-29 (accountId removed).
const (
	liveRateLimitsRead = `{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1793239774},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1793786823},"credits":{"hasCredits":false,"unlimited":false,"balance":null},"individualLimit":null,"spendControlReached":false,"planType":"team","rateLimitReachedType":"workspace_member_credits_depleted"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1793239774},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1793786823},"credits":{"hasCredits":false,"unlimited":false,"balance":null},"individualLimit":null,"spendControlReached":false,"planType":"team","rateLimitReachedType":"workspace_member_credits_depleted"}},"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`
	livePremiumDepleted = `{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"premium","limitName":null,"normalModelSlug":null,"primary":null,"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":null},"individualLimit":null,"spendControlReached":null,"planType":null,"rateLimitReachedType":"workspace_member_credits_depleted"}}}`

	reset5h = int64(1793239774) // 2026-10-29 02:09:34 UTC
	reset7d = int64(1793786823)
)

var rlNow = time.Unix(1793233800, 0) // 2026-10-29 00:30 UTC, 1h39m before reset5h

// codexWindow builds a "codex"-bucket snapshot with the 5h window at pct.
func codexWindow(pct float64, resetsAt int64) rateLimitSnapshot {
	mins := int64(300)
	return rateLimitSnapshot{LimitID: "codex", Primary: &rateLimitWindow{UsedPercent: pct, WindowDurationMins: &mins, ResetsAt: &resetsAt}}
}

func newRLState() *rateLimitState {
	return &rateLimitState{snaps: map[string]rateLimitSnapshot{}, marks: map[string]rlMark{}}
}

func hhmm(unix int64) string { return time.Unix(unix, 0).In(timeutil.Location()).Format("15:04") }

// TestRateLimitsFirstObservationNeverWarns proves the restart rule: the first
// snapshot of each window and of the limit-reached state only seeds, even when
// it shows a window used up and the workspace out of credits — a foci restart
// must not re-announce what the user was already told.
func TestRateLimitsFirstObservationNeverWarns(t *testing.T) {
	s := newRLState()
	if n := s.apply(codexWindow(100, reset5h), rlNow); len(n) != 0 {
		t.Fatalf("first codex snapshot warned: %q", n)
	}
	reached := "workspace_member_credits_depleted"
	if n := s.apply(rateLimitSnapshot{LimitID: "premium", RateLimitReachedType: &reached}, rlNow); len(n) != 0 {
		t.Fatalf("first premium snapshot warned: %q", n)
	}
}

// TestRateLimitsWarnOncePerThreshold proves a window warns once as it crosses
// 80%, once at 95% and once at 100% — not on every report in between, and
// not again when the reported reset time wobbles by a second (seen live).
func TestRateLimitsWarnOncePerThreshold(t *testing.T) {
	s := newRLState()
	s.apply(codexWindow(70, reset5h), rlNow)
	steps := []struct {
		pct    float64
		reset  int64
		wantIn string // "" = no notice
	}{
		{79, reset5h, ""},
		{82, reset5h, "⚠️ Codex 5h usage at 82%. Resets at " + hhmm(reset5h) + " (in 1h 39m)."},
		{85, reset5h, ""},
		{85, reset5h + 1, ""},
		{96, reset5h, "⚠️ Codex 5h usage at 96%."},
		{99, reset5h, ""},
		{100, reset5h, "🛑 Codex 5h usage used up (100%)."},
		{100, reset5h, ""},
	}
	for i, st := range steps {
		got := s.apply(codexWindow(st.pct, st.reset), rlNow)
		if st.wantIn == "" {
			if len(got) != 0 {
				t.Fatalf("step %d (%v%%): unexpected notice %q", i, st.pct, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0], st.wantIn) {
			t.Fatalf("step %d (%v%%): notices %q, want one containing %q", i, st.pct, got, st.wantIn)
		}
	}
}

// TestRateLimitsNewWindowRearms proves a new window (resetsAt moves on by
// hours) starts from no level: a low reading is silent, and crossing 80% in
// the new window warns again.
func TestRateLimitsNewWindowRearms(t *testing.T) {
	s := newRLState()
	s.apply(codexWindow(70, reset5h), rlNow)
	s.apply(codexWindow(100, reset5h), rlNow)
	next := reset5h + 5*3600
	if n := s.apply(codexWindow(3, next), rlNow); len(n) != 0 {
		t.Fatalf("new window at 3%% warned: %q", n)
	}
	if n := s.apply(codexWindow(81, next), rlNow); len(n) != 1 {
		t.Fatalf("crossing 80%% in the new window: notices %q, want one", n)
	}
}

// TestRateLimitsReachedWarnsOnTransition proves rateLimitReachedType warns
// when it appears, stays quiet while it persists, and warns again only after
// it cleared and came back. It names when included usage resets.
func TestRateLimitsReachedWarnsOnTransition(t *testing.T) {
	s := newRLState()
	s.apply(codexWindow(100, reset5h), rlNow)
	s.apply(rateLimitSnapshot{LimitID: "premium"}, rlNow) // seed: not reached
	depleted := "workspace_member_credits_depleted"
	got := s.apply(rateLimitSnapshot{LimitID: "premium", RateLimitReachedType: &depleted}, rlNow)
	want := "🛑 Codex limit reached: workspace out of credits. Turns will fail until it is lifted. Included usage resets at " + hhmm(reset5h) + " (in 1h 39m)."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("notices %q, want [%q]", got, want)
	}
	if n := s.apply(rateLimitSnapshot{LimitID: "premium", RateLimitReachedType: &depleted}, rlNow); len(n) != 0 {
		t.Fatalf("repeat warned: %q", n)
	}
	s.apply(rateLimitSnapshot{LimitID: "premium"}, rlNow)
	if n := s.apply(rateLimitSnapshot{LimitID: "premium", RateLimitReachedType: &depleted}, rlNow); len(n) != 1 {
		t.Fatalf("after clearing, return of the limit: notices %q, want one", n)
	}
}

// TestRateLimitsStatusLineFromLiveRead proves the status block renders the
// live account/rateLimits/read payload: each window's used % and reset time
// in foci's timezone, and the limit-reached state. Credits are omitted: the
// account reports no balance.
func TestRateLimitsStatusLineFromLiveRead(t *testing.T) {
	b := newStartableBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "account/rateLimits/read" {
			t.Fatalf("method = %q, want account/rateLimits/read", method)
		}
		return json.RawMessage(liveRateLimitsRead), nil
	})
	b.startOpts.AgentID = t.Name()
	var notices []string
	b.SetHostHooks(delegator.HostHooks{OnRateLimitNotice: func(_, n string) { notices = append(notices, n) }})

	b.seedRateLimits()

	got := b.rateLimits().statusLine(rlNow)
	want := "usage: 5h 100% (resets " + hhmm(reset5h) + "), 7d 34% (resets " +
		time.Unix(reset7d, 0).In(timeutil.Location()).Format("Mon 2 Jan 15:04") +
		") | limit reached: workspace out of credits"
	if got != want {
		t.Fatalf("statusLine =\n  %q\nwant\n  %q", got, want)
	}
	if len(notices) != 0 {
		t.Fatalf("seeding warned: %q", notices)
	}
	if sd := b.StatusDetail(); !strings.Contains(sd, " | usage: 5h 100%") {
		t.Fatalf("StatusDetail() = %q, want the usage line", sd)
	}
}

// TestRateLimitsUpdatedNotificationReachesHook proves the live
// account/rateLimits/updated notification, dispatched like any codex line,
// reaches the gateway's rate-limit notice hook with the session key once the
// limit is newly reached, and that the status block shows it.
func TestRateLimitsUpdatedNotificationReachesHook(t *testing.T) {
	b := newTestBackend(t)
	b.startOpts.AgentID = t.Name()
	type call struct{ key, notice string }
	var calls []call
	b.SetHostHooks(delegator.HostHooks{OnRateLimitNotice: func(k, n string) { calls = append(calls, call{k, n}) }})
	b.rateLimits().apply(rateLimitSnapshot{LimitID: "premium"}, rlNow) // seen before: not reached

	b.dispatch([]byte(livePremiumDepleted))

	if len(calls) != 1 || calls[0].key != "test/session" || !strings.Contains(calls[0].notice, "workspace out of credits") {
		t.Fatalf("hook calls = %+v, want one for test/session naming the credits limit", calls)
	}
	if sd := b.StatusDetail(); !strings.Contains(sd, "limit reached: workspace out of credits") {
		t.Fatalf("StatusDetail() = %q, want the limit", sd)
	}
}

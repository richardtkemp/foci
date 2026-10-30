package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/timeutil"
)

// Payloads captured from codex 0.164.0 on 2026-10-29 (accountId removed).
const (
	liveRateLimitsRead  = `{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1793239774},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1793786823},"credits":{"hasCredits":false,"unlimited":false,"balance":null},"individualLimit":null,"spendControlReached":false,"planType":"team","rateLimitReachedType":"workspace_member_credits_depleted"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":1793239774},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1793786823},"credits":{"hasCredits":false,"unlimited":false,"balance":null},"individualLimit":null,"spendControlReached":false,"planType":"team","rateLimitReachedType":"workspace_member_credits_depleted"}},"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`
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

// TestRateLimitsGateEngagesOnReachedLimit proves a live limit-reached update
// closes the agent's rate-limit gate (pausing background work) until the
// soonest reset of a window at 100%, and that repeats of the same state do
// not re-close it while it is still shut.
func TestRateLimitsGateEngagesOnReachedLimit(t *testing.T) {
	b := newTestBackend(t)
	b.startOpts.AgentID = t.Name()
	var sigs []ratelimit.Signal
	b.SetHostHooks(delegator.HostHooks{EngageRateLimit: func(s ratelimit.Signal) { sigs = append(sigs, s) }})
	// The handler reads the real clock, so the window resets relative to it.
	reset := time.Now().Add(2 * time.Hour).Unix()
	b.rateLimits().apply(codexWindow(100, reset), time.Now())

	b.dispatch([]byte(livePremiumDepleted))
	b.dispatch([]byte(livePremiumDepleted))

	if len(sigs) != 1 {
		t.Fatalf("EngageRateLimit calls = %d, want 1", len(sigs))
	}
	if sigs[0].Kind != ratelimit.KindUsage || !sigs[0].ResetAt.Equal(time.Unix(reset, 0)) {
		t.Fatalf("signal = %+v, want usage limit resetting at the 5h window's reset", sigs[0])
	}
}

// TestRateLimitsGateIgnoresFullWindowWithoutLimit proves a window at 100%
// alone does not pause work: codex keeps running on credits past included
// usage, and only rateLimitReachedType means turns fail.
func TestRateLimitsGateIgnoresFullWindowWithoutLimit(t *testing.T) {
	s := newRLState()
	s.apply(codexWindow(100, reset5h), rlNow)
	if sig := s.gateSignal(rlNow); sig != nil {
		t.Fatalf("gateSignal = %+v, want nil without a reached limit", sig)
	}
}

// TestRateLimitsGateNoResetUsesFallbackAndReprobes proves an out-of-credits
// limit with no window at 100% carries no reset hint (so the shared policy's
// one-hour usage fallback applies), is not re-engaged while that hour runs,
// and is re-engaged by the next report after it — one probe per period, not
// a pause forever.
func TestRateLimitsGateNoResetUsesFallbackAndReprobes(t *testing.T) {
	s := newRLState()
	depleted := "workspace_member_credits_depleted"
	s.apply(codexWindow(40, reset5h), rlNow)
	s.apply(rateLimitSnapshot{LimitID: "premium", RateLimitReachedType: &depleted}, rlNow)

	sig := s.gateSignal(rlNow)
	if sig == nil || !sig.ResetAt.IsZero() {
		t.Fatalf("gateSignal = %+v, want a signal with no reset hint", sig)
	}
	if got, want := ratelimit.Resolve(rlNow, *sig, 0).Until, rlNow.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("gate until %v, want %v (one-hour usage fallback)", got, want)
	}
	if again := s.gateSignal(rlNow.Add(59 * time.Minute)); again != nil {
		t.Fatalf("re-engaged while the gate is shut: %+v", again)
	}
	if again := s.gateSignal(rlNow.Add(61 * time.Minute)); again == nil {
		t.Fatal("not re-engaged after the gate reopened with the limit still reported")
	}
	// Credits added: codex clears rateLimitReachedType; nothing re-closes.
	s.apply(rateLimitSnapshot{LimitID: "premium"}, rlNow)
	if again := s.gateSignal(rlNow.Add(3 * time.Hour)); again != nil {
		t.Fatalf("engaged after the limit cleared: %+v", again)
	}
}

// TestRateLimitsSeedDoesNotEngageGate proves the startup read never closes
// the gate: its deadline survives a restart by itself, and re-closing would
// re-broadcast the rate-limited notice on every restart.
func TestRateLimitsSeedDoesNotEngageGate(t *testing.T) {
	b := newStartableBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return json.RawMessage(liveRateLimitsRead), nil
	})
	b.startOpts.AgentID = t.Name()
	engaged := 0
	b.SetHostHooks(delegator.HostHooks{EngageRateLimit: func(ratelimit.Signal) { engaged++ }})
	b.seedRateLimits()
	if engaged != 0 {
		t.Fatalf("seed engaged the gate %d times, want 0", engaged)
	}
}

// poolAs registers b as agent's running app-server for the test.
func poolAs(t *testing.T, agent string, b *Backend) {
	t.Helper()
	b.startOpts.AgentID = agent
	sharedPool.Lock()
	sharedPool.servers[agent] = b
	sharedPool.Unlock()
	t.Cleanup(func() {
		sharedPool.Lock()
		delete(sharedPool.servers, agent)
		sharedPool.Unlock()
	})
}

// TestQueryUsage_FreshReadFromRunningServer pins /usage on a codex agent
// (#2658): with the agent's app-server running, QueryUsage reads the
// allowance fresh (account/rateLimits/read) and reports it as Codex — each
// window's used % and reset, the plan type and the limit-reached state.
func TestQueryUsage_FreshReadFromRunningServer(t *testing.T) {
	reads := 0
	b := newStartableBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method != "account/rateLimits/read" {
			t.Errorf("method = %q, want account/rateLimits/read", method)
		}
		reads++
		return json.RawMessage(liveRateLimitsRead), nil
	})
	poolAs(t, t.Name(), b)

	info, err := QueryUsage(context.Background(), t.Name())
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if reads != 1 {
		t.Fatalf("account/rateLimits/read sent %d times, want 1", reads)
	}
	if info.Backend != "Codex" || info.Allowance == nil {
		t.Fatalf("info = %+v, want a Codex allowance", info)
	}
	a := info.Allowance
	if !a.Fresh || a.Note != "" || a.AsOf.IsZero() {
		t.Errorf("Fresh=%v Note=%q AsOf=%v, want a fresh read", a.Fresh, a.Note, a.AsOf)
	}
	if a.PlanType != "team" {
		t.Errorf("PlanType = %q, want team", a.PlanType)
	}
	if len(a.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want only codex", a.Buckets)
	}
	bk := a.Buckets[0]
	want := []delegator.AllowanceWindow{
		{Label: "5h", Percent: 100, ResetsAt: time.Unix(reset5h, 0)},
		{Label: "7d", Percent: 34, ResetsAt: time.Unix(reset7d, 0)},
	}
	if bk.Name != "" || len(bk.Windows) != 2 || bk.Windows[0] != want[0] || bk.Windows[1] != want[1] {
		t.Errorf("codex bucket = %+v, want windows %+v", bk, want)
	}
	if bk.LimitReached != "workspace out of credits" {
		t.Errorf("LimitReached = %q", bk.LimitReached)
	}
	// hasCredits:false with no balance is not reported as a credit state.
	if bk.Credits != "" {
		t.Errorf("Credits = %q, want none (codex sent no balance)", bk.Credits)
	}
}

// TestQueryUsage_NoServerReportsLastPushed pins the stale path: with no
// running app-server QueryUsage starts nothing, returns the numbers codex
// last pushed with the time they arrived, and says why they are not fresh.
// Each agent sees only its own account's numbers.
func TestQueryUsage_NoServerReportsLastPushed(t *testing.T) {
	agent, other := t.Name(), t.Name()+"-other"
	before := time.Now()
	rateLimitsFor(agent).apply(codexWindow(42, reset5h), timeutil.Now())
	rateLimitsFor(other).apply(codexWindow(99, reset5h), timeutil.Now())

	info, err := QueryUsage(context.Background(), agent)
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	a := info.Allowance
	if a.Fresh || !strings.Contains(a.Note, "no codex process is running") {
		t.Errorf("Fresh=%v Note=%q, want a stale reply that says why", a.Fresh, a.Note)
	}
	if a.AsOf.Before(before.Add(-time.Second)) {
		t.Errorf("AsOf = %v, want the time the numbers arrived", a.AsOf)
	}
	if len(a.Buckets) != 1 || len(a.Buckets[0].Windows) != 1 || a.Buckets[0].Windows[0].Percent != 42 {
		t.Errorf("buckets = %+v, want this agent's 5h at 42%%", a.Buckets)
	}

	empty, err := QueryUsage(context.Background(), t.Name()+"-never")
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(empty.Allowance.Buckets) != 0 || !empty.Allowance.AsOf.IsZero() {
		t.Errorf("agent with no reports: %+v, want no buckets and no AsOf", empty.Allowance)
	}
}

// TestQueryUsage_ReadFailureFallsBack pins that a failed fresh read is not an
// error for /usage: it reports the last pushed numbers and the failure.
func TestQueryUsage_ReadFailureFallsBack(t *testing.T) {
	b := newStartableBackend(t, func(string, json.RawMessage, int64) (json.RawMessage, error) {
		return nil, rpcErrorReply{code: -32600, message: "nope"}
	})
	poolAs(t, t.Name(), b)
	rateLimitsFor(t.Name()).apply(codexWindow(10, reset5h), timeutil.Now())

	info, err := QueryUsage(context.Background(), t.Name())
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	a := info.Allowance
	if a.Fresh || !strings.Contains(a.Note, "the read failed") {
		t.Errorf("Fresh=%v Note=%q, want a stale reply naming the failed read", a.Fresh, a.Note)
	}
	if len(a.Buckets) != 1 || a.Buckets[0].Windows[0].Percent != 10 {
		t.Errorf("buckets = %+v, want the last pushed 10%%", a.Buckets)
	}
}

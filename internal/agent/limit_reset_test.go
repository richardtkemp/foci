package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// usageHoldIdxStore is the agent_metadata store the gateway gives the
// registry (cmd/foci-gw usageHoldStore), rebuilt here over a test index.
type usageHoldIdxStore struct{ idx *session.SessionIndex }

func (s usageHoldIdxStore) LoadUsageHold(acct string) (string, bool) {
	v, err := s.idx.GetAgentMetadata("_usage_hold", "usage_hold:"+acct)
	return v, err == nil && v != ""
}

func (s usageHoldIdxStore) SaveUsageHold(acct, v string) {
	if v == "" {
		_ = s.idx.DeleteAgentMetadata("_usage_hold", "usage_hold:"+acct)
		return
	}
	_ = s.idx.SetAgentMetadata("_usage_hold", "usage_hold:"+acct, v)
}

// TestResetLimits_ClearsGateAndHold_SurvivesRestart is the 2026-11-11 work-Mac
// incident (#3043): the five-hour window reached (usage STOP) and CC's
// rejection closed the agent's gate. Both were persisted, and in a stop
// nothing runs that could clear them. ResetLimits lifts both, and a "restart"
// (a new registry and agent over the same state.db) comes up open.
func TestResetLimits_ClearsGateAndHold_SurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	boot := func() *Agent {
		reg := usagehold.New()
		reg.SetStore(usageHoldIdxStore{idx})
		a := holdAgent(reg, "claude-code")
		a.Endpoint = "" // a delegated agent's gate row is ratelimit_until: (empty endpoint)
		a.SessionIndex = idx
		return a
	}

	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	a := boot()
	a.ObserveUsage(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: reset})
	a.EngageRateLimit(ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: reset})

	// Restart: both come back closed (the incident's state).
	a = boot()
	if a.UsageHold().Phase != usagehold.Stop {
		t.Fatalf("after restart: hold phase %v, want stop", a.UsageHold().Phase)
	}
	if lim, _ := a.KeepaliveRateLimited("claude-code-agent/c1"); !lim {
		t.Fatal("after restart: keepalive not blocked")
	}

	res := a.ResetLimits("test")
	if !res.Cleared() || res.HoldFrom != usagehold.Stop || !res.GateUntil.Equal(reset) {
		t.Fatalf("ResetLimits = %+v, want a stop and a gate until %v cleared", res, reset)
	}
	if lim, reason := a.SessionRateLimited("claude-code-agent/c1"); lim {
		t.Fatalf("after ResetLimits: still blocked (%s)", reason)
	}

	// Restart again: the reset survives.
	a = boot()
	if a.UsageHold().Phase != usagehold.Open {
		t.Errorf("after reset + restart: hold phase %v, want open", a.UsageHold().Phase)
	}
	if lim, reason := a.SessionRateLimited("claude-code-agent/c1"); lim {
		t.Errorf("after reset + restart: still blocked (%s)", reason)
	}

	// Nothing blocked: a second reset clears nothing.
	if res := a.ResetLimits("test"); res.Cleared() {
		t.Errorf("ResetLimits on an open agent = %+v, want nothing cleared", res)
	}
}

// TestResetLimits_ReleasesHeldInput: input parked by a stop runs at the
// forced lift, as at a natural one.
func TestResetLimits_ReleasesHeldInput(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	a.ObserveUsage(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: time.Now().Add(2 * time.Hour)})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "M1", Driver: d})
	if waitFor(300*time.Millisecond, func() bool { return d.NumCalls() > 0 }) {
		t.Fatalf("a message ran during the stop; calls=%d", d.NumCalls())
	}
	a.ResetLimits("test")
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("held message did not run after ResetLimits; calls=%d", d.NumCalls())
	}
}

// TestUsageAvailable_UsesHoldThreshold: the /usage lift compares the read to
// the live hold threshold.
func TestUsageAvailable_UsesHoldThreshold(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	info := &delegator.UsageInfo{
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: 96, Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: 10, Reported: true},
	}
	if !a.UsageAvailable(info) {
		t.Error("96% at the default 98% threshold: not available, want available")
	}
	reg.SetThresholdPercent(95)
	if a.UsageAvailable(info) {
		t.Error("96% at a 95% threshold: available, want not")
	}
}

package agent

import (
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagehold"
)

func rateLimitRetry(delay time.Duration) delegator.APIRetry {
	return delegator.APIRetry{Endpoint: "Claude Code", Attempt: 1, MaxRetries: 15, Delay: delay, Status: 429, Category: "rate_limit"}
}

// TestInRateLimitRetry_Marks: a rate-limit retry marks the session asleep
// until its delay ends; output, another kind of retry, or the turn's end
// clears the mark.
func TestInRateLimitRetry_Marks(t *testing.T) {
	a := &Agent{}
	const sk = "cam/c1"
	a.noteBackendRetry(sk, rateLimitRetry(2*time.Hour))
	if !a.InRateLimitRetry(sk) {
		t.Fatal("a 429 retry with a 2h delay: not asleep")
	}
	a.noteBackendProgress(sk)
	if a.InRateLimitRetry(sk) {
		t.Error("output after the retry: still asleep")
	}

	a.noteBackendRetry(sk, delegator.APIRetry{Category: "rate_limit", Delay: time.Hour}) // no status: category alone counts
	if !a.InRateLimitRetry(sk) {
		t.Error("a rate_limit retry with no status: not asleep")
	}
	a.noteBackendRetry(sk, delegator.APIRetry{Status: 529, Category: "overloaded", Delay: time.Hour})
	if a.InRateLimitRetry(sk) {
		t.Error("an overloaded retry after a rate-limit one: still asleep (ruling covers rate-limit retries only)")
	}

	a.noteBackendRetry(sk, rateLimitRetry(-time.Second))
	if a.InRateLimitRetry(sk) {
		t.Error("a retry whose delay has passed: still asleep")
	}

	done := a.markInFlight(sk, false)
	a.noteBackendRetry(sk, rateLimitRetry(time.Hour))
	done()
	if a.InRateLimitRetry(sk) {
		t.Error("the turn ended: still asleep")
	}
}

// TestResetLimits_WakesRateLimitRetries is #3044 requirements 1-3: a reset
// from agent A interrupts agent B's chat turn that is asleep in a rate-limit
// retry (B is on the same Claude Code account), and that session then runs
// the hidden resume line; B's other running turn, not in a retry, is left
// alone.
func TestResetLimits_WakesRateLimitRetries(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	a.StartUsageHold()
	defer a.StopUsageHold()

	b, cancel := startedAgent(t)
	defer cancel()
	b.UsageHoldRegistry = reg
	b.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	b.StartUsageHold()
	defer b.StopUsageHold()
	sleeping, busy := newBlockingDriver(), newBlockingDriver()
	b.SetTurnObserver(func(sk string, batch []Envelope) {
		if sk == "test/sleeping" {
			sleeping.record(sk, batch)
		} else {
			busy.record(sk, batch)
		}
	})
	b.UsageHeldDriver = func(sk string) Driver {
		if sk == "test/sleeping" {
			return sleeping
		}
		return busy
	}

	b.Enqueue(Envelope{SessionKey: "test/sleeping", Text: "M0", Driver: sleeping})
	b.Enqueue(Envelope{SessionKey: "test/busy", Text: "B0", Driver: busy})
	for _, d := range []*blockingDriver{sleeping, busy} {
		select {
		case <-d.started:
		case <-time.After(2 * time.Second):
			t.Fatal("a first turn never started")
		}
	}
	b.noteBackendRetry("test/sleeping", rateLimitRetry(2*time.Hour))

	res := a.ResetLimits("test")
	if res.Woken != 0 {
		t.Errorf("A woke %d of its own turns, want 0 (B's turn is B's)", res.Woken)
	}
	select {
	case <-sleeping.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("B's turn asleep in a rate-limit retry was not interrupted")
	}
	if !waitFor(2*time.Second, func() bool { return len(sleeping.Calls()) == 2 }) {
		t.Fatalf("turns on the woken session = %d, want the first and one resume", len(sleeping.Calls()))
	}
	if got := sleeping.Calls()[1]; len(got) != 1 || !got[0].usageResume || got[0].Text != UsageResumeLine {
		t.Errorf("resume turn = %+v, want the hidden resume line alone", got)
	}
	select {
	case <-busy.ended:
		t.Error("B's turn not in a retry was interrupted")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestResetLimits_ReportsOwnWoken: the reset's own report counts the calling
// agent's woken turns, and a reset that only wakes turns is not "nothing".
func TestResetLimits_ReportsOwnWoken(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := newBlockingDriver()
	a.SetTurnObserver(d.record)
	a.UsageHeldDriver = func(string) Driver { return d }

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "M0", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first turn never started")
	}
	a.noteBackendRetry("test/s", rateLimitRetry(time.Hour))
	res := a.ResetLimits("test")
	if res.Woken != 1 || !res.Cleared() {
		t.Fatalf("ResetLimits = %+v, want one woken turn", res)
	}
	if s := res.Summary(); s != "Resumed 1 turn(s) that were waiting in a rate-limit retry." {
		t.Errorf("Summary = %q", s)
	}
	if !waitFor(2*time.Second, func() bool { return len(d.Calls()) == 2 }) {
		t.Fatalf("turns = %d, want the first and one resume", len(d.Calls()))
	}
}

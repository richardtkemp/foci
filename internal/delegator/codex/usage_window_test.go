package codex

import (
	"testing"

	"foci/internal/delegator"
	"foci/internal/usagehold"
)

// TestRateLimitsUpdatedReportsUsageWindows is #2491 for codex: a live update
// reports each window's use to the usage hold, and the reached signal as a
// window of its own (requirement 10, codex part).
func TestRateLimitsUpdatedReportsUsageWindows(t *testing.T) {
	b := newTestBackend(t)
	b.startOpts.AgentID = t.Name()
	isolateRateLimits(t, t.Name())
	var evs []usagehold.Event
	b.SetHostHooks(delegator.HostHooks{OnUsageWindow: func(ev usagehold.Event) { evs = append(evs, ev) }})

	b.dispatch([]byte(`{"method":"account/rateLimits/updated","params":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":98,"windowDurationMins":300,"resetsAt":1893456000},"secondary":null,"rateLimitReachedType":null}}}`))
	var primary, reached *usagehold.Event
	for i := range evs {
		switch evs[i].Window {
		case "codex codex primary":
			primary = &evs[i]
		case "codex codex reached":
			reached = &evs[i]
		}
	}
	if primary == nil || !primary.HasUtil || primary.Util != 0.98 || primary.ResetAt.Unix() != 1893456000 {
		t.Fatalf("events = %+v, want the primary window at 0.98", evs)
	}
	if reached == nil || reached.Reached {
		t.Fatalf("events = %+v, want a not-reached signal", evs)
	}

	evs = nil
	b.dispatch([]byte(livePremiumDepleted))
	got := false
	for _, ev := range evs {
		if ev.Window == "codex premium reached" && ev.Reached {
			got = true
		}
	}
	if !got {
		t.Errorf("events = %+v, want the premium bucket reported reached", evs)
	}
}

//go:build integration && race

package integration

// #2909: this test exists only in a -race build. A gateway under test whose
// detector is off, or a harness that stops capturing its stderr, turns it red,
// so a green nightly -race run cannot have tested a plain gateway.

import (
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

func TestL2_RaceCanary(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:     []testharness.AgentSpec{{ID: "alpha", UserID: 29091}},
		RaceCanary: true,
	})
	deadline := time.Now().Add(30 * time.Second)
	for len(h.GatewayRaces()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a -race foci-gw committed a data race and no report reached its stderr:\n%s", h.Stderr())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if report := h.GatewayRaces()[0]; !strings.Contains(report, "race_canary.go") {
		t.Errorf("the report does not name the canary in foci-gw:\n%s", report)
	}
}

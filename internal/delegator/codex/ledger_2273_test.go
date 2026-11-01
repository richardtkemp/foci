package codex

import (
	"bytes"
	"strings"
	"testing"

	"foci/internal/delegator/accounting"
)

// TestCodexUnseenUsageKeyAlarmsOnce is invUnseenField for codex (#2273): a
// tokenUsage breakdown key codex adds alarms once, naming the key path and its
// value, and the cycle is still booked; the next cycle carrying it does not
// alarm again. (The fixture's own keys are silent: TestCodexCyclesAreCallsOnTheTurn
// allows no alarm but the child's unpriced cache write.)
func TestCodexUnseenUsageKeyAlarmsOnce(t *testing.T) {
	b, _, alarms, path := ledgerFacade(t)
	lines := fixtureLines(t)
	for _, line := range lines[:2] {
		// The fixture is pretty-printed; "last" comes before "total".
		edited := bytes.Replace(line, []byte(`"reasoningOutputTokens": 0,`),
			[]byte(`"reasoningOutputTokens": 0, "audioInputTokens": 42,`), 1)
		if bytes.Equal(edited, line) {
			t.Fatal("premise: fixture line lacks the key to edit")
		}
		b.dispatch(edited)
	}
	var got []accounting.Alarm
	for _, a := range *alarms {
		if a.Invariant == accounting.InvUnseenField {
			got = append(got, a)
		}
	}
	if len(got) != 1 {
		t.Fatalf("invUnseenField raised %d times, want once: %+v", len(got), *alarms)
	}
	if d := got[0].Detail; got[0].Backend != accounting.BackendCodex || !strings.Contains(d, "codex") ||
		!strings.Contains(d, "last.audioInputTokens=42") {
		t.Errorf("alarm must be codex's and name the key path and value: %+v", got[0])
	}
	if n := len(readCycles(t, path)); n != 2 {
		t.Errorf("booked %d cycles, want 2: an unseen key does not stop the booking", n)
	}
}

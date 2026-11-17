//go:build integration

package integration

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/testharness"
)

// TestL2_UsageReadAlert_PerAccount (#3144, end to end): account B's
// get_usage reports the five-hour window at 85%. The scheduled usage read of
// B (one read for the login, the first 10-60 s after start) tells beta AND
// beta2 (both on B) in their own chats, once each, with the stream's notice
// text at the read's use; alpha (host account, no usage figure) has been
// told nothing by then.
func TestL2_UsageReadAlert_PerAccount(t *testing.T) {
	testharness.ParallelWait(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	writeStubUsage(t, ccB, 85)
	users := [3]int64{31144, 31145, 31146}
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: users[0]},
			{ID: "beta", UserID: users[1], ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
			{ID: "beta2", UserID: users[2], ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	for i, id := range []string{"alpha", "beta", "beta2"} {
		marker := fmt.Sprintf("WARM_3144_%s", id)
		pushUserMessage(t, h, id, users[i], marker)
		waitForTurnDone(t, h, id, fmt.Sprintf("%s/c%d", id, users[i]), marker)
	}

	const notice = "Approaching Anthropic 5-hour rate limit."
	// #3154: the figure is in the window table (Q97 dropped the notice's own
	// "Usage:" line when a table is attached).
	waitSent(t, h, "beta", notice, "| 5h | 85% |")
	waitSent(t, h, "beta2", notice, "| 5h | 85% |")
	for _, id := range []string{"beta", "beta2"} {
		if n := countContaining(sentTexts(h, id), notice); n != 1 {
			t.Errorf("%s got %d usage notices, want 1: %q", id, n, sentTexts(h, id))
		}
	}
	if n := countContaining(sentTexts(h, "alpha"), "Approaching Anthropic"); n != 0 {
		t.Errorf("alpha (host account) got %d usage notices: %q", n, sentTexts(h, "alpha"))
	}
}

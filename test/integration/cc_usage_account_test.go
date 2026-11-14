//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// The #3104 phase-3 L2 tests: usage limits are per Claude Code account.
// alpha is on the host folder; beta and beta2 on <tmp>/ccB. cc-stub answers
// get_usage per config folder (.ccstub-usage.json), and a scripted turn
// emits a rate_limit_event, which feeds the usage hold.

// writeStubUsage sets what get_usage reports for one config folder.
func writeStubUsage(t *testing.T, dir string, fiveHour int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]int{"five_hour": fiveHour, "seven_day": 5})
	if err := os.WriteFile(filepath.Join(dir, ".ccstub-usage.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// scriptFiveHourEvent makes agentID's next turn containing match report the
// five-hour window at util (0..1), resetting in two hours.
func scriptFiveHourEvent(t *testing.T, h *testharness.Harness, agentID, match string, util float64) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"match": match,
		"extra_envelopes": []map[string]any{{
			"type": "rate_limit_event",
			"rate_limit_info": map[string]any{
				"status":        "allowed_warning",
				"rateLimitType": "five_hour",
				"utilization":   util,
				"resetsAt":      float64(time.Now().Add(2 * time.Hour).Unix()),
			},
		}},
		"text": "stub-reply: " + match,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.WriteCCStubScript(t, agentID, body)
}

// startUsageAccounts starts alpha (host folder), beta and beta2 (ccB) and
// runs one turn each, so every chat exists.
func startUsageAccounts(t *testing.T, base int64, ccB string) (*testharness.Harness, [3]int64) {
	t.Helper()
	users := [3]int64{base, base + 1, base + 2}
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: users[0]},
			{ID: "beta", UserID: users[1], ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
			{ID: "beta2", UserID: users[2], ExtraEnv: map[string]string{"CLAUDE_CONFIG_DIR": ccB}},
		},
		ReadyTimeout: 30 * time.Second,
	})
	for i, id := range []string{"alpha", "beta", "beta2"} {
		marker := fmt.Sprintf("WARM_31043_%s", id)
		pushUserMessage(t, h, id, users[i], marker)
		waitForTurnDone(t, h, id, fmt.Sprintf("%s/c%d", id, users[i]), marker)
	}
	return h, users
}

// ranUserMessage reports whether a user message containing text has reached
// cc-stub in a workdir containing workdir, now (no wait).
func ranUserMessage(t *testing.T, h *testharness.Harness, workdir, text string) bool {
	t.Helper()
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, workdir) && strings.Contains(e.TextPrefix, text) {
			return true
		}
	}
	return false
}

// holdB makes beta report B's five-hour window at 99% and waits for B's
// hold notice in beta's chat, which names B's account.
func holdB(t *testing.T, h *testharness.Harness, user int64, ccB string) {
	t.Helper()
	scriptFiveHourEvent(t, h, "beta", "HOLD_31043_B", 0.99)
	pushUserMessage(t, h, "beta", user, "HOLD_31043_B")
	waitSent(t, h, "beta", "Usage hold", "(account: "+ccB+")")
}

// TestL2_UsageHold_PerAccount (H1, H7): B's five-hour window at 99% holds
// beta and beta2 (beta2's message waits, and its "held" reply names B), and
// alpha (host account) runs at once.
func TestL2_UsageHold_PerAccount(t *testing.T) {
	testharness.ParallelWait(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	h, users := startUsageAccounts(t, 31060, ccB)
	holdB(t, h, users[1], ccB)

	pushUserMessage(t, h, "beta2", users[2], "MARKER_31043_B2")
	waitSent(t, h, "beta2", "Held:", "(account: "+ccB+")")
	pushUserMessage(t, h, "alpha", users[0], "MARKER_31043_A1")
	waitForTurnDone(t, h, "alpha", fmt.Sprintf("alpha/c%d", users[0]), "MARKER_31043_A1")
	if n := countContaining(sentTexts(h, "alpha"), "Usage hold"); n != 0 {
		t.Errorf("alpha (host account) got %d usage hold notices", n)
	}
	if n := countContaining(sentTexts(h, "alpha"), "Held:"); n != 0 {
		t.Errorf("alpha (host account) got %d held replies", n)
	}
	// alpha's whole turn ran meanwhile; beta2's message still has not.
	if ranUserMessage(t, h, "workspaces/beta2", "MARKER_31043_B2") {
		t.Error("beta2's message ran during B's hold")
	}
}

// TestL2_Usage_PerAccount (H3): /usage reads the calling
// agent's own account and names it; with B held, /usage in alpha's chat
// (headroom on A) does not lift B; /usage in beta's chat once B reports
// headroom lifts B, and beta2's held message then runs.
func TestL2_Usage_PerAccount(t *testing.T) {
	testharness.ParallelWait(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	writeStubUsage(t, ccB, 99)
	h, users := startUsageAccounts(t, 31070, ccB)
	host := filepath.Join(h.TempDir(), "workspaces", ".claude")
	writeStubUsage(t, host, 10)

	pushUserMessage(t, h, "beta", users[1], "/usage")
	waitSent(t, h, "beta", "Session (5h): 99%", "Account: "+ccB)
	pushUserMessage(t, h, "alpha", users[0], "/usage")
	waitSent(t, h, "alpha", "Session (5h): 10%", "Account: ")
	if n := countContaining(sentTexts(h, "alpha"), "Account: "+ccB); n != 0 {
		t.Errorf("alpha's /usage named B's account")
	}
	// beta's read ran in B's folder. (The spawn reserve's scheduled reads
	// add get_usage reads of their own, so only presence is checked.)
	sawB := false
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		sawB = sawB || (e.Kind == "get_usage" && e.ConfigDir == ccB)
	}
	if !sawB {
		t.Errorf("no get_usage read ran with CLAUDE_CONFIG_DIR=%s", ccB)
	}

	holdB(t, h, users[1], ccB)
	pushUserMessage(t, h, "beta2", users[2], "MARKER_31043_B2")
	waitSent(t, h, "beta2", "Held:")

	// alpha's read shows headroom on A: B stays held.
	pushUserMessage(t, h, "alpha", users[0], "/usage")
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	for countContaining(sentTexts(h, "alpha"), "Session (5h): 10%") < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if ranUserMessage(t, h, "workspaces/beta2", "MARKER_31043_B2") {
		t.Fatal("/usage in alpha's chat lifted B: beta2's held message ran")
	}

	// B now reports headroom: /usage in beta's chat lifts B only.
	writeStubUsage(t, ccB, 10)
	pushUserMessage(t, h, "beta", users[1], "/usage")
	waitSent(t, h, "beta", "Usage is available", "Account: "+ccB)
	if !waitForUserMessage(t, h, "workspaces/beta2", "MARKER_31043_B2", 20*time.Second) {
		t.Errorf("beta2's held message did not run after B was lifted\nstderr:\n%s", stderrTail(h.Stderr()))
	}
}

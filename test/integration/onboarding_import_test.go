//go:build integration

package integration

import (
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// First-run import (#3049): the first agent on a NEW install gets the import
// from other AI tools appended to its onboarding; every other agent gets the
// onboarding alone. These drive the real foci-gw binary with claude-code
// (cc-stub) agents, so the delegated delivery path (ComposePrompt) is the one
// under test. The import's first line is the marker below; it sits right after
// the ~1k-char onboarding, inside cc-stub's 2k recorded prefix.
const importMarkerL2 = "[IMPORT FROM OTHER AI TOOLS]"

func pushHello(h *testharness.Harness, agentID string, uid int64, text string) {
	h.TelegramStub().PushUpdate(h.AgentBotToken(agentID), gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: uid, Type: "private"},
			From: &gotgbot.User{Id: uid, FirstName: "Tester"},
			Text: text,
		},
	})
}

// A fresh install with one agent: its first turn carries the onboarding AND
// the import.
func TestL2_OnboardingImport_FreshInstallOneAgent(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:           []testharness.AgentSpec{{ID: "alpha", UserID: 9111}},
		EnableOnboarding: true,
	})
	pushHello(h, "alpha", 9111, "first hello")
	if !waitForUserMessage(t, h, "workspaces/alpha", "[FIRST RUN]", 30*time.Second) {
		t.Fatalf("onboarding never delivered\nstderr:\n%s", stderrTail(h.Stderr()))
	}
	if n := countUserMessagesContaining(t, h, "alpha", importMarkerL2); n != 1 {
		t.Fatalf("import delivered %d times to the only fresh agent, want 1\nrecorder:\n%s", n, recorderTail(t, h.RecorderPath()))
	}
}

// Two fresh agents at one boot, first turns pushed together: both get the
// onboarding, exactly one gets the import.
func TestL2_OnboardingImport_TwoFreshAgentsExactlyOne(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9112},
			{ID: "beta", UserID: 9113},
		},
		EnableOnboarding: true,
	})
	pushHello(h, "alpha", 9112, "hello alpha")
	pushHello(h, "beta", 9113, "hello beta")
	for _, id := range []string{"alpha", "beta"} {
		if !waitForUserMessage(t, h, "workspaces/"+id, "[FIRST RUN]", 30*time.Second) {
			t.Fatalf("%s: onboarding never delivered\nstderr:\n%s", id, stderrTail(h.Stderr()))
		}
	}
	n := countUserMessagesContaining(t, h, "alpha", importMarkerL2) + countUserMessagesContaining(t, h, "beta", importMarkerL2)
	if n != 1 {
		t.Fatalf("import delivered %d times across two fresh agents, want exactly 1\nrecorder:\n%s", n, recorderTail(t, h.RecorderPath()))
	}
}

// An existing install (an agent already onboarded, no install marker): a
// fresh agent gets the onboarding but no import.
func TestL2_OnboardingImport_ExistingInstallNoImport(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9114},
			{ID: "delta", UserID: 9115},
		},
		EnableOnboarding:  true,
		SeedAgentMetadata: map[string]map[string]string{"alpha": {"first_run_completed": "true"}},
	})
	pushHello(h, "delta", 9115, "hello delta")
	if !waitForUserMessage(t, h, "workspaces/delta", "[FIRST RUN]", 30*time.Second) {
		t.Fatalf("delta: onboarding never delivered\nstderr:\n%s", stderrTail(h.Stderr()))
	}
	if n := countUserMessagesContaining(t, h, "delta", importMarkerL2); n != 0 {
		t.Fatalf("import delivered on an existing install\nrecorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
}

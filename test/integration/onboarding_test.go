//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// First-run onboarding (checkFirstRun, cmd/foci-gw/agents.go:337) injects
// the prompts.FirstRun() text ("[FIRST RUN] ...") as a startup turn for any
// agent whose session index lacks first_run_completed=true. That startup
// turn spawns a CC backend and races any one-shot cc-stub script a test
// writes after StartGateway — which is why the harness suppresses onboarding
// by default (seeds first_run_completed=true for every agent). These two
// tests pin BOTH sides of that switch so a regression in either the harness
// default or the production checkFirstRun path fails loudly.

// TestL2_Onboarding_FiresWhenEnabled proves the opt-in path: with
// EnableOnboarding set, the harness leaves first_run_completed unset, so
// foci-gw's checkFirstRun stores the onboarding prompt (FirstRunMessage) to be
// prepended as a separate content block on the FIRST turn that builds a
// message (internal/agent/turn_message.go:82 — it is NOT a standalone startup
// turn; it rides the first real turn). Drive one turn, then assert the cc-stub
// recorder captures a user_message for alpha's workdir whose text carries the
// "[FIRST RUN]" marker (cc-stub concatenates all content-block text, so the
// prepended onboarding block lands in TextPrefix).
func TestL2_Onboarding_FiresWhenEnabled(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9101},
		},
		EnableOnboarding: true,
	})

	// The onboarding message only gets consumed when a real turn builds a
	// message — drive one so the stored FirstRunMessage is prepended.
	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 9101, Type: "private"},
			From: &gotgbot.User{Id: 9101, FirstName: "Tester"},
			Text: "first hello",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "[FIRST RUN]", 30*time.Second) {
		t.Fatalf("onboarding never fired with EnableOnboarding=true; expected a [FIRST RUN] block prepended to the first turn for alpha\nstderr:\n%s",
			stderrTail(h.Stderr()))
	}
}

// TestL2_Onboarding_SuppressedByDefault proves the default path: with
// EnableOnboarding unset the harness seeds first_run_completed=true, so
// checkFirstRun returns "" and NO onboarding prompt is injected. Assertion:
// after driving one ordinary turn to completion (proving the agent is live
// and processing), the recorder holds the driven message but never a
// "[FIRST RUN]" marker.
func TestL2_Onboarding_SuppressedByDefault(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9102},
		},
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 9102, Type: "private"},
			From: &gotgbot.User{Id: 9102, FirstName: "Tester"},
			Text: "steady-state turn",
		},
	})
	// Wait for the driven turn so we know the agent booted and processed a
	// message. Had onboarding fired, its "[FIRST RUN]" turn would already be
	// in the recorder by the time this returns (it precedes any inbound
	// message, injected at startup).
	if !waitForUserMessage(t, h, "workspaces/alpha", "steady-state turn", 30*time.Second) {
		t.Fatalf("driven turn never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[FIRST RUN]") {
			t.Fatalf("onboarding fired despite default suppression — first_run_completed seed regressed\nmatching entry: %q\nstderr:\n%s",
				e.TextPrefix, stderrTail(h.Stderr()))
		}
	}
}

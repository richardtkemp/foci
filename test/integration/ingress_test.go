//go:build integration

// Package integration holds foci's L2 integration tests. They spin up a
// real foci-gw subprocess against a stubbed Telegram Bot API and a
// stubbed `claude` binary (cc-stub), then drive scenarios end-to-end and
// assert on side effects (cc-stub invocation recorder, Telegram stub's
// outbound call log).
//
// Run with: go test -tags=integration ./test/integration/...
package integration

import (
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_Ingress_TelegramMessageReachesAgent is the first end-to-end
// scenario test for the L2 layer. It pushes a Telegram update to an
// agent's bot and asserts that foci-gw spawned cc-stub in the agent's
// workspace. This proves the full ingress pipeline works:
//
//	PushUpdate → getUpdates poll → bot handler → session creation
//	→ Agent.HandleMessage → backend.Start (cc-stub spawn)
//
// The assertion is structural: a JSONL line in the cc-stub recorder
// with workdir == the test agent's workspace.
func TestL2_Ingress_TelegramMessageReachesAgent(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7777},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 7777, Type: "private"},
			From: &gotgbot.User{Id: 7777, FirstName: "Tester"},
			Text: "hello alpha",
		},
	})

	deadline := time.Now().Add(15 * time.Second)
	var invocations []recorderEntry
	for time.Now().Before(deadline) {
		invocations = invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
		if len(invocations) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(invocations) == 0 {
		t.Fatalf("cc-stub for alpha was never invoked; foci-gw stderr:\n%s", h.Stderr())
	}

	inv := invocations[0]
	if !strings.Contains(inv.Workdir, "workspaces/alpha") {
		t.Errorf("cc-stub workdir = %q, want a path under workspaces/alpha", inv.Workdir)
	}
}

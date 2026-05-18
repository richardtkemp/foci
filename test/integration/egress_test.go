//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_Egress_AssistantReplyReachesTelegram asserts that when an agent
// emits an assistant text response, foci-gw delivers it to the user's
// Telegram chat via sendMessage. This is the egress half of the
// platform-bridge contract — paired with ingress, it proves the full
// round-trip works without a real Telegram or a real claude.
//
// Mechanism: send a Telegram update to alpha containing the text
// "round-trip ping". cc-stub's default behaviour echoes the user text
// prefixed with "stub-reply: ". The reply travels back through foci's
// Telegram bridge to the stub's recorded sendMessage calls. Test polls
// until the recorded sendMessage body contains the echo prefix.
func TestL2_Egress_AssistantReplyReachesTelegram(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5555},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 5555, Type: "private"},
			From: &gotgbot.User{Id: 5555, FirstName: "Tester"},
			Text: "round-trip ping",
		},
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			text, _ := body["text"].(string)
			if strings.Contains(text, "stub-reply") && strings.Contains(text, "round-trip ping") {
				return // pass
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("never received a sendMessage with the stub-reply echo; sent calls were:\n%s\nstderr tail:\n%s",
		sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
}

// TestL2_Egress_LateAssistantTextReachesTelegram captures the suspected
// delivery gap from 2026-05-18: a detailed report posted by the agent
// AFTER a turn ended never reached Telegram. The architecture has a
// session router with a SessionSink fallback for "late-arriving text
// outside any turn" (see internal/agent/session_router.go +
// inbox.lateDeliverySink), so this path SHOULD deliver via
// platform.Connection.SendToSession. This test exercises the fallback.
//
// Mechanism: cc-stub script sets LateText="this is the late reply".
// The stub emits the normal assistant + result envelopes, sleeps 200ms
// to let foci process the turn-complete (router.Clear via defer in
// run_turn.go), then emits a SECOND assistant message. With no
// per-turn sink registered, the session router forwards to its
// late-delivery fallback, which should send via SendToSession.
//
// Should FAIL until the late-delivery path is verified working — this
// is the test for clutch's "lost top-20 report" hypothesis, where a
// CC-harness-internal task-notification injection produced assistant
// text outside any foci-side user message.
func TestL2_Egress_LateAssistantTextReachesTelegram(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5556},
		},
		ReadyTimeout: 30 * time.Second,
	})

	const firstReply = "first reply from turn"
	const lateReply = "this is the late reply"
	scriptBody, err := json.Marshal(map[string]any{
		"text":       firstReply,
		"late_text":  lateReply,
	})
	if err != nil {
		t.Fatalf("marshal script body: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 5556, Type: "private"},
			From: &gotgbot.User{Id: 5556, FirstName: "Tester"},
			Text: "kick off a turn",
		},
	})

	// First, wait for the in-turn reply to arrive. If this never comes
	// the harness itself is broken and the late-delivery assertion is
	// meaningless.
	gotFirst := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			text, _ := body["text"].(string)
			if strings.Contains(text, firstReply) {
				gotFirst = true
				break
			}
		}
		if gotFirst {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gotFirst {
		t.Fatalf("first reply %q never arrived; sent calls:\n%s\nstderr:\n%s",
			firstReply, sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	// Now poll for the late-delivered text. This is the actual assertion
	// — the in-turn reply is just the prelude.
	gotLate := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			text, _ := body["text"].(string)
			if strings.Contains(text, lateReply) {
				gotLate = true
				break
			}
		}
		if gotLate {
			return // pass
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("late-delivered text %q never reached Telegram (this is the suspected bug); sent calls:\n%s\nstderr:\n%s",
		lateReply, sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
}

// sentCallsTail summarises recorded sendMessage bodies for failure logs.
func sentCallsTail(stub *testharness.TelegramStub, token string) string {
	var sb strings.Builder
	for _, c := range stub.PeekSent(token) {
		sb.WriteString("  ")
		sb.WriteString(c.Method)
		sb.WriteString("\t")
		sb.Write(c.Body)
		sb.WriteString("\n")
	}
	return sb.String()
}

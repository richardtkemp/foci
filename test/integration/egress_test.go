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

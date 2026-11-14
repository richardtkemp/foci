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

// legacyCompletionWarning is the Warnf ccstream logs the first time a foci
// turn's result arrives with no session_state_changed event seen: the
// backend has fallen back to complete-on-result (handlers.go OnResult).
const legacyCompletionWarning = "no session_state_changed events from CC"

// runOneTelegramTurn sends one Telegram message to agent alpha and waits for
// cc-stub's echo of it to reach the chat.
func runOneTelegramTurn(t *testing.T, h *testharness.Harness, userID int64, text string) {
	t.Helper()
	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: userID, Type: "private"},
			From: &gotgbot.User{Id: userID, FirstName: "Tester"},
			Text: text,
		},
	})
	if !waitForStubReply(h, text, 20*time.Second) {
		t.Fatalf("reply to %q never reached Telegram; stderr tail:\n%s", text, stderrTail(h.Stderr()))
	}
}

// waitForStubReply waits for cc-stub's echo of text to reach alpha's chat.
func waitForStubReply(h *testharness.Harness, text string, timeout time.Duration) bool {
	token := h.AgentBotToken("alpha")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			if s, _ := body["text"].(string); strings.Contains(s, "stub-reply") && strings.Contains(s, text) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestL2_SessionState_StubRunsProductionCompletionPath pins #3055: cc-stub
// emits session_state_changed running/idle as real CC does (foci sets
// CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS at launch), so L2 turns complete on
// idle, the path production runs, and never on ccstream's legacy
// complete-on-result fallback. Paired with the opt-out test below, which
// shows the warning this test looks for does appear when the events are off.
func TestL2_SessionState_StubRunsProductionCompletionPath(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: 5571}},
		ReadyTimeout: 30 * time.Second,
	})
	runOneTelegramTurn(t, h, 5571, "idle keyed one")
	runOneTelegramTurn(t, h, 5571, "idle keyed two")
	if strings.Contains(h.Stderr(), legacyCompletionWarning) {
		t.Fatalf("ccstream fell back to complete-on-result: cc-stub emitted no session-state events; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
}

// TestL2_SessionState_OptOutRunsLegacyCompletionPath keeps the no-events
// path reachable: a per-agent backend_config.env that turns
// CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS off makes cc-stub emit none (as real
// CC would), and turns still complete, on the result.
func TestL2_SessionState_OptOutRunsLegacyCompletionPath(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:       "alpha",
			UserID:   5572,
			ExtraEnv: map[string]string{"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "0"},
		}},
		ReadyTimeout: 30 * time.Second,
	})
	runOneTelegramTurn(t, h, 5572, "result keyed one")
	runOneTelegramTurn(t, h, 5572, "result keyed two")
	if !waitForStderr(h, legacyCompletionWarning, 5*time.Second) {
		t.Fatalf("expected the legacy complete-on-result warning with session-state events off; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
}

// TestL2_SessionState_SteerFoldedIntoOpenRun runs production's steer path end
// to end (#3056): a message sent while a turn is open reaches cc-stub
// mid-run, cc-stub folds it into that run as real CC does (result, init,
// assistant, result, ONE idle), and foci still delivers both replies.
func TestL2_SessionState_SteerFoldedIntoOpenRun(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 5573
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:       "alpha",
			UserID:   userID,
			ExtraEnv: map[string]string{"CCSTUB_HANG_DURING_TURN": "2s"},
		}},
		ReadyTimeout: 30 * time.Second,
	})
	pushUserMessage(t, h, "alpha", userID, "FOLD_FIRST")
	if !waitForUserMessage(t, h, "workspaces/alpha", "FOLD_FIRST", 20*time.Second) {
		t.Fatalf("first turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "alpha", userID, "FOLD_STEER")
	for _, m := range []string{"FOLD_FIRST", "FOLD_STEER"} {
		if !waitForStubReply(h, m, 30*time.Second) {
			t.Fatalf("reply to %s never reached Telegram; recorder:\n%s\nstderr:\n%s",
				m, recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
		}
	}
	// The steer must have reached cc-stub while the first run was open;
	// otherwise this test ran the old "steer = next run" shape.
	var folded bool
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "FOLD_STEER") {
			folded = e.Folded
		}
	}
	if !folded {
		t.Fatalf("FOLD_STEER was not folded into the open run; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
}

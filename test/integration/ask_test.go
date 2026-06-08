//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// The ask test cluster exercises the foci-native `ask`/`foci_ask` tool
// end-to-end through the real gateway:
//
//	cc-stub emits a Bash tool_use running `foci_ask --json '{...}'`
//	  → the exec bridge dispatches to internal/tools/ask
//	  → the tool posts question 1 to Telegram as an inline keyboard and
//	    returns IMMEDIATELY (async)
//	  → a callback_query (button click) advances the accumulator; the next
//	    question is posted, then the next click completes it
//	  → the assembled {questions, answers} batch is delivered back into the
//	    session via the same SessionNotifyFn → HandleMessage path as
//	    send_to_session, waking cc-stub with a fresh user_message
//
// This proves the parts the in-process tests can't: the real bash shell
// function, the real Telegram interactive-message round-trip (button data
// routing), and async answer delivery reaching the backend as a new turn.

// askButtonData returns the callback_data of the keyboard button whose visible
// text equals label. Fails the test if absent.
func askButtonData(t *testing.T, kb [][]map[string]string, label string) string {
	t.Helper()
	for _, row := range kb {
		for _, b := range row {
			if b["text"] == label {
				return b["callback_data"]
			}
		}
	}
	t.Fatalf("button %q not found in keyboard %+v", label, kb)
	return ""
}

// answerQuestion waits for a question prompt containing wantText, then clicks
// the button labelled clickLabel by replaying its callback_data.
func answerQuestion(t *testing.T, h *testharness.Harness, token, wantText, clickLabel string) {
	t.Helper()
	call, ok := waitForPermissionPrompt(t, h.TelegramStub(), token, wantText, 20*time.Second)
	if !ok {
		t.Fatalf("question %q never presented\n--- stderr ---\n%s", wantText, stderrTail(h.Stderr()))
	}
	kb := decodeInlineKeyboard(call.Body)
	if kb == nil {
		t.Fatalf("question %q had no inline keyboard; body=%s", wantText, call.Body)
	}
	data := askButtonData(t, kb, clickLabel)
	// msgID is irrelevant: foci routes the callback by callback_data, not by the
	// clicked message id.
	h.TelegramStub().PushCallbackQuery(token, data, permTestUserID, permTestUserID, 0)
}

// TestL2_Ask_FullFlow drives a two-question ask to completion via button clicks
// and asserts the answer batch is delivered back to the backend.
func TestL2_Ask_FullFlow(t *testing.T) {
	t.Parallel()
	h, token := permTestSetup(t, []testharness.AgentSpec{{ID: "alpha", UserID: permTestUserID}}, "")

	// Distinct labels so the delivered batch is unambiguous in the recorder.
	askJSON := `{"questions":[` +
		`{"question":"Which colour?","header":"Colour","options":[{"label":"Crimson"},{"label":"ZEBRA"}]},` +
		`{"question":"Confirm choice?","header":"Confirm","options":[{"label":"Affirmative"},{"label":"Negative"}]}` +
		`]}`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "asking you two questions",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": "foci_ask --json '" + askJSON + "'"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	pushTrigger(t, h, token, "go ahead and ask")

	// Q1 → click ZEBRA; Q2 → click Affirmative.
	answerQuestion(t, h, token, "Which colour?", "ZEBRA")
	answerQuestion(t, h, token, "Confirm choice?", "Affirmative")

	// The completed batch must arrive at cc-stub as a fresh user_message
	// carrying both answers.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind != "user_message" {
				continue
			}
			if strings.Contains(e.TextPrefix, "answered your") &&
				strings.Contains(e.TextPrefix, "ZEBRA") &&
				strings.Contains(e.TextPrefix, "Affirmative") {
				return // pass: full async ask round-trip delivered the batch
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("answer batch never delivered to backend\n--- stderr ---\n%s", stderrTail(h.Stderr()))
}

// Note on typed ("Other") answers: there is intentionally NO L2 test for the
// typed-reply path. It is non-deterministic in the harness — a typed answer can
// arrive during the brief in-flight window of the asking turn (before the tool
// call's turn completes), in which case the platform routes it as a mid-turn
// steer rather than through RunTurn's pending-ask interception. That is the
// documented real limitation (typed answers route only when the session is
// idle; button clicks always work). The typed-routing logic itself is covered
// deterministically by the unit test TestAsk_TypedAnswerViaRouter.

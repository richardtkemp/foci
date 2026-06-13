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
	testharness.ParallelWait(t)
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
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5556},
		},
		ReadyTimeout: 30 * time.Second,
	})

	const firstReply = "first reply from turn"
	const lateReply = "this is the late reply"
	scriptBody, err := json.Marshal(map[string]any{
		"text":      firstReply,
		"late_text": lateReply,
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

// TestL2_Egress_SilentIntermediateThenRealReplyReachesTelegram exercises the
// multi-text-block-in-turn shape: agent emits [[NO_RESPONSE]] as an
// intermediate text, then a real reply also within the same turn (no tool
// loop between them). The real reply must reach Telegram.
//
// This shape was NOT covered before — egress tests only covered single-text
// and post-turn late-delivery. The 2026-05-18 22:33 production incident
// involved a similar multi-text shape but with a pre-answer-gate-driven
// re-round between the two texts (turnText.Reset in beginTurn means the
// second round's text becomes FinalText alone). See
// docs/delivery-gap-2026-05-18-2233.md for the corrected analysis.
//
// In THIS shape (no pre-answer round, two assistant messages back-to-back
// before result), foci accumulates both texts into turnText, fires
// se.OnText for each (emitting TextBlock intermediate to the sink), and the
// second OnReply delivers via SendReply before TurnComplete fires. The
// StreamingSink delivered-flag-set-on-silent bug exposed by the unit test
// TestStreamingSinkSilentIntermediateDoesNotSuppressFinalize does NOT
// manifest here because the second OnReply delivers the real text before
// TurnComplete's delivered=true gate fires.
//
// Mechanism: cc-stub script sets intermediate_texts=["[[NO_RESPONSE]]"] +
// text="real reply". The stub emits two assistant messages within one turn —
// first the silent intermediate, then the real Text-bearing one — followed
// by the result envelope. Passes on current code; locks in the shape so a
// future regression in OnReply's delivery path is caught.
func TestL2_Egress_SilentIntermediateThenRealReplyReachesTelegram(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5557},
		},
		ReadyTimeout: 30 * time.Second,
	})

	const realReply = "the real reply that must reach Telegram"
	scriptBody, err := json.Marshal(map[string]any{
		"intermediate_texts": []string{"[[NO_RESPONSE]]"},
		"text":               realReply,
	})
	if err != nil {
		t.Fatalf("marshal script body: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 5557, Type: "private"},
			From: &gotgbot.User{Id: 5557, FirstName: "Tester"},
			Text: "kick off a turn",
		},
	})

	// Poll for the real reply on Telegram. The silent intermediate must NOT
	// flip the sink's delivered flag — if it does, the real reply (carried
	// by TurnComplete.FinalText) is swallowed and never sendMessage'd.
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
			if strings.Contains(text, realReply) {
				return // pass
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("real reply %q never reached Telegram after a silent intermediate (the 22:33 bug); sent calls:\n%s\nstderr:\n%s",
		realReply, sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
}

// TestL2_Egress_TrailingSentinelStrippedNonStreaming demonstrates the
// trailing-sentinel leak on the non-streaming delivery path. An agent emits
// a SINGLE assistant text block consisting of a real reply immediately
// followed by the [[NO_RESPONSE]] sentinel — the shape produced when an
// agent staples the silence marker onto the end of a real message.
//
// platform.IsSilent is exact-match (TrimSpace(text) == sentinel), so a
// message that merely *ends with* the sentinel is not silent and is
// delivered verbatim — the literal "[[NO_RESPONSE]]" leaks to Telegram.
//
// Desired behaviour: the real reply reaches Telegram with the trailing
// sentinel stripped. This test asserts that — so it FAILS on current code
// (the marker leaks) and PASSES once the trim fix lands.
func TestL2_Egress_TrailingSentinelStrippedNonStreaming(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5560},
		},
		ReadyTimeout: 30 * time.Second,
	})

	const realReply = "all clean, nothing uncommitted"
	scriptBody, err := json.Marshal(map[string]any{
		"text": realReply + "\n[[NO_RESPONSE]]",
	})
	if err != nil {
		t.Fatalf("marshal script body: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 5560, Type: "private"},
			From: &gotgbot.User{Id: 5560, FirstName: "Tester"},
			Text: "keepalive check",
		},
	})

	// Wait for the reply to arrive, then assert it carries the real text
	// WITHOUT the trailing sentinel literal.
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
			if !strings.Contains(text, realReply) {
				continue
			}
			if strings.Contains(text, "[[NO_RESPONSE]]") {
				t.Errorf("delivered message leaked the trailing sentinel:\n  %q\nexpected the [[NO_RESPONSE]] marker to be stripped", text)
			}
			return // found the reply; assertion above decides pass/fail
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("reply %q never reached Telegram; sent calls:\n%s\nstderr:\n%s",
		realReply, sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
}

// TestL2_Egress_TrailingSentinelStrippedStreaming is the streaming twin of
// the test above. With stream_output enabled, the reply arrives as
// token-level deltas that foci accumulates into a live-edited Telegram
// message. The final delta is the [[NO_RESPONSE]] sentinel, appended after
// real text — so by the time it arrives, the streaming-prefix gate
// (IsSilencingPrefix) has already released (the buffer diverged from a pure
// sentinel once real text streamed), and the sentinel is committed into the
// final message edit.
//
// This is the path Dick suspected: the streamed message can't be un-sent, so
// the trailing sentinel must be stripped at the final commit edit. Asserts
// the committed message excludes the marker — FAILS on current code, PASSES
// after the fix.
func TestL2_Egress_TrailingSentinelStrippedStreaming(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 5561},
		},
		ReadyTimeout: 30 * time.Second,
		// Enable live streaming so the reply is delivered via incremental
		// message edits (StreamWriter), exercising chokepoint 3.
		ExtraConfigTOML: "[defaults.display]\nstream_output = true\nstreaming = true\n",
	})

	const realReply = "streamed reply that must stay clean"
	// Deltas concatenate to realReply + trailing sentinel. The sentinel is a
	// separate final delta so the prefix gate has already released on the
	// preceding real text.
	scriptBody, err := json.Marshal(map[string]any{
		"stream_deltas": []string{realReply + " ", "[[NO_RESPONSE]]"},
		"text":          realReply + " [[NO_RESPONSE]]",
	})
	if err != nil {
		t.Fatalf("marshal script body: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 5561, Type: "private"},
			From: &gotgbot.User{Id: 5561, FirstName: "Tester"},
			Text: "keepalive check",
		},
	})

	// The streamed message is created then edited; inspect both sendMessage
	// (initial) and editMessageText (final) calls. Assert the agent's reply
	// is present and the final rendered text excludes the sentinel literal.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" && call.Method != "editMessageText" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			text, _ := body["text"].(string)
			if !strings.Contains(text, realReply) {
				continue
			}
			if strings.Contains(text, "[[NO_RESPONSE]]") {
				t.Errorf("streamed message leaked the trailing sentinel (method=%s):\n  %q\nexpected [[NO_RESPONSE]] to be stripped from the committed edit", call.Method, text)
			}
			return // found the reply; assertion above decides pass/fail
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("streamed reply %q never reached Telegram; sent calls:\n%s\nstderr:\n%s",
		realReply, sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
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

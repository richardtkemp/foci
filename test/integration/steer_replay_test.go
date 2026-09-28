//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"
)

// TestL2_Steer_SurvivesCCDeathBeforeConsumption reproduces foci_todo #2050: a
// steer written to CC's stdin is lost when CC dies before consuming it.
//
// cc-stub's crash_on_unconsumed script holds the primary turn open (assistant
// envelope, no result) and exits 1 the moment the steer's line arrives on its
// stdin, recording it as "unconsumed_user_message". The kill is keyed on the
// steer's arrival, so the write provably reached the process that dies and
// was provably never folded. foci respawns the backend with --resume; the
// steer must reach that respawned process as a consumed user message without
// the user having to send anything else.
//
// On failure the test also sends a follow-up message and reports whether the
// steer rode along with it, plus every outbound Telegram message, so the
// evidence says what the user was (not) told.
func TestL2_Steer_SurvivesCCDeathBeforeConsumption(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 20501
	const primary = "MARKER_2050_PRIMARY"
	const steer = "MARKER_2050_STEER"
	const followUp = "MARKER_2050_FOLLOWUP"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})
	script, _ := json.Marshal(map[string]any{
		"match":               primary,
		"text":                "working on it",
		"crash_on_unconsumed": steer,
	})
	h.WriteCCStubScript(t, "alpha", script)

	pushUserMessage(t, h, "alpha", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/alpha", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// The turn is now held open inside cc-stub, so this message steers.
	pushUserMessage(t, h, "alpha", userID, steer)
	if !waitForRecorderKind(t, h, "unconsumed_user_message", steer) {
		t.Fatalf("steer never reached the first cc-stub's stdin (not steered?); recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	waitForStubExitObserved(t, h, "alpha", userID, "workspaces/alpha")

	if waitForUserMessage(t, h, "workspaces/alpha", steer, 0) {
		return
	}

	// Lost. Gather evidence: does a follow-up turn carry it, and what did the
	// user see?
	pushUserMessage(t, h, "alpha", userID, followUp)
	followUpArrived := waitForUserMessage(t, h, "workspaces/alpha", followUp, 20*time.Second)
	steerWithFollowUp := countUserMessagesContaining(t, h, "alpha", steer) > 0
	t.Fatalf("steer %q written into a CC that died before consuming it was never delivered to the respawned CC "+
		"(follow-up turn arrived=%v, steer rode along with it=%v)\n"+
		"invocations:\n%s\nrecorder:\n%s\ntelegram out:\n%s\nstderr:\n%s",
		steer, followUpArrived, steerWithFollowUp,
		describeInvocations(t, h, "workspaces/alpha"), recorderTail(t, h.RecorderPath()),
		telegramOutbound(h, "alpha"), stderrTail(h.Stderr()))
}

// TestL2_Steer_FoldedBeforeDeathNotRedelivered is the no-double-delivery arm
// of #2050: CC folds a steer into its conversation (writes its transcript
// record) and dies before acknowledging it. foci must find the steer in the
// dead process's transcript and NOT redeliver it — a redelivery would put the
// same message into the resumed conversation twice.
//
// The assertion is ordered, not timed: after the death a follow-up message is
// queued; any redelivery is enqueued at the death, ahead of it, so once the
// follow-up reaches the respawned CC every redelivery would already have too.
func TestL2_Steer_FoldedBeforeDeathNotRedelivered(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 20502
	const primary = "MARKER_2050B_PRIMARY"
	const steer = "MARKER_2050B_STEER"
	const followUp = "MARKER_2050B_FOLLOWUP"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})
	script, _ := json.Marshal(map[string]any{
		"match":                 primary,
		"text":                  "working on it",
		"crash_after_consuming": steer,
	})
	h.WriteCCStubScript(t, "alpha", script)

	pushUserMessage(t, h, "alpha", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/alpha", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "alpha", userID, steer)
	if !waitForUserMessage(t, h, "workspaces/alpha", steer, 20*time.Second) {
		t.Fatalf("steer never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	waitForStubExitObserved(t, h, "alpha", userID, "workspaces/alpha")

	pushUserMessage(t, h, "alpha", userID, followUp)
	if !waitForUserMessage(t, h, "workspaces/alpha", followUp, 20*time.Second) {
		t.Fatalf("follow-up never reached the respawned cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	for _, m := range []string{primary, steer} {
		if n := countUserMessagesContaining(t, h, "alpha", m); n != 1 {
			t.Errorf("%s delivered %d times, want exactly 1 (a message CC folded before dying must not be redelivered)", m, n)
		}
	}
	if t.Failed() {
		t.Logf("recorder:\n%s\nstderr:\n%s", recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_Steer_AckedInputsNotRedelivered is the ack arm of #2050: messages CC
// acknowledged (user_message_uuids on their response) — a turn-starting
// message and a second turn's message — are not redelivered when CC later
// dies holding an unconsumed steer. Only the steer is. cc-stub keeps no
// transcript for these, so only the ack can be what spares them. Ordered like
// the test above: the follow-up is queued after every redelivery.
func TestL2_Steer_AckedInputsNotRedelivered(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 20503
	const first = "MARKER_2050C_FIRST"
	const second = "MARKER_2050C_SECOND"
	const steer = "MARKER_2050C_STEER"
	const followUp = "MARKER_2050C_FOLLOWUP"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	pushUserMessage(t, h, "alpha", userID, first)
	if !waitForUserMessage(t, h, "workspaces/alpha", first, 20*time.Second) {
		t.Fatalf("first turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	script, _ := json.Marshal(map[string]any{
		"match":               second,
		"text":                "working on it",
		"crash_on_unconsumed": steer,
	})
	h.WriteCCStubScript(t, "alpha", script)
	pushUserMessage(t, h, "alpha", userID, second)
	if !waitForUserMessage(t, h, "workspaces/alpha", second, 20*time.Second) {
		t.Fatalf("second turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "alpha", userID, steer)
	if !waitForRecorderKind(t, h, "unconsumed_user_message", steer) {
		t.Fatalf("steer never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	waitForStubExitObserved(t, h, "alpha", userID, "workspaces/alpha")

	pushUserMessage(t, h, "alpha", userID, followUp)
	if !waitForUserMessage(t, h, "workspaces/alpha", followUp, 20*time.Second) {
		t.Fatalf("follow-up never reached the respawned cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	for _, m := range []string{first, second, steer} {
		if n := countUserMessagesContaining(t, h, "alpha", m); n != 1 {
			t.Errorf("%s delivered %d times, want exactly 1", m, n)
		}
	}
	if t.Failed() {
		t.Logf("recorder:\n%s\nstderr:\n%s", recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_Steer_PendingSurvivesGatewayRestart: a steer CC has not consumed when
// foci shuts down is persisted and re-sent by the next foci process (#2050,
// Dick's ruling: pending input is preserved across shutdown/restart). The
// primary turn is held open by a crash_on_unconsumed marker that never
// arrives, so the steer sits unconsumed in the stub until the shutdown.
func TestL2_Steer_PendingSurvivesGatewayRestart(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 20504
	const primary = "MARKER_2050D_PRIMARY"
	const steer = "MARKER_2050D_STEER"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})
	script, _ := json.Marshal(map[string]any{
		"match":               primary,
		"text":                "working on it",
		"crash_on_unconsumed": "MARKER_2050D_NEVER_SENT",
	})
	h.WriteCCStubScript(t, "alpha", script)

	pushUserMessage(t, h, "alpha", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/alpha", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	pushUserMessage(t, h, "alpha", userID, steer)
	// The held stub reads the steer without folding it. foci persisted it
	// before writing it, so once the stub has it the pending row exists.
	if !waitForRecorderKind(t, h, "held_user_message", steer) {
		t.Fatalf("steer never reached the held cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	if err := h.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !waitForUserMessage(t, h, "workspaces/alpha", steer, 30*time.Second) {
		t.Fatalf("steer pending at shutdown was not redelivered after the restart; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if n := countUserMessagesContaining(t, h, "alpha", primary); n != 1 {
		t.Errorf("primary (acknowledged before the shutdown) delivered %d times, want 1", n)
	}
}

// waitForRecorderKind waits for a recorder entry of the given kind whose text
// contains marker.
func waitForRecorderKind(t *testing.T, h *testharness.Harness, kind, marker string) bool {
	t.Helper()
	deadline := time.Now().Add(testharness.CorrectnessWaitFloor)
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind == kind && strings.Contains(e.TextPrefix, marker) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func describeInvocations(t *testing.T, h *testharness.Harness, workdirSubstr string) string {
	var b strings.Builder
	for _, inv := range invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), workdirSubstr) {
		if isBatchInvocation(inv) {
			continue
		}
		fmt.Fprintf(&b, "  pid=%d resume=%q\n", inv.PID, inv.ResumeID)
	}
	return b.String()
}

// telegramOutbound lists the text of every message foci sent or edited on the
// agent's bot.
func telegramOutbound(h *testharness.Harness, agentID string) string {
	var b strings.Builder
	for _, c := range h.TelegramStub().PeekSent(h.AgentBotToken(agentID)) {
		if c.Method != "sendMessage" && c.Method != "editMessageText" {
			continue
		}
		var body struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(c.Body, &body)
		fmt.Fprintf(&b, "  %s: %q\n", c.Method, body.Text)
	}
	return b.String()
}

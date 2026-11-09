//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/testharness"
)

// TestL2_RestartResumesCutTurn (#2999): a user turn still running when foci
// restarts is cut; the next process gives the session exactly one resume
// turn saying so. The cut turn's input reached cc-stub and stays in its kept
// conversation, so the resume does not repeat it (the input is delivered
// once).
func TestL2_RestartResumesCutTurn(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 29991
	const primary = "MARKER_2999_PRIMARY"
	const resumeMarker = "[SYSTEM: FOCI RESTARTED]"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
		// The graceful wait runs out well inside Restart's 15s grace, so the
		// shutdown itself cuts the held turn (MarkTurnsCut, then the backend
		// close), not the SIGKILL a crash would be.
		GracefulShutdownTimeout: "2s",
	})
	// Hold the primary turn open (assistant envelope, no result) until the
	// restart cuts it.
	script, _ := json.Marshal(map[string]any{
		"match":               primary,
		"text":                "working on it",
		"crash_on_unconsumed": "MARKER_2999_NEVER_SENT",
	})
	h.WriteCCStubScript(t, "alpha", script)

	pushUserMessage(t, h, "alpha", userID, primary)
	if !waitForUserMessage(t, h, "workspaces/alpha", primary, 20*time.Second) {
		t.Fatalf("primary turn never reached cc-stub; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	if err := h.Restart(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !waitForUserMessage(t, h, "workspaces/alpha", resumeMarker, 30*time.Second) {
		t.Fatalf("the cut turn was not resumed after the restart; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	// Nothing further may resume it.
	time.Sleep(2 * time.Second)
	if n := countUserMessagesContaining(t, h, "alpha", resumeMarker); n != 1 {
		t.Errorf("resume turns = %d, want 1", n)
	}
	if n := countUserMessagesContaining(t, h, "alpha", primary); n != 1 {
		t.Errorf("primary delivered %d times, want 1 (the kept conversation has it; the resume does not repeat it)", n)
	}
	if t.Failed() {
		t.Logf("recorder:\n%s\nstderr:\n%s", recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

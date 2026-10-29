package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

const creditsMsg = "Your workspace is out of credits. Ask your workspace owner to refill in order to continue."

// TestTurnFailedErrorReachesChat proves a failed codex turn's error message is
// delivered to the user — streamed through OnText and present in the turn's
// final text — instead of only being logged. Drives a real turn/completed
// notification carrying status "failed" through dispatch.
func TestTurnFailedErrorReachesChat(t *testing.T) {
	b := newTestBackend(t)
	var streamed []string
	b.AttachSessionEvents(&delegator.SessionEvents{OnText: func(s string) { streamed = append(streamed, s) }})
	var got *delegator.TurnResult
	openTurn(b, &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) { got = r }})

	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"tu_1","status":"failed","error":{"message":"` + creditsMsg + `"}}}}`))

	if got == nil {
		t.Fatal("OnTurnComplete did not fire")
	}
	if !strings.Contains(got.Text, creditsMsg) {
		t.Errorf("TurnResult.Text = %q, want it to carry the codex error", got.Text)
	}
	if len(streamed) != 1 || !strings.Contains(streamed[0], creditsMsg) {
		t.Errorf("OnText calls = %q, want one carrying the codex error", streamed)
	}
}

// TestTurnFailedKeepsEarlierText proves the error is appended after any text
// the turn produced before failing, separated by a blank line, so the partial
// answer is not lost.
func TestTurnFailedKeepsEarlierText(t *testing.T) {
	b := newTestBackend(t)
	var got *delegator.TurnResult
	openTurn(b, &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) { got = r }})
	b.turnMu.Lock()
	b.turnText.WriteString("partial answer")
	b.turnMu.Unlock()

	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"tu_1","status":"failed","error":{"message":"boom"}}}}`))

	if want := "partial answer\n\n" + turnFailedText("boom"); got == nil || got.Text != want {
		t.Fatalf("TurnResult = %+v, want Text %q", got, want)
	}
}

// TestTurnFailedNamedInStatusDetailUntilSuccess proves the status block names
// the last turn's failure (not just codex's bare "systemError") and that a
// later clean turn clears it.
func TestTurnFailedNamedInStatusDetailUntilSuccess(t *testing.T) {
	b := newTestBackend(t)
	b.dispatch([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread-1","status":{"type":"systemError"}}}`))
	openTurn(b, &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}})
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"tu_1","status":"failed","error":{"message":"` + creditsMsg + `"}}}}`))

	want := "sandbox=workspace-write | codex=systemError | last turn failed: " + creditsMsg
	if got := b.StatusDetail(); got != want {
		t.Fatalf("StatusDetail() after failure = %q, want %q", got, want)
	}

	openTurn(b, &delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}})
	b.dispatch([]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"tu_2","status":"completed"}}}`))
	if got := b.StatusDetail(); strings.Contains(got, "last turn failed") {
		t.Fatalf("StatusDetail() after a clean turn = %q, want the error cleared", got)
	}
}

// TestStatusErrorTruncated proves a long error is cut to one short line in the
// status block (newlines collapsed, ellipsis at the cap).
func TestStatusErrorTruncated(t *testing.T) {
	got := truncateStatusError("line one\n" + strings.Repeat("x", 300))
	if strings.Contains(got, "\n") {
		t.Errorf("truncateStatusError kept a newline: %q", got)
	}
	if n := len([]rune(got)); n != statusErrorMax || !strings.HasSuffix(got, "…") {
		t.Errorf("truncateStatusError len=%d %q, want %d runes ending in …", n, got, statusErrorMax)
	}
}

// TestTurnStartFailedCarriesCodexError proves that when turn/start itself
// returns a failed turn, the user-facing text names codex's error rather than
// the generic "turn failed", and the status block records it.
func TestTurnStartFailedCarriesCodexError(t *testing.T) {
	b := newStartableBackend(t, func(method string, _ json.RawMessage, _ int64) (json.RawMessage, error) {
		if method == "turn/start" {
			return json.RawMessage(`{"turn":{"id":"tu_1","status":"failed","error":{"message":"` + creditsMsg + `"}}}`), nil
		}
		return json.RawMessage(`{}`), nil
	})
	b.startOpts.SessionKey = "test/session"
	b.registerThread("test/session", "thread-1")

	var got *delegator.TurnResult
	err := b.beginTurn("hello", &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) { got = r }})
	if err == nil {
		t.Fatal("beginTurn: want error for a failed turn/start")
	}
	if got == nil || !strings.Contains(got.Text, creditsMsg) {
		t.Fatalf("TurnResult = %+v, want Text carrying the codex error", got)
	}
	if sd := b.StatusDetail(); !strings.Contains(sd, "last turn failed: "+creditsMsg) {
		t.Fatalf("StatusDetail() = %q, want the turn/start error", sd)
	}
}

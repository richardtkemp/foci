package codex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// #3070: the app-server connection reaching EOF mid-turn completes the turn
// as interrupted, keeping its partial text, through the real reader loop.
func TestReadStream_EOFMidTurnMarksInterrupted(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	var got *delegator.TurnResult
	applySessionAndTurn(b, &delegator.SessionEvents{}, &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	})
	b.turnMu.Lock()
	b.turnText.WriteString("partial answer")
	b.turnMu.Unlock()

	b.readStream(context.Background(), strings.NewReader(""))

	if got == nil {
		t.Fatal("OnTurnComplete never fired on EOF")
	}
	if got.Text != "partial answer" {
		t.Errorf("Text = %q, want the partial text kept", got.Text)
	}
	if !errors.Is(got.Interrupted, delegator.ErrTurnInterrupted) {
		t.Errorf("Interrupted = %v, want an error matching delegator.ErrTurnInterrupted: an EOF is not a completed turn", got.Interrupted)
	}
}

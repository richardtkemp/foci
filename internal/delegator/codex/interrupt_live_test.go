package codex

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestInterruptLive drives a REAL model turn through foci's codex Backend and
// stops it with Backend.Interrupt (#3117). It spends real tokens, so it is
// opt-in: FOCI_CODEX_BIN is the codex binary and FOCI_CODEX_LIVE_HOME a
// throwaway CODEX_HOME holding a copy of auth.json (outside /tmp: codex makes
// no helper binaries there). Never point it at the real ~/.codex.
func TestInterruptLive(t *testing.T) {
	bin, home := os.Getenv("FOCI_CODEX_BIN"), os.Getenv("FOCI_CODEX_LIVE_HOME")
	if bin == "" || home == "" {
		t.Skip("set FOCI_CODEX_BIN and FOCI_CODEX_LIVE_HOME (throwaway CODEX_HOME with auth.json) for a live, token-spending interrupt probe")
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatalf("FOCI_CODEX_LIVE_HOME has no auth.json: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw, _ := newFromConfig(map[string]any{"binary": bin})
	b := raw.(*Backend)
	opts := delegator.StartOptions{AgentID: t.Name(), SessionKey: "test/interrupt", WorkDir: t.TempDir(),
		SystemPrompt: "Live interrupt probe.", Env: map[string]string{"CODEX_HOME": home}}
	if err := b.Start(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	var deltas atomic.Int64
	b.AttachSessionEvents(&delegator.SessionEvents{OnText: func(string) { deltas.Add(1) }})
	done := make(chan *delegator.TurnResult, 1)
	turn := &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) { done <- r }}
	start := time.Now()
	if err := b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceUser, Turn: turn,
		Text: "Write a 4000-word essay on the history of bridges. Do not use any tools."}); err != nil {
		t.Fatal(err)
	}
	// Wait until the model is visibly producing output, so the turn is mid-work.
	for deltas.Load() < 5 {
		select {
		case r := <-done:
			t.Fatalf("turn completed before the interrupt: %q", r.Text)
		case <-ctx.Done():
			t.Fatal("no text deltas within the deadline")
		case <-time.After(100 * time.Millisecond):
		}
	}
	turnID := b.currentTurnID()
	if turnID == "" {
		t.Fatal("running turn has no recorded turnId")
	}
	stopAt := time.Now()
	if err := b.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt = %v", err)
	}
	select {
	case r := <-done:
		t.Logf("codex %s: turn %s stopped %.2fs after Interrupt (%.1fs into the turn, %d deltas, %d chars kept)",
			b.version(), turnID, time.Since(stopAt).Seconds(), stopAt.Sub(start).Seconds(), deltas.Load(), len(r.Text))
	case <-time.After(15 * time.Second):
		t.Fatal("turn still running 15s after Interrupt")
	}
	if got := b.currentTurnID(); got != "" {
		t.Errorf("turnId after the stopped turn = %q, want empty", got)
	}
	// No running turn now: a second Interrupt is a clean no-op.
	if err := b.Interrupt(ctx); err != nil {
		t.Errorf("Interrupt with no running turn = %v, want nil", err)
	}
}

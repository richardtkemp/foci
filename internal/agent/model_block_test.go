package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"foci/internal/delegator"
)

const fableCredits = "Fable 5 requires usage credits. Switch to another model, or manage usage credits at https://claude.ai/settings/usage, to continue."

// TestModelBlock_RefusesTurnsUntilModelChanges is #3106 (Dick 2026-11-14,
// option 1): once the backend reports the session's model needs usage
// credits, no turn on that session reaches the backend, user or background,
// until the session's model changes; the block is reported fresh once, so the
// notice posts once.
func TestModelBlock_RefusesTurnsUntilModelChanges(t *testing.T) {
	t.Parallel()
	f := newEntrySplitFixture(t)
	f.completeTurns()
	sent := 0
	inner := f.be.sendToPaneFn
	f.be.sendToPaneFn = func(ctx context.Context, p string, h *mockHandler) (*delegator.TurnResult, error) {
		sent++
		return inner(ctx, p, h)
	}

	if !f.a.BlockSessionModel(entrySplitKey, fableCredits) {
		t.Fatal("first block not reported fresh")
	}
	if f.a.BlockSessionModel(entrySplitKey, fableCredits) {
		t.Error("repeated block reported fresh: the notice would post twice")
	}

	for _, trigger := range []string{"voice", TriggerKeepalive, "scheduled_wake"} {
		err := f.a.HandleMessage(WithTrigger(context.Background(), trigger), entrySplitKey, []string{"go"}, nil)
		var mb *ModelBlockedError
		if !errors.As(err, &mb) {
			t.Fatalf("%s: HandleMessage = %v, want ModelBlockedError", trigger, err)
		}
		if !strings.Contains(err.Error(), "/model") {
			t.Errorf("%s: refusal %q does not say how to switch model", trigger, err)
		}
	}
	if sent != 0 {
		t.Fatalf("backend got %d turns while the model was blocked, want 0", sent)
	}
	f.assertNoTurnRan(t)

	// Another session of the agent is not blocked.
	if _, ok := f.a.SessionModelBlocked("test/other"); ok {
		t.Error("the block reached another session")
	}

	// /model (any model change) lifts it.
	f.a.SetSessionModel(entrySplitKey, "anthropic/claude-opus-5", "", "", nil)
	if err := f.a.HandleMessage(WithTrigger(context.Background(), "voice"), entrySplitKey, []string{"go"}, nil); err != nil {
		t.Fatalf("after a model change: HandleMessage = %v, want nil", err)
	}
	if sent != 1 {
		t.Errorf("backend got %d turns after the model change, want 1", sent)
	}
	if !f.a.BlockSessionModel(entrySplitKey, fableCredits) {
		t.Error("a block after the lift not reported fresh")
	}
}

func TestModelBlockNotice_SaysSwitchModel(t *testing.T) {
	n := ModelBlockNotice(fableCredits)
	for _, want := range []string{"Fable 5 requires usage credits", "/model"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice %q missing %q", n, want)
		}
	}
}

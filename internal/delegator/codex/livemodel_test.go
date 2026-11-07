package codex

import (
	"testing"

	"foci/internal/delegator"
)

// TestLiveModel proves a spawn's backend can name its model from the thread start
// response, before any turn ends, in TurnResult.Model's spelling (#2879).
func TestLiveModel(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	lr, ok := delegator.As[delegator.LiveModelReporter](b)
	if !ok {
		t.Fatal("codex must implement LiveModelReporter")
	}
	if got := lr.LiveModel(); got != "" {
		t.Errorf("no thread yet: LiveModel = %q, want empty", got)
	}
	b.model = "gpt-6.1-sol"
	if got := lr.LiveModel(); got != "codex/gpt-6.1-sol" {
		t.Errorf("after thread start: LiveModel = %q, want codex/gpt-6.1-sol", got)
	}
}

// TestLiveModel_ChosenModelNotYetSent: between a choice of model and the
// turn/start that sends it, b.model names the old one, so LiveModel says nothing
// and the caller falls back to the session's own choice.
func TestLiveModel_ChosenModelNotYetSent(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	b.model = "gpt-old"
	b.pendingModel = "gpt-new"
	b.modelSwitchPending = true
	if got := b.LiveModel(); got != "" {
		t.Errorf("switch pending: LiveModel = %q, want empty", got)
	}
}

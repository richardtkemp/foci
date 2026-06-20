package agent

import (
	"context"
	"testing"
)

// TestBounceSessionIfPromptChanged covers the optimisation over the
// unconditional #828 post-compaction bounce: the reload-bounce fires only when
// the system prompt rebuilt from disk differs from the one the running CC
// session launched with. Unchanged prompt → no restart, no flow interruption.
func TestBounceSessionIfPromptChanged(t *testing.T) {
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)
	const sk = "test-agent/c1/1000"

	prompt := "system prompt v1 — character files and skill list"
	mgr.StartOpts.SystemPromptFunc = func() string { return prompt }

	if _, err := mgr.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}

	t.Run("unchanged prompt skips bounce", func(t *testing.T) {
		if mgr.BounceSessionIfPromptChanged(sk) {
			t.Errorf("bounced despite an unchanged prompt")
		}
		if _, ok := mgr.getManaged(sk); !ok {
			t.Errorf("backend unmapped after a skipped bounce")
		}
	})

	t.Run("changed prompt bounces", func(t *testing.T) {
		prompt = "system prompt v2 — a skill was added"
		if !mgr.BounceSessionIfPromptChanged(sk) {
			t.Errorf("did not bounce despite a changed prompt")
		}
		if _, ok := mgr.getManaged(sk); ok {
			t.Errorf("backend still mapped after a bounce")
		}
	})

	t.Run("no live backend is a no-op", func(t *testing.T) {
		if mgr.BounceSessionIfPromptChanged("test-agent/c1/9999") {
			t.Errorf("bounced for a session with no backend")
		}
	})
}

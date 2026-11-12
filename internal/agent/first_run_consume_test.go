package agent

import "testing"

// TestConsumeFirstRunMessage_OnceAndHook verifies the onboarding message is
// returned and cleared exactly once, and OnFirstRunConsumed fires only on the
// real consumption — the contract both turn paths rely on (#853).
func TestConsumeFirstRunMessage_OnceAndHook(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	fired := 0
	a.OnFirstRunConsumed = func() { fired++ }
	a.FirstRunMessage.Store("[FIRST RUN] hello")

	got := a.consumeFirstRunMessage()
	if got != "[FIRST RUN] hello" {
		t.Fatalf("first consume = %q; want the stored onboarding", got)
	}
	if fired != 1 {
		t.Fatalf("OnFirstRunConsumed fired %d times; want 1", fired)
	}

	// Second consume: nothing left, hook must not fire again.
	if got := a.consumeFirstRunMessage(); got != "" {
		t.Fatalf("second consume = %q; want empty", got)
	}
	if fired != 1 {
		t.Fatalf("OnFirstRunConsumed fired %d times after second consume; want 1", fired)
	}
}

// TestConsumeFirstRunMessage_NonePending verifies a no-op when no onboarding
// was stored: returns "" and never fires the hook.
func TestConsumeFirstRunMessage_NonePending(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	fired := false
	a.OnFirstRunConsumed = func() { fired = true }

	if got := a.consumeFirstRunMessage(); got != "" {
		t.Fatalf("consume with nothing stored = %q; want empty", got)
	}
	if fired {
		t.Fatal("OnFirstRunConsumed fired with no pending onboarding")
	}
}

// TestConsumeFirstRunMessage_Addendum verifies the FirstRunAddendum hook runs
// only on the winning consume and its result is appended to the onboarding,
// so the install-level import is decided at delivery (#3049).
func TestConsumeFirstRunMessage_Addendum(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	calls := 0
	a.FirstRunAddendum = func() string { calls++; return "[IMPORT] extra" }
	a.FirstRunMessage.Store("[FIRST RUN] hello")

	if got := a.consumeFirstRunMessage(); got != "[FIRST RUN] hello\n\n[IMPORT] extra" {
		t.Fatalf("consume = %q; want onboarding plus addendum", got)
	}
	if got := a.consumeFirstRunMessage(); got != "" || calls != 1 {
		t.Fatalf("second consume = %q, addendum calls = %d; want empty and 1", got, calls)
	}

	// An empty addendum leaves the onboarding text exactly as stored.
	b := &Agent{}
	b.FirstRunAddendum = func() string { return "" }
	b.FirstRunMessage.Store("[FIRST RUN] hello")
	if got := b.consumeFirstRunMessage(); got != "[FIRST RUN] hello" {
		t.Fatalf("consume with empty addendum = %q; want the onboarding unchanged", got)
	}
}

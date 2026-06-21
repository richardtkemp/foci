package agent

import "testing"

// TestConsumeFirstRunMessage_OnceAndHook verifies the onboarding message is
// returned and cleared exactly once, and OnFirstRunConsumed fires only on the
// real consumption — the contract both turn paths rely on (#853).
func TestConsumeFirstRunMessage_OnceAndHook(t *testing.T) {
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

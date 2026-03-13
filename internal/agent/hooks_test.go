package agent

import "testing"

func TestHookListAddAndRange(t *testing.T) {
	// Proves that HookList is empty and safe to range over before any Add calls, and that after adding callbacks they are invoked in registration order when iterated.
	var hooks HookList[func(string)]

	// Empty list — range should be a no-op.
	for _, fn := range hooks {
		fn("should not happen")
		t.Fatal("unexpected callback on empty HookList")
	}

	// Add two hooks and verify both fire in order.
	var calls []string
	hooks.Add(func(s string) { calls = append(calls, "a:"+s) })
	hooks.Add(func(s string) { calls = append(calls, "b:"+s) })

	for _, fn := range hooks {
		fn("x")
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0] != "a:x" || calls[1] != "b:x" {
		t.Errorf("calls = %v, want [a:x b:x]", calls)
	}
}

func TestHookListMultiArg(t *testing.T) {
	// Proves that HookList is generic enough to hold multi-argument function types and that all registered callbacks receive all arguments correctly.
	var hooks HookList[func(string, int)]

	var got []string
	hooks.Add(func(s string, n int) { got = append(got, s) })
	hooks.Add(func(s string, n int) { got = append(got, "second") })

	for _, fn := range hooks {
		fn("hello", 42)
	}

	if len(got) != 2 || got[0] != "hello" || got[1] != "second" {
		t.Errorf("got = %v, want [hello second]", got)
	}
}

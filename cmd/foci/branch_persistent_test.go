package main

import "testing"

// TestBranchPersistentFlagForwarded checks the public CLI flag reaches the HTTP body, not message text.
func TestBranchPersistentFlagForwarded(t *testing.T) {
	body := captureBody(t, func(base string) error {
		return cmdBranch(base, []string{"-a", "codex", "--persistent", "one-off task"})
	})
	if body["persistent"] != true || body["text"] != "one-off task" {
		t.Fatalf("body=%v", body)
	}
}

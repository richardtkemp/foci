package command

import (
	"context"
	"strings"
	"testing"
)

// TestHelpTableMentionsSteerPrefixes proves /help teaches the two
// delivery-mode prefixes (#1318): they are not registry commands (the word
// is stripped and the rest is delivered with that mode), so the help table
// appends the one-line usage (the same constant the bots' bare-prefix
// replies use). The literal string — not the shared constant — keeps this
// test compilable against the pre-change tree, so it is red by assertion.
func TestHelpTableMentionsSteerPrefixes(t *testing.T) {
	const usage = "Usage: /queue <message> — wait for the current turn to finish; /steer <message> — redirect the agent now."

	reg := NewRegistry()
	reg.Register(PingCommand())

	got := helpTable(context.Background(), reg, Request{}, CommandContext{})

	if !strings.Contains(got, usage) {
		t.Errorf("help table does not teach the steer prefixes:\n%s", got)
	}
	if !strings.HasSuffix(got, usage) {
		t.Errorf("help table footer should BE the usage line (nothing after it):\n%q", got)
	}
	if strings.Contains(got, "| /steer") || strings.Contains(got, "| /queue") {
		t.Errorf("the prefixes must not appear as registry command rows:\n%s", got)
	}
}

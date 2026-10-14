package command

import (
	"context"
	"strings"
	"testing"
)

// TestHelpTableSteerPrefixFooter proves /help teaches the two delivery-mode
// prefixes (#1318) exactly where users can type them: the chat surfaces whose
// receive paths parse the prefixes (telegram, discord) get the one-line usage
// footer; every other surface must not be taught a syntax it does not honour —
// the app and HTTP set the mode as a field and answer the bare /steer|/queue
// words with "Unknown command", so a footer there would advertise a dead
// syntax. An unidentified source ("") fails the same safe way. The literal
// strings — not the shared constants — keep this test red-by-assertion against
// the pre-change tree, where the footer was unconditional.
func TestHelpTableSteerPrefixFooter(t *testing.T) {
	const usage = "Usage: /queue <message> — wait for the current turn to finish; /steer <message> — redirect the agent now."

	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"telegram", true},
		{"discord", true},
		{"app", false},
		{"http", false},
		{"", false},
	} {
		reg := NewRegistry()
		reg.Register(PingCommand())
		got := helpTable(context.Background(), reg, Request{Source: tc.source}, CommandContext{})
		if strings.Contains(got, usage) != tc.want {
			t.Errorf("source %q: footer present = %v, want %v:\n%s", tc.source, !tc.want, tc.want, got)
		}
		if tc.want && !strings.HasSuffix(got, usage) {
			t.Errorf("source %q: footer should BE the usage line (nothing after it):\n%q", tc.source, got)
		}
		if strings.Contains(got, "| /steer") || strings.Contains(got, "| /queue") {
			t.Errorf("source %q: the prefixes must not appear as registry command rows:\n%s", tc.source, got)
		}
	}
}

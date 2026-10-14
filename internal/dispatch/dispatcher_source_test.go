package dispatch

import (
	"context"
	"strings"
	"testing"

	"foci/internal/command"
)

// TestDispatchTextStampsSourceOnHelp proves the dispatcher stamps its
// platform on every dispatched request (#1318): /help rendered through a
// telegram- or discord-sourced dispatcher teaches the /steer|/queue chat
// prefixes (only those receive paths parse them), while the same dispatcher
// built with any other source does not — the footer follows the surface the
// command actually arrived on, not the registry that renders it.
func TestDispatchTextStampsSourceOnHelp(t *testing.T) {
	const usage = "/queue <message>"
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
		reg := command.NewRegistry()
		reg.Register(command.HelpCommand(reg))
		d := NewDispatcher(reg, command.CommandContext{}, "agent1", tc.source)

		result := d.DispatchText(context.Background(), "/help", 123, "user1")
		if !result.Handled {
			t.Fatal("expected /help to be handled")
		}
		if strings.Contains(result.Response.Text, usage) != tc.want {
			t.Errorf("source %q: /help teaches the chat prefixes = %v, want %v", tc.source, !tc.want, tc.want)
		}
	}
}

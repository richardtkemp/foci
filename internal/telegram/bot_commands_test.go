package telegram

import (
	"context"
	"fmt"
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
)

func TestRegisterCommands(t *testing.T) {
	// Verifies that RegisterCommands properly registers
	// commands with the Telegram API, including stop (visible) and done (hidden).
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "help", Description: "List available commands"})
	cmds.Register(&command.Command{Name: "ping", Description: "Check bot health"})
	cmds.Register(&command.Command{Name: "status", Description: "Show agent status"})
	cmds.Register(command.StopCommand())
	cmds.Register(command.DoneCommand()) // hidden — should be excluded

	b, mock := testBot([]string{"111"}, cmds)
	b.RegisterCommands()

	if mock.setCmds == nil {
		t.Fatal("SetMyCommands was not called")
	}

	// 3 user commands + stop (visible). Done is hidden → excluded.
	if len(mock.setCmds) != 4 {
		t.Fatalf("expected 4 commands, got %d", len(mock.setCmds))
	}

	// Registry commands sorted by name: help, ping, status, stop
	names := make([]string, len(mock.setCmds))
	for i, c := range mock.setCmds {
		names[i] = c.Command
	}
	wantOrder := []string{"help", "ping", "status", "stop"}
	for i, want := range wantOrder {
		if names[i] != want {
			t.Errorf("command[%d] = %q, want %q", i, names[i], want)
		}
	}

	// Verify descriptions
	for _, c := range mock.setCmds {
		if c.Description == "" {
			t.Errorf("command %q has empty description", c.Command)
		}
	}
}

func TestRegisterCommands_EmptyDescription(t *testing.T) {
	// Verifies that RegisterCommands falls
	// back to the command name when description is empty.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "test", Description: ""})

	b, mock := testBot([]string{"111"}, cmds)
	b.RegisterCommands()

	// Should fall back to command name as description
	for _, c := range mock.setCmds {
		if c.Command == "test" && c.Description != "test" {
			t.Errorf("expected description fallback to name, got %q", c.Description)
		}
	}
}

func TestRegisterCommands_APIError(t *testing.T) {
	// Verifies that RegisterCommands handles API
	// errors gracefully without panicking.
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "help", Description: "List commands"})

	b, mock := testBot([]string{"111"}, cmds)
	mock.setCmdsErr = fmt.Errorf("telegram API error")

	// Should not panic — just logs a warning
	b.RegisterCommands()
}

func TestSendReply_SkipsEmptyText(t *testing.T) {
	// sendReply trims parts and skips empty text — callers don't need to guard.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	msg := makeMsg(111, "owner", "hello")

	b.sendReply(msg, "")
	if mock.sentCount() != 0 {
		t.Errorf("sends = %d, want 0 (sendReply skips empty text)", mock.sentCount())
	}
}

func TestSendNotification_EmptyTextSkipped(t *testing.T) {
	// Verifies that SendNotification skips
	// empty or whitespace-only text.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	b.SetChatID(12345)

	// Empty text should not send
	b.SendNotification("")
	b.SendNotification("   ")
	b.SendNotification("\n\t")

	if mock.sentCount() != 0 {
		t.Errorf("sends = %d, want 0 (empty text should be skipped)", mock.sentCount())
	}

	// Non-empty text should send
	b.SendNotification("test alert")
	if mock.sentCount() != 1 {
		t.Errorf("sends = %d, want 1", mock.sentCount())
	}
}

// TestRegisterCommands_OnlyAvailable pins #898 for the Telegram menu: once the
// bot is wired to an agent, the menu lists only the commands that agent can run.
// A RequiresBackend command on an agent without a delegated backend, and one
// whose Visible gate says no, used to be advertised anyway.
func TestRegisterCommands_OnlyAvailable(t *testing.T) {
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{Name: "help", Description: "List commands"})
	cmds.Register(&command.Command{Name: "login", Description: "CC only", Requires: command.RequiresBackend})
	cmds.Register(&command.Command{Name: "gated", Description: "never", Visible: func(context.Context, command.Request, command.CommandContext) bool { return false }})
	cmds.Register(&command.Command{Name: "pause", Description: "app has buttons", ExcludeApp: true})

	b, mock := testBot([]string{"111"}, cmds)
	b.SetCommandContext(command.CommandContext{Agent: &agent.Agent{}})
	b.RegisterCommands()

	var got []string
	for _, c := range mock.setCmds {
		got = append(got, c.Command)
	}
	if len(got) != 2 || got[0] != "help" || got[1] != "pause" {
		t.Errorf("menu = %v, want [help pause]", got)
	}
}

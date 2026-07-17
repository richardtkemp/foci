package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/tools"
)

// TestPingCommand verifies that the ping command returns a response starting with "pong ".
func TestPingCommand(t *testing.T) {
	cmd := PingCommand()
	result, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(result.Text, "pong ") {
		t.Errorf("result = %q, want prefix 'pong '", result.Text)
	}
}

// TestVersionCommand verifies that version info is rendered correctly.
func TestVersionCommand(t *testing.T) {
	cmd := VersionCommand()
	cc := CommandContext{
		BuildInfo: BuildInfo{
			Version:   "1.0.0",
			GoVersion: "go1.22",
			GitCommit: "abc123",
			BuildTime: "2026-02-21",
		},
	}

	result, _ := cmd.Execute(context.Background(), Request{}, cc)
	if !strings.Contains(result.Text, "1.0.0") || !strings.Contains(result.Text, "abc123") {
		t.Errorf("result = %q", result.Text)
	}
}

// TestHelpCommand verifies help output includes all categories and commands with correct ordering.
func TestHelpCommand(t *testing.T) {
	reg := NewRegistry()
	reg.Register(PingCommand())
	reg.Register(CacheCommand())
	reg.Register(VersionCommand())
	reg.Register(&Command{Name: "custom", Description: "Custom thing",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{}, nil
		}})
	reg.Register(&Command{Name: "hidden", Description: "Hidden cmd", Hidden: true,
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			return Response{}, nil
		}})
	reg.Register(HelpCommand(reg))

	cmd := reg.Get("help")
	result, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Check category headers appear
	for _, header := range []string{"Observability", "Diagnostics", "Session"} {
		if !strings.Contains(result.Text, header) {
			t.Errorf("missing category header %q in:\n%s", header, result.Text)
		}
	}
	// Commands present
	if !strings.Contains(result.Text, "/ping") {
		t.Errorf("missing /ping in help output:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "/cache") {
		t.Errorf("missing /cache in help output:\n%s", result.Text)
	}
	// Uncategorized goes to Other
	if !strings.Contains(result.Text, "Other") {
		t.Errorf("missing Other group in help output:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "/custom") {
		t.Errorf("missing /custom in help output:\n%s", result.Text)
	}
	// Hidden should NOT appear
	if strings.Contains(result.Text, "/hidden") {
		t.Errorf("hidden command should not appear in help:\n%s", result.Text)
	}
	// Rendered as a markdown pipe table with Command/Description columns.
	if !strings.Contains(result.Text, "| Command | Description |") {
		t.Errorf("missing table header in help output:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "| --- | --- |") {
		t.Errorf("missing table separator in help output:\n%s", result.Text)
	}
	// Category header rows carry emoji + bold label, empty description cell.
	if !strings.Contains(result.Text, "| **📊 Observability** |  |") {
		t.Errorf("missing observability section row in help output:\n%s", result.Text)
	}
}

// TestToolsCommand verifies tools list renders with all registered tools.
func TestToolsCommand(t *testing.T) {
	cmd := ToolsCommand()
	reg := tools.NewRegistry()
	reg.Register(&tools.Tool{Name: "shell", Description: "Run commands"})
	reg.Register(&tools.Tool{Name: "read", Description: "Read files"})
	cc := CommandContext{ToolsRegistry: reg}

	result, _ := cmd.Execute(context.Background(), Request{}, cc)
	if !strings.Contains(result.Text, "| Name |") {
		t.Errorf("missing pipe table header:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "shell") || !strings.Contains(result.Text, "read") {
		t.Errorf("missing tools in:\n%s", result.Text)
	}
}

// TestToolsCommandEmpty verifies empty tools list renders appropriate message.
func TestToolsCommandEmpty(t *testing.T) {
	cmd := ToolsCommand()
	result, _ := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if result.Text != "No tools registered." {
		t.Errorf("result = %q", result.Text)
	}
}

// TestAgentsCommand verifies the agents list renders id, session, model, and
// message count for each agent (the status column was removed).
func TestAgentsCommand(t *testing.T) {
	cmd := AgentsCommand()
	cc := CommandContext{
		AgentListFn: func() []AgentInfo {
			return []AgentInfo{
				{ID: "main", SessionKey: "agent:main:main", Model: "opus-4", MessageCount: 31},
				{ID: "scout", SessionKey: "agent:scout:main", Model: "haiku-4", MessageCount: 12},
			}
		},
	}

	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "Agents") {
		t.Errorf("missing header in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "ID") || !strings.Contains(result.Text, "Session") || !strings.Contains(result.Text, "Messages") {
		t.Errorf("missing table headers in:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "Status") {
		t.Errorf("status column should be gone, found it in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator line in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "agent:main:main") || !strings.Contains(result.Text, "agent:scout:main") {
		t.Errorf("missing sessions in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "opus-4") || !strings.Contains(result.Text, "31") {
		t.Errorf("missing model/message-count in:\n%s", result.Text)
	}
}

// TestAgentsCommandNoSession verifies agents without sessions show placeholder dashes.
func TestAgentsCommandNoSession(t *testing.T) {
	cmd := AgentsCommand()
	cc := CommandContext{
		AgentListFn: func() []AgentInfo {
			return []AgentInfo{
				{ID: "clutch", SessionKey: "agent:clutch:main", Model: "opus-4", MessageCount: 31},
				{ID: "scout", SessionKey: "", Model: "", MessageCount: 0},
			}
		},
	}

	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "—") {
		t.Errorf("expected dash for no-session agent in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "clutch") {
		t.Errorf("missing clutch agent in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "scout") {
		t.Errorf("missing scout agent in:\n%s", result.Text)
	}
}

// TestAgentsCommandEmpty verifies empty agents list renders appropriate message.
func TestAgentsCommandEmpty(t *testing.T) {
	cmd := AgentsCommand()
	result, _ := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if result.Text != "No agents configured." {
		t.Errorf("result = %q", result.Text)
	}
}

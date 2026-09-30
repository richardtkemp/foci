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

// TestHelpCommand_WithArg verifies /help <command> (#1551): the named
// description, aliases and subcommands come from the registry; a registered
// command the table hides is still described (with why it is unavailable);
// only a name the registry lacks is "Unknown", followed by the full table.
func TestHelpCommand_WithArg(t *testing.T) {
	noop := func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }
	reg := NewRegistry()
	reg.Register(&Command{Name: "widget", Aliases: []string{"wd"}, Description: "Manage widgets",
		Subcommands: []Subcommand{
			{Name: "list", Description: "List every widget", Execute: noop},
			{Name: "add", Description: "Add a widget by name", Hidden: true, Execute: noop},
			{Name: "secret", Description: "Never shown", Execute: noop,
				Visible: func(context.Context, CommandContext) bool { return false }},
		}})
	reg.Register(&Command{Name: "plain", Description: "A plain command", Execute: noop})
	reg.Register(&Command{Name: "hidden", Description: "Hidden cmd", Hidden: true, Execute: noop})
	reg.Register(&Command{Name: "cconly", Description: "Backend-only cmd", Requires: RequiresBackend, Execute: noop})
	reg.Register(&Command{Name: "offmodel", Description: "Model-gated cmd", Execute: noop,
		Visible: func(context.Context, Request, CommandContext) bool { return false }})
	reg.Register(HelpCommand(reg))
	help := reg.Get("help")

	run := func(args string) string {
		t.Helper()
		resp, err := help.Execute(context.Background(), Request{Name: "help", Args: args}, CommandContext{})
		if err != nil {
			t.Fatalf("Execute(%q): %v", args, err)
		}
		return resp.Text
	}

	full := run("")

	for _, args := range []string{"widget", "/widget", "WIDGET", "wd"} {
		got := run(args)
		for _, want := range []string{"/widget", "Manage widgets", "/wd",
			"| list | List every widget |", "| add | Add a widget by name |"} {
			if !strings.Contains(got, want) {
				t.Errorf("/help %s: missing %q in:\n%s", args, want, got)
			}
		}
		if strings.Contains(got, "secret") {
			t.Errorf("/help %s: invisible subcommand shown:\n%s", args, got)
		}
		if strings.Contains(got, "/plain") {
			t.Errorf("/help %s: should show only the named command, got:\n%s", args, got)
		}
	}

	if got := run("plain"); !strings.Contains(got, "/plain") || !strings.Contains(got, "A plain command") ||
		strings.Contains(got, "Subcommand") {
		t.Errorf("/help plain: want description and no subcommand table, got:\n%s", got)
	}

	got := run("nosuch")
	if !strings.Contains(got, "Unknown command /nosuch") || !strings.Contains(got, full) {
		t.Errorf("/help nosuch: want unknown-command notice plus full table, got:\n%s", got)
	}

	// Registered but hidden from the table: described, never "Unknown".
	// CommandContext{} has no Agent, so RequiresBackend fails with its reason.
	for _, tc := range []struct{ args, desc, avail string }{
		{"hidden", "Hidden cmd", ""},
		{"cconly", "Backend-only cmd", "Not available on this agent: /cconly requires a Claude Code backend."},
		{"offmodel", "Model-gated cmd", "Not available on this agent in its current configuration"},
	} {
		got := run(tc.args)
		if strings.Contains(got, "Unknown command") || strings.Contains(got, full) {
			t.Errorf("/help %s: registered command reported as unknown:\n%s", tc.args, got)
		}
		if !strings.Contains(got, "/"+tc.args) || !strings.Contains(got, tc.desc) {
			t.Errorf("/help %s: missing name/description in:\n%s", tc.args, got)
		}
		if tc.avail == "" && strings.Contains(got, "Not available") {
			t.Errorf("/help %s: available command marked unavailable:\n%s", tc.args, got)
		}
		if tc.avail != "" && !strings.Contains(got, tc.avail) {
			t.Errorf("/help %s: missing %q in:\n%s", tc.args, tc.avail, got)
		}
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

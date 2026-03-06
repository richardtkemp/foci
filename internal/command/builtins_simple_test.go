package command

import (
	"context"
	"strings"
	"testing"
)

// TestPingCommand verifies that the ping command returns a response starting with "pong ".
func TestPingCommand(t *testing.T) {
	cmd := NewPingCommand()
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(result, "pong ") {
		t.Errorf("result = %q, want prefix 'pong '", result)
	}
}

// TestResetCommand verifies that reset invokes the reset callback and returns confirmation.
func TestResetCommand(t *testing.T) {
	cleared := false
	cmd := NewResetCommand(func() error {
		cleared = true
		return nil
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !cleared {
		t.Error("reset function not called")
	}
	if result != "Session cleared." {
		t.Errorf("result = %q", result)
	}
}

// TestVersionCommand verifies that version info is rendered correctly.
func TestVersionCommand(t *testing.T) {
	cmd := NewVersionCommand(BuildInfo{
		Version:   "1.0.0",
		GoVersion: "go1.22",
		GitCommit: "abc123",
		BuildTime: "2026-02-21",
	})

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "1.0.0") || !strings.Contains(result, "abc123") {
		t.Errorf("result = %q", result)
	}
}

// TestHelpCommand verifies help output includes all categories and commands with correct ordering.
func TestHelpCommand(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewPingCommand())                                      // category: session
	reg.Register(NewCacheCommand("/dev/null"))                          // category: observability
	reg.Register(NewResetCommand(func() error { return nil }))          // category: operations
	reg.Register(NewVersionCommand(BuildInfo{Version: "1.0"}))          // category: diagnostics
	reg.Register(&Command{Name: "custom", Description: "Custom thing"}) // no category
	reg.Register(&Command{Name: "hidden", Description: "Hidden cmd", Hidden: true})
	reg.Register(NewHelpCommand(reg))

	cmd := reg.Get("help")
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Check category headers appear
	for _, header := range []string{"Observability", "Operations", "Diagnostics", "Session"} {
		if !strings.Contains(result, header) {
			t.Errorf("missing category header %q in:\n%s", header, result)
		}
	}
	// Commands present
	if !strings.Contains(result, "/ping") {
		t.Errorf("missing /ping in help output:\n%s", result)
	}
	if !strings.Contains(result, "/cache") {
		t.Errorf("missing /cache in help output:\n%s", result)
	}
	// Uncategorized goes to Other
	if !strings.Contains(result, "Other") {
		t.Errorf("missing Other group in help output:\n%s", result)
	}
	if !strings.Contains(result, "/custom") {
		t.Errorf("missing /custom in help output:\n%s", result)
	}
	// Hidden should NOT appear
	if strings.Contains(result, "/hidden") {
		t.Errorf("hidden command should not appear in help:\n%s", result)
	}
	// Check category ordering: Observability before Operations before Diagnostics before Session
	obsIdx := strings.Index(result, "Observability")
	opsIdx := strings.Index(result, "Operations")
	diagIdx := strings.Index(result, "Diagnostics")
	sessIdx := strings.Index(result, "Session")
	if obsIdx > opsIdx || opsIdx > diagIdx || diagIdx > sessIdx {
		t.Errorf("categories not in expected order:\n%s", result)
	}
}

// TestToolsCommand verifies tools list renders with all registered tools.
func TestToolsCommand(t *testing.T) {
	cmd := NewToolsCommand(func() []ToolInfo {
		return []ToolInfo{
			{Name: "shell", Description: "Run commands"},
			{Name: "read", Description: "Read files"},
		}
	})

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "| Name |") {
		t.Errorf("missing pipe table header:\n%s", result)
	}
	if !strings.Contains(result, "shell") || !strings.Contains(result, "read") {
		t.Errorf("missing tools in:\n%s", result)
	}
}

// TestToolsCommandEmpty verifies empty tools list renders appropriate message.
func TestToolsCommandEmpty(t *testing.T) {
	cmd := NewToolsCommand(func() []ToolInfo { return nil })
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No tools registered." {
		t.Errorf("result = %q", result)
	}
}

// TestAgentsCommand verifies agents list renders with all agent info and statuses.
func TestAgentsCommand(t *testing.T) {
	cmd := NewAgentsCommand(func() []AgentInfo {
		return []AgentInfo{
			{ID: "main", SessionKey: "agent:main:main", Model: "opus-4", Busy: false, MessageCount: 31},
			{ID: "scout", SessionKey: "agent:scout:main", Model: "haiku-4", Busy: true, MessageCount: 12},
		}
	}, nil, nil)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Agents") {
		t.Errorf("missing header in:\n%s", result)
	}
	if !strings.Contains(result, "ID") || !strings.Contains(result, "Session") || !strings.Contains(result, "Messages") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "---") {
		t.Errorf("missing separator line in:\n%s", result)
	}
	if !strings.Contains(result, "agent:main:main") {
		t.Errorf("missing main session in:\n%s", result)
	}
	if !strings.Contains(result, "agent:scout:main") {
		t.Errorf("missing scout session in:\n%s", result)
	}
	if !strings.Contains(result, "idle") {
		t.Errorf("missing idle status in:\n%s", result)
	}
	if !strings.Contains(result, "busy") {
		t.Errorf("missing busy status in:\n%s", result)
	}
}

// TestAgentsCommandNoSession verifies agents without sessions show placeholder dashes.
func TestAgentsCommandNoSession(t *testing.T) {
	cmd := NewAgentsCommand(func() []AgentInfo {
		return []AgentInfo{
			{ID: "clutch", SessionKey: "agent:clutch:main", Model: "opus-4", MessageCount: 31},
			{ID: "scout", SessionKey: "", Model: "", MessageCount: 0},
		}
	}, nil, nil)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Agent with no session should show "—" placeholders
	if !strings.Contains(result, "—") {
		t.Errorf("expected dash for no-session agent in:\n%s", result)
	}
	if !strings.Contains(result, "clutch") {
		t.Errorf("missing clutch agent in:\n%s", result)
	}
	if !strings.Contains(result, "scout") {
		t.Errorf("missing scout agent in:\n%s", result)
	}
}

// TestAgentsCommandEmpty verifies empty agents list renders appropriate message.
func TestAgentsCommandEmpty(t *testing.T) {
	cmd := NewAgentsCommand(func() []AgentInfo { return nil }, nil, nil)
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No agents configured." {
		t.Errorf("result = %q", result)
	}
}

package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/tools"
)

// TestFacetCommand verifies facet calls ConfigureFacet and ConnMgr.
// Note: full facet testing requires platform integration; this tests the command shell.
func TestFacetCommand(t *testing.T) {
	cmd := FacetCommand()
	// Without ConnMgr, the command should fail
	cc := CommandContext{
		Agent:       &agent.Agent{},
		AgentConfig: config.AgentConfig{ID: "test"},
	}

	_, err := cmd.Execute(context.Background(), Request{}, cc)
	if err == nil {
		t.Fatal("expected error without ConnMgr")
	}
}

// TestFacetCommand_AppPath verifies an app-sourced facet forks the session and
// surfaces it via MintFacetConversation, returning the new conversation to
// foreground on the requesting device (no facet-bot pool involved).
func TestFacetCommand_AppPath(t *testing.T) {
	ag := &agent.Agent{Sessions: session.NewStore(t.TempDir())}
	noCompact := false
	var gotAgent, gotSession string
	cc := CommandContext{
		Agent:       ag,
		Sessions:    ag.Sessions,
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{ID: "test", Sessions: config.AgentSessionsOverride{CompactionConfig: config.CompactionConfig{FacetNoCompact: &noCompact}}},
		MintFacetConversation: func(agentID, sessionKey string) (string, error) {
			gotAgent, gotSession = agentID, sessionKey
			return "conv-xyz", nil
		},
	}
	ctx := tools.WithSessionKey(context.Background(), "test/c1")

	resp, err := FacetCommand().Execute(ctx, Request{Source: "app"}, cc)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.OpenConversationID != "conv-xyz" {
		t.Errorf("OpenConversationID = %q, want conv-xyz", resp.OpenConversationID)
	}
	if gotAgent != "test" {
		t.Errorf("mint agentID = %q, want test", gotAgent)
	}
	if gotSession == "" || gotSession == "test/c1" {
		t.Errorf("mint sessionKey = %q, want a fresh branch of test/c1", gotSession)
	}
}

// TestFacetCommand_AppPathUnwired verifies an app facet with no minter wired
// fails cleanly rather than falling through to the (nonexistent) bot pool.
func TestFacetCommand_AppPathUnwired(t *testing.T) {
	cc := CommandContext{
		Agent:       &agent.Agent{},
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{ID: "test"},
	}
	ctx := tools.WithSessionKey(context.Background(), "test/c1")
	_, err := FacetCommand().Execute(ctx, Request{Source: "app"}, cc)
	if err == nil {
		t.Fatal("want error when MintFacetConversation is nil")
	}
}

// TestCompactCommand verifies compact session operation delegates to runCompaction.
func TestCompactCommand(t *testing.T) {
	cmd := CompactCommand()
	// Without a compactor, the command should error
	cc := CommandContext{
		Agent:       &agent.Agent{},
		AgentConfig: config.AgentConfig{ID: "test"},
	}
	_, err := cmd.Execute(context.Background(), Request{}, cc)
	if err == nil {
		t.Fatal("expected error without compactor")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("unexpected error: %v", err)
	}
	if cmd.Category != "operations" {
		t.Errorf("category = %q, want operations", cmd.Category)
	}
}

// TestScriptCommand verifies script command executes and captures output.
func TestScriptCommand(t *testing.T) {
	cmd := ScriptCommand("test", "test cmd", "echo hello from script", 10)
	result, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "hello from script" {
		t.Errorf("result = %q", result.Text)
	}
}

// TestScriptCommandFailure verifies script command captures stderr and exit code.
func TestScriptCommandFailure(t *testing.T) {
	cmd := ScriptCommand("fail", "failing cmd", "echo oops >&2; exit 1", 10)
	result, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result.Text, "oops") {
		t.Errorf("missing stderr in: %q", result.Text)
	}
	if !strings.Contains(result.Text, "Error:") {
		t.Errorf("missing Error in: %q", result.Text)
	}
}

// TestScriptCommandTimeout verifies script command times out correctly.
func TestScriptCommandTimeout(t *testing.T) {
	cmd := ScriptCommand("slow", "slow cmd", "sleep 60", 1)
	result, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result.Text, "timed out") {
		t.Errorf("missing timeout message in: %q", result.Text)
	}
}

// TestScriptCommandDefaultTimeout verifies default timeout is applied when zero is passed.
func TestScriptCommandDefaultTimeout(t *testing.T) {
	cmd := ScriptCommand("test", "test", "echo ok", 0)
	result, _ := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if result.Text != "ok" {
		t.Errorf("result = %q", result.Text)
	}
}

// TestLogCommand verifies log command displays last N lines in code block.
func TestLogCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	cmd := LogCommand()
	cc := CommandContext{EventLogPath: logPath}

	// Default: last 20, wrapped in code block
	result, _ := cmd.Execute(context.Background(), Request{}, cc)
	if !strings.HasPrefix(result.Text, "```\n") || !strings.HasSuffix(result.Text, "\n```") {
		t.Errorf("result not wrapped in code block:\n%s", result.Text)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(result.Text, "```\n"), "\n```")
	resultLines := strings.Split(inner, "\n")
	if len(resultLines) != 20 {
		t.Errorf("got %d lines, want 20", len(resultLines))
	}
	if resultLines[0] != "line 10" {
		t.Errorf("first line = %q, want 'line 10'", resultLines[0])
	}

	// Custom: last 5
	result, _ = cmd.Execute(context.Background(), Request{Args: "5"}, cc)
	inner = strings.TrimSuffix(strings.TrimPrefix(result.Text, "```\n"), "\n```")
	resultLines = strings.Split(inner, "\n")
	if len(resultLines) != 5 {
		t.Errorf("got %d lines, want 5", len(resultLines))
	}
}

// TestErrorsCommand verifies errors command filters by log level field, not message content.
// INFO lines containing "ERROR" or "WARN" in their message body must NOT be included.
func TestErrorsCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	content := strings.Join([]string{
		"2026-03-01T00:00:00Z INFO  [test] normal",
		"2026-03-01T00:00:01Z ERROR [test] bad thing",
		"2026-03-01T00:00:02Z INFO  [test] got ERROR response from API",
		"2026-03-01T00:00:03Z WARN  [test] warning",
		"2026-03-01T00:00:04Z INFO  [test] WARN string in message body",
		"2026-03-01T00:00:05Z INFO  [test] fine",
	}, "\n") + "\n"
	os.WriteFile(logPath, []byte(content), 0644)

	cmd := ErrorsCommand()
	cc := CommandContext{EventLogPath: logPath}
	result, _ := cmd.Execute(context.Background(), Request{}, cc)

	if !strings.HasPrefix(result.Text, "```\n") || !strings.HasSuffix(result.Text, "\n```") {
		t.Errorf("result not wrapped in code block:\n%s", result.Text)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(result.Text, "```\n"), "\n```")
	resultLines := strings.Split(inner, "\n")
	if len(resultLines) != 2 {
		t.Fatalf("got %d lines, want 2 (ERROR + WARN only):\n%s", len(resultLines), result.Text)
	}
	if !strings.Contains(resultLines[0], "bad thing") {
		t.Errorf("line 0 should be the ERROR line: %q", resultLines[0])
	}
	if !strings.Contains(resultLines[1], "warning") {
		t.Errorf("line 1 should be the WARN line: %q", resultLines[1])
	}
}

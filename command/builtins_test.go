package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func writeAPILog(t *testing.T, entries []apiEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, _ := json.Marshal(e)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()
	return path
}

func TestStatusCommand(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 100, Output: 50, CacheRead: 80, CacheWrite: 100, CostUSD: 0.001},
		{Timestamp: now, Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 200, Output: 100, CacheRead: 150, CacheWrite: 0, CostUSD: 0.002},
		{Timestamp: now, Session: "other:session", Model: "claude-haiku-4-5", Input: 500, Output: 200, CostUSD: 0.005},
	})

	cmd := NewStatusCommand(func() StatusInfo {
		return StatusInfo{
			SessionKey:   "agent:main:main",
			MessageCount: 42,
			Model:        "claude-haiku-4-5",
			Uptime:       2*time.Hour + 30*time.Minute,
			AgentBusy:    false,
		}
	}, path)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"agent:main:main",
		"claude-haiku-4-5",
		"42",
		"idle",
		"2h30m",
		"in=300",     // 100+200
		"out=150",    // 50+100
		"cache_read=230", // 80+150
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
}

func TestStatusCommandBusy(t *testing.T) {
	path := writeAPILog(t, nil)
	cmd := NewStatusCommand(func() StatusInfo {
		return StatusInfo{AgentBusy: true}
	}, path)

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "processing") {
		t.Errorf("expected 'processing', got:\n%s", result)
	}
}

func TestCacheCommand(t *testing.T) {
	now := time.Now().UTC()
	entries := make([]apiEntry, 7)
	for i := range entries {
		entries[i] = apiEntry{
			Timestamp:  now.Add(time.Duration(i) * time.Minute),
			Input:      100,
			CacheRead:  50,
			CacheWrite: 100,
			CostUSD:    0.001,
		}
	}
	path := writeAPILog(t, entries)

	cmd := NewCacheCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show last 5 only
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 5 {
		t.Errorf("got %d lines, want 5:\n%s", len(lines), result)
	}
	if !strings.Contains(result, "hit)") {
		t.Errorf("missing hit rate in:\n%s", result)
	}
}

func TestCacheCommandEmpty(t *testing.T) {
	cmd := NewCacheCommand("/nonexistent/api.jsonl")
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No API calls logged yet." {
		t.Errorf("result = %q", result)
	}
}

func TestLastCommand(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 100, Output: 50, StopReason: "end_turn", DurationMS: 1234, CostUSD: 0.001},
		{Timestamp: now.Add(time.Minute), Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 200, Output: 100, StopReason: "tool_use", DurationMS: 567, CostUSD: 0.002},
	})

	cmd := NewLastCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show the last entry
	if !strings.Contains(result, "tool_use") {
		t.Errorf("missing stop_reason in:\n%s", result)
	}
	if !strings.Contains(result, "567ms") {
		t.Errorf("missing duration in:\n%s", result)
	}
	if !strings.Contains(result, "in=200") {
		t.Errorf("missing input tokens in:\n%s", result)
	}
}

func TestCostCommandToday(t *testing.T) {
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	path := writeAPILog(t, []apiEntry{
		{Timestamp: yesterday, CostUSD: 0.100},
		{Timestamp: now, CostUSD: 0.050},
		{Timestamp: now, CostUSD: 0.025},
	})

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "$0.0750") {
		t.Errorf("expected today's cost $0.0750 in:\n%s", result)
	}
	if !strings.Contains(result, "2 API calls") {
		t.Errorf("expected 2 API calls in:\n%s", result)
	}
}

func TestCostCommandSession(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "session-a", CostUSD: 0.010},
		{Timestamp: now, Session: "session-b", CostUSD: 0.020},
		{Timestamp: now, Session: "session-a", CostUSD: 0.030},
	})

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "session")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "session-a") || !strings.Contains(result, "session-b") {
		t.Errorf("missing sessions in:\n%s", result)
	}
	if !strings.Contains(result, "total: $0.0600") {
		t.Errorf("missing total in:\n%s", result)
	}
}

func TestCostCommandDays(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now.AddDate(0, 0, -10), CostUSD: 0.100},
		{Timestamp: now.AddDate(0, 0, -2), CostUSD: 0.050},
		{Timestamp: now, CostUSD: 0.025},
	})

	cmd := NewCostCommand(path)
	result, _ := cmd.Execute(context.Background(), "3")
	if !strings.Contains(result, "Last 3 days") {
		t.Errorf("missing 'Last 3 days' in:\n%s", result)
	}
	if !strings.Contains(result, "$0.0750") {
		t.Errorf("expected $0.0750 in:\n%s", result)
	}
}

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

func TestModelCommand(t *testing.T) {
	model := "claude-haiku-4-5"
	cmd := NewModelCommand(
		func() string { return model },
		func(m string) { model = m },
	)

	// Show current
	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "claude-haiku-4-5") {
		t.Errorf("result = %q", result)
	}

	// Switch
	result, _ = cmd.Execute(context.Background(), "claude-opus-4-6")
	if model != "claude-opus-4-6" {
		t.Errorf("model not switched: %s", model)
	}
	if !strings.Contains(result, "claude-opus-4-6") {
		t.Errorf("result = %q", result)
	}
}

func TestSessionCommand(t *testing.T) {
	cmd := NewSessionCommand(func() SessionInfo {
		return SessionInfo{
			SessionKey:   "agent:main:main",
			MessageCount: 10,
			CreatedAt:    "2026-02-21T00:00:00Z",
			LastActivity: "2026-02-21T04:30:00Z",
		}
	})

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "agent:main:main") {
		t.Errorf("missing session key in:\n%s", result)
	}
	if !strings.Contains(result, "10") {
		t.Errorf("missing message count in:\n%s", result)
	}
}

func TestToolsCommand(t *testing.T) {
	cmd := NewToolsCommand(func() []ToolInfo {
		return []ToolInfo{
			{Name: "exec", Description: "Run commands"},
			{Name: "read", Description: "Read files"},
		}
	})

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "exec") || !strings.Contains(result, "read") {
		t.Errorf("missing tools in:\n%s", result)
	}
}

func TestToolsCommandEmpty(t *testing.T) {
	cmd := NewToolsCommand(func() []ToolInfo { return nil })
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No tools registered." {
		t.Errorf("result = %q", result)
	}
}

func TestConfigCommand(t *testing.T) {
	cmd := NewConfigCommand(func() string { return "model = \"haiku\"" })
	result, _ := cmd.Execute(context.Background(), "")
	if result != "model = \"haiku\"" {
		t.Errorf("result = %q", result)
	}
}

func TestLogCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	cmd := NewLogCommand(logPath)

	// Default: last 20
	result, _ := cmd.Execute(context.Background(), "")
	resultLines := strings.Split(strings.TrimSpace(result), "\n")
	if len(resultLines) != 20 {
		t.Errorf("got %d lines, want 20", len(resultLines))
	}
	if resultLines[0] != "line 10" {
		t.Errorf("first line = %q, want 'line 10'", resultLines[0])
	}

	// Custom: last 5
	result, _ = cmd.Execute(context.Background(), "5")
	resultLines = strings.Split(strings.TrimSpace(result), "\n")
	if len(resultLines) != 5 {
		t.Errorf("got %d lines, want 5", len(resultLines))
	}
}

func TestErrorsCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	content := "INFO normal\nERROR bad thing\nINFO ok\nWARN warning\nINFO fine\n"
	os.WriteFile(logPath, []byte(content), 0644)

	cmd := NewErrorsCommand(logPath)
	result, _ := cmd.Execute(context.Background(), "")

	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2:\n%s", len(lines), result)
	}
	if !strings.Contains(lines[0], "ERROR") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "WARN") {
		t.Errorf("line 1 = %q", lines[1])
	}
}

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

func TestUptimeCommand(t *testing.T) {
	startTime := time.Now().Add(-1 * time.Hour)
	cmd := NewUptimeCommand(startTime)

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "1h0m") {
		t.Errorf("result = %q", result)
	}
}

func TestScriptCommand(t *testing.T) {
	cmd := NewScriptCommand("test", "test cmd", "echo hello from script", 10)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "hello from script" {
		t.Errorf("result = %q", result)
	}
}

func TestScriptCommandFailure(t *testing.T) {
	cmd := NewScriptCommand("fail", "failing cmd", "echo oops >&2; exit 1", 10)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "oops") {
		t.Errorf("missing stderr in: %q", result)
	}
	if !strings.Contains(result, "Error:") {
		t.Errorf("missing Error in: %q", result)
	}
}

func TestScriptCommandTimeout(t *testing.T) {
	cmd := NewScriptCommand("slow", "slow cmd", "sleep 60", 1)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "timed out") {
		t.Errorf("missing timeout message in: %q", result)
	}
}

func TestScriptCommandDefaultTimeout(t *testing.T) {
	// Verify default timeout is applied (not 0)
	cmd := NewScriptCommand("test", "test", "echo ok", 0)
	result, _ := cmd.Execute(context.Background(), "")
	if result != "ok" {
		t.Errorf("result = %q", result)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{2*time.Hour + 15*time.Minute, "2h15m0s"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

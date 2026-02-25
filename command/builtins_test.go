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
			AgentID:          "main",
			SessionKey:       "agent:main:main",
			MessageCount:     42,
			Model:            "claude-haiku-4-5",
			Uptime:           2*time.Hour + 30*time.Minute,
			StartTime:        now.Add(-2*time.Hour - 30*time.Minute),
			AgentBusy:        false,
			CreatedAt:        "2026-02-23T13:33:00Z",
			LastActivity:     "2026-02-23T19:58:00Z",
			ContextLimit:     200000,
			CompactThreshold: 0.8,
		}
	}, path)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"main",
		"agent:main:main",
		"claude-haiku-4-5",
		"42",
		"idle",
		"2h30m",
		"13:33 UTC",
		"19:58 UTC",
		"$0.00",   // session cost
		"2 calls", // session call count
		"200,000", // context limit
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
		return StatusInfo{AgentID: "test", AgentBusy: true}
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

	// Summary line with avg hit rate
	if !strings.Contains(result, "Cache — last 5 calls") {
		t.Errorf("missing summary header in:\n%s", result)
	}
	if !strings.Contains(result, "avg") && !strings.Contains(result, "% hit") {
		t.Errorf("missing avg hit rate in:\n%s", result)
	}
	// Code block table format
	if !strings.Contains(result, "```") {
		t.Errorf("expected code block in:\n%s", result)
	}
	if !strings.Contains(result, "Time") || !strings.Contains(result, "CacheRead") || !strings.Contains(result, "Hit%") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "─") {
		t.Errorf("missing separator line in:\n%s", result)
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
		{Timestamp: yesterday, Session: "old-session", CostUSD: 0.100},
		{Timestamp: now, Session: "session-a", CostUSD: 0.050},
		{Timestamp: now, Session: "session-b", CostUSD: 0.025},
	})

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Total line (header before code block)
	if !strings.Contains(result, "$0.08") {
		t.Errorf("expected today's total in:\n%s", result)
	}
	if !strings.Contains(result, "2 calls") {
		t.Errorf("expected 2 calls in:\n%s", result)
	}
	// Code block table format
	if !strings.Contains(result, "```") {
		t.Errorf("expected code block in:\n%s", result)
	}
	if !strings.Contains(result, "Session") || !strings.Contains(result, "Cost") || !strings.Contains(result, "Calls") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "─") {
		t.Errorf("missing separator line in:\n%s", result)
	}
	// Per-session breakdown
	if !strings.Contains(result, "session-a") || !strings.Contains(result, "session-b") {
		t.Errorf("missing session breakdown in:\n%s", result)
	}
	// Total row
	if !strings.Contains(result, "Total") {
		t.Errorf("missing Total row in:\n%s", result)
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
	// "session" is an alias for default output now
	result, err := cmd.Execute(context.Background(), "session")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "session-a") || !strings.Contains(result, "session-b") {
		t.Errorf("missing sessions in:\n%s", result)
	}
	// session-a should appear first (higher total cost: 0.04 > 0.02)
	aIdx := strings.Index(result, "session-a")
	bIdx := strings.Index(result, "session-b")
	if aIdx > bIdx {
		t.Errorf("expected session-a before session-b (sorted by cost desc):\n%s", result)
	}
}

func TestCostCommandTop10Limit(t *testing.T) {
	now := time.Now().UTC()
	var entries []apiEntry
	for i := 0; i < 12; i++ {
		entries = append(entries, apiEntry{
			Timestamp: now,
			Session:   fmt.Sprintf("session-%02d", i),
			CostUSD:   float64(12-i) * 0.01,
		})
	}
	path := writeAPILog(t, entries)

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show top 10 + "+2 more"
	if !strings.Contains(result, "+2 more") {
		t.Errorf("missing '+2 more' overflow indicator in:\n%s", result)
	}
	// session-00 (highest cost) should be present
	if !strings.Contains(result, "session-00") {
		t.Errorf("missing top session in:\n%s", result)
	}
	// session-10 and session-11 (lowest cost) should be truncated
	if strings.Contains(result, "session-10") || strings.Contains(result, "session-11") {
		t.Errorf("low-cost sessions should be truncated:\n%s", result)
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

func TestCostCommand24h(t *testing.T) {
	now := time.Now().UTC()
	entries := []apiEntry{
		// 25h ago — should be excluded
		{Timestamp: now.Add(-25 * time.Hour), Session: "old", Model: "claude-haiku-4-5",
			Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 1000, CostUSD: 0.050},
		// 12h ago — included
		{Timestamp: now.Add(-12 * time.Hour), Session: "recent-a", Model: "claude-haiku-4-5",
			Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 1000, CostUSD: 0.040},
		// 1h ago — included
		{Timestamp: now.Add(-1 * time.Hour), Session: "recent-b", Model: "claude-opus-4-6",
			Input: 500, Output: 200, CacheRead: 3000, CacheWrite: 500, CostUSD: 0.100},
	}
	path := writeAPILog(t, entries)

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "24h")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show "last 24h" header
	if !strings.Contains(result, "last 24h") {
		t.Errorf("missing 'last 24h' header in:\n%s", result)
	}
	// Total should be 0.04 + 0.10 = $0.14
	if !strings.Contains(result, "$0.14") {
		t.Errorf("expected total $0.14 in:\n%s", result)
	}
	// Per-category breakdown lines
	for _, label := range []string{"Cache reads:", "Cache writes:", "Input:", "Output:"} {
		if !strings.Contains(result, label) {
			t.Errorf("missing category %q in:\n%s", label, result)
		}
	}
}

func TestCostCommandWeek(t *testing.T) {
	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	entries := []apiEntry{
		// 10 days ago — should be excluded
		{Timestamp: startOfToday.AddDate(0, 0, -10), Session: "old", CostUSD: 1.00},
		// 5 days ago
		{Timestamp: startOfToday.AddDate(0, 0, -5), Session: "s1", CostUSD: 0.50},
		// 2 days ago
		{Timestamp: startOfToday.AddDate(0, 0, -2), Session: "s2", CostUSD: 0.30},
		// today
		{Timestamp: startOfToday, Session: "s3", CostUSD: 0.20},
	}
	path := writeAPILog(t, entries)

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "week")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Header
	if !strings.Contains(result, "7-day summary") {
		t.Errorf("missing '7-day summary' header in:\n%s", result)
	}
	// Total = 0.50 + 0.30 + 0.20 = $1.00
	if !strings.Contains(result, "$1.00") {
		t.Errorf("expected total $1.00 in:\n%s", result)
	}
	// Mean/day
	if !strings.Contains(result, "Mean/day:") {
		t.Errorf("missing Mean/day in:\n%s", result)
	}
	// Today's date should appear
	todayStr := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(result, todayStr) {
		t.Errorf("missing today's date %s in:\n%s", todayStr, result)
	}
	// Days with no data should show $0.00
	if !strings.Contains(result, "$0.00") {
		t.Errorf("expected $0.00 for empty days in:\n%s", result)
	}
	// Verify newest-first order: today should appear before 5 days ago
	fiveDaysAgo := startOfToday.AddDate(0, 0, -5).Format("2006-01-02")
	todayIdx := strings.Index(result, todayStr)
	fiveIdx := strings.Index(result, fiveDaysAgo)
	if todayIdx > fiveIdx {
		t.Errorf("expected newest-first order, today before %s:\n%s", fiveDaysAgo, result)
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
	cmd := NewConfigCommand(func(args string) (string, error) {
		switch args {
		case "toml":
			return "toml output", nil
		case "available":
			return "available output", nil
		default:
			return "model = \"haiku\"", nil
		}
	})
	result, _ := cmd.Execute(context.Background(), "")
	if result != "model = \"haiku\"" {
		t.Errorf("default result = %q", result)
	}
	result, _ = cmd.Execute(context.Background(), "toml")
	if result != "toml output" {
		t.Errorf("toml result = %q", result)
	}
	result, _ = cmd.Execute(context.Background(), "available")
	if result != "available output" {
		t.Errorf("available result = %q", result)
	}
}

func TestPromptsCommand(t *testing.T) {
	cmd := NewPromptsCommand(func() PromptsData {
		return PromptsData{
			AgentID: "clutch",
			Prompts: []PromptInfo{
				{Label: "compaction_summary", Path: "/home/clod/prompts/compaction.md", Exists: true},
				{Label: "session_reset"},
				{Label: "handoff_msg", Inline: "You are picking up a compacted session."},
				{Label: "fork_prompt", Path: "/missing/file.md", Exists: false},
			},
			PromptDirs: []string{"/home/clod/prompts"},
			Files: []PromptFile{
				{Dir: "/home/clod/prompts", Name: "compaction.md", Configured: true},
				{Dir: "/home/clod/prompts", Name: "daily-review.md", Configured: false},
			},
		}
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"agent: clutch",
		"compaction_summary",
		"/home/clod/prompts/compaction.md",
		"✓",
		"session_reset",
		"[default]",
		"handoff_msg",
		"[inline: 39 chars]",
		"fork_prompt",
		"✗ (not found)",
		"Prompt files on disk:",
		"/home/clod/prompts/",
		"compaction.md",
		"[configured]",
		"daily-review.md",
		"[cron/other]",
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
}

func TestPromptsCommandEmpty(t *testing.T) {
	cmd := NewPromptsCommand(func() PromptsData {
		return PromptsData{
			AgentID: "test",
			Prompts: []PromptInfo{
				{Label: "fork_prompt"},
			},
		}
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "[default]") {
		t.Errorf("expected [default] in:\n%s", result)
	}
	// No files section when no dirs scanned
	if strings.Contains(result, "Prompt files on disk") {
		t.Errorf("should not show files section when no dirs:\n%s", result)
	}
}

func TestPromptsCommandNoFiles(t *testing.T) {
	cmd := NewPromptsCommand(func() PromptsData {
		return PromptsData{
			AgentID:    "test",
			Prompts:    []PromptInfo{{Label: "fork_prompt"}},
			PromptDirs: []string{"/some/dir"},
			Files:      nil,
		}
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "No prompt files found") {
		t.Errorf("expected 'No prompt files found' in:\n%s", result)
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

func TestFormatCommas(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{32793, "32,793"},
		{200000, "200,000"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		got := formatCommas(tt.n)
		if got != tt.want {
			t.Errorf("formatCommas(%d) = %q, want %q", tt.n, got, tt.want)
		}
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

func TestHelpCommand(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewPingCommand())                                       // category: session
	reg.Register(NewCacheCommand("/dev/null"))                           // category: observability
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

func TestMultiballCommand(t *testing.T) {
	forked := false
	cmd := NewMultiballCommand(func(ctx context.Context) (string, error) {
		forked = true
		return "Forked to @testbot (session: agent:main:multiball:mb-1)", nil
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !forked {
		t.Error("fork function not called")
	}
	if !strings.Contains(result, "@testbot") {
		t.Errorf("expected bot name in result, got %q", result)
	}
}

func TestMultiballCommandError(t *testing.T) {
	cmd := NewMultiballCommand(func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("no secondary bots configured")
	})

	_, err := cmd.Execute(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no secondary bots") {
		t.Errorf("unexpected error: %v", err)
	}
}

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

	// Code block table format
	if !strings.Contains(result, "Agents") {
		t.Errorf("missing header in:\n%s", result)
	}
	if !strings.Contains(result, "```") {
		t.Errorf("expected code block in:\n%s", result)
	}
	if !strings.Contains(result, "ID") || !strings.Contains(result, "Session") || !strings.Contains(result, "Messages") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "─") {
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

func TestCompactCommand(t *testing.T) {
	cmd := NewCompactCommand(func(ctx context.Context) (int, error) {
		return 42, nil
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "42 messages") {
		t.Errorf("expected message count in result: %q", result)
	}
	if cmd.Category != "operations" {
		t.Errorf("category = %q, want operations", cmd.Category)
	}
}

func TestCompactCommandError(t *testing.T) {
	cmd := NewCompactCommand(func(ctx context.Context) (int, error) {
		return 0, fmt.Errorf("too few messages to compact (3)")
	})

	_, err := cmd.Execute(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "too few") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAgentsCommandEmpty(t *testing.T) {
	cmd := NewAgentsCommand(func() []AgentInfo { return nil }, nil, nil)
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No agents configured." {
		t.Errorf("result = %q", result)
	}
}

func TestVoiceCommand(t *testing.T) {
	voiceOn := false
	cmd := NewVoiceCommand(
		func() bool { return voiceOn },
		func(on bool) { voiceOn = on },
	)

	// Toggle on
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !voiceOn {
		t.Error("voice mode should be on after toggle")
	}
	if !strings.Contains(result, "ON") {
		t.Errorf("expected ON in result, got %q", result)
	}

	// Toggle off
	result, err = cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if voiceOn {
		t.Error("voice mode should be off after second toggle")
	}
	if !strings.Contains(result, "OFF") {
		t.Errorf("expected OFF in result, got %q", result)
	}
}

func TestManaCommand(t *testing.T) {
	tests := []struct {
		name       string
		cmdName    string
		manaFn     func(context.Context) (string, error)
		wantResult string
	}{
		{
			name:    "default mana name",
			cmdName: "mana",
			manaFn: func(ctx context.Context) (string, error) {
				return "mana: 75% remaining", nil
			},
			wantResult: "mana: 75% remaining",
		},
		{
			name:    "custom name juice",
			cmdName: "juice",
			manaFn: func(ctx context.Context) (string, error) {
				return "juice: 50% remaining", nil
			},
			wantResult: "juice: 50% remaining",
		},
		{
			name:    "custom name credits",
			cmdName: "credits",
			manaFn: func(ctx context.Context) (string, error) {
				return "credits: 10% remaining", nil
			},
			wantResult: "credits: 10% remaining",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewManaCommand(tt.cmdName, tt.manaFn)
			if cmd.Name != tt.cmdName {
				t.Errorf("cmd.Name = %q, want %q", cmd.Name, tt.cmdName)
			}
			result, err := cmd.Execute(context.Background(), "")
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result != tt.wantResult {
				t.Errorf("result = %q, want %q", result, tt.wantResult)
			}
		})
	}
}

func TestManaCommandDescription(t *testing.T) {
	cmd := NewManaCommand("juice", func(ctx context.Context) (string, error) {
		return "", nil
	})
	if !strings.Contains(cmd.Description, "juice") {
		t.Errorf("Description should contain 'juice', got %q", cmd.Description)
	}
}

func testContextInfo() ContextInfo {
	return ContextInfo{
		SessionKey:       "agent:main:main",
		Model:            "claude-sonnet-4-5",
		CompactionThresh: 0.8,
		ContextLimit:     200000,
		SystemSections: []SystemSection{
			{Name: "IDENTITY.md", Chars: 2000},
			{Name: "SOUL.md", Chars: 4000},
			{Name: "MEMORY.md", Chars: 3000},
		},
		EnvironmentChars: 1200,
		SkillsChars:      800,
		Messages: MessageBreakdown{
			UserChars:       8000,
			AssistantChars:  12000,
			ToolResultChars: 6000,
			UserCount:       5,
			AssistantCount:  5,
		},
	}
}

func TestContextCommand(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 50000, CacheRead: 30000, CacheWrite: 10000},
		{Timestamp: now.Add(time.Minute), Session: "agent:main:main", Input: 60000, CacheRead: 40000, CacheWrite: 5000, Output: 1500},
		{Timestamp: now, Session: "other:session", Input: 100000, CacheRead: 0, CacheWrite: 0},
	})

	info := testContextInfo()
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"105,000",           // total tokens (60000 + 40000 + 5000)
		"200,000",           // context limit
		"52.5%",             // 105000 / 200000
		"160,000",           // threshold tokens (200000 * 0.8)
		"80%",               // compaction threshold
		"55,000 tokens until compaction",
		// System prompt sections
		"System prompt:",
		"IDENTITY.md",
		"SOUL.md",
		"MEMORY.md",
		"Environment",
		"Skills",
		// Conversation
		"Conversation:",
		"User messages",
		"Assistant",
		"Tool results",
		// Last API call
		"input:",
		"cache_read:",
		"cache_write:",
		"output:",
		"1,500", // output tokens
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
}

func TestContextCommandAtThreshold(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 150000, CacheRead: 20000, CacheWrite: 0},
	})

	info := testContextInfo()
	info.CompactionThresh = 0.8
	info.ContextLimit = 200000
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 170000 tokens is 85%, above 80% threshold
	if !strings.Contains(result, "at/above threshold") {
		t.Errorf("expected 'at/above threshold' in:\n%s", result)
	}
}

func TestContextCommandNoApiCalls(t *testing.T) {
	path := writeAPILog(t, nil)

	info := testContextInfo()
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result != "No API calls yet for this session." {
		t.Errorf("result = %q", result)
	}
}

func TestContextCommandOtherSession(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "other:session", Input: 50000, CacheRead: 0, CacheWrite: 0},
	})

	info := testContextInfo()
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// No entries for this session
	if result != "No API calls yet for this session." {
		t.Errorf("result = %q", result)
	}
}

func TestContextCommandCustomThreshold(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 100000, CacheRead: 0, CacheWrite: 0},
	})

	info := testContextInfo()
	info.Model = "claude-sonnet-4-5"
	info.CompactionThresh = 0.5
	info.ContextLimit = 200000
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, _ := cmd.Execute(context.Background(), "")

	// 100000 tokens is 50%, at threshold
	if !strings.Contains(result, "at/above threshold") {
		t.Errorf("expected 'at/above threshold' with 50%% threshold:\n%s", result)
	}
}

func TestContextCommandNoSkillsOrEnv(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 10000, CacheRead: 5000, CacheWrite: 1000},
	})

	info := testContextInfo()
	info.EnvironmentChars = 0
	info.SkillsChars = 0
	info.Messages.ToolResultChars = 0
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Environment and Skills lines should not appear
	if strings.Contains(result, "Environment") {
		t.Errorf("should not show Environment when 0 chars:\n%s", result)
	}
	if strings.Contains(result, "Skills") {
		t.Errorf("should not show Skills when 0 chars:\n%s", result)
	}
	if strings.Contains(result, "Tool results") {
		t.Errorf("should not show Tool results when 0 chars:\n%s", result)
	}
}

func TestSecretsListTable(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{
			"anthropic.token":      "x",
			"telegram.clutch":      "x",
			"telegram.clutchling":  "x",
			"telegram.scout":       "x",
			"brave.api_key":        "x",
		},
		allowedHosts: map[string][]string{
			"anthropic": {"api.anthropic.com"},
		},
	}

	cmd := NewSecretsCommand(store)
	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Header with count
	if !strings.Contains(result, "Secrets (5 keys)") {
		t.Errorf("missing header in:\n%s", result)
	}
	// Code block
	if !strings.Contains(result, "```") {
		t.Errorf("expected code block in:\n%s", result)
	}
	// Table headers
	if !strings.Contains(result, "Section") || !strings.Contains(result, "Key") || !strings.Contains(result, "Allowed Hosts") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	// Separator
	if !strings.Contains(result, "─") {
		t.Errorf("missing separator in:\n%s", result)
	}
	// Section grouping — "telegram" should appear once, not three times
	if strings.Count(result, "telegram") != 1 {
		t.Errorf("section 'telegram' should appear once (not repeated for each key):\n%s", result)
	}
	// All keys present
	for _, key := range []string{"token", "clutch", "clutchling", "scout", "api_key"} {
		if !strings.Contains(result, key) {
			t.Errorf("missing key %q in:\n%s", key, result)
		}
	}
	// Allowed hosts column
	if !strings.Contains(result, "api.anthropic.com") {
		t.Errorf("missing allowed host in:\n%s", result)
	}
	if !strings.Contains(result, "(none)") {
		t.Errorf("missing (none) for sections without allowed_hosts in:\n%s", result)
	}
}

func TestSecretsListEmpty(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := NewSecretsCommand(store)
	result, _ := cmd.Execute(context.Background(), "list")
	if result != "No secrets configured." {
		t.Errorf("result = %q", result)
	}
}

func TestSecretsHostsView(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	// View hosts for a section
	result, err := cmd.Execute(context.Background(), "hosts myapi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "api.example.com") || !strings.Contains(result, "api.backup.com") {
		t.Errorf("expected hosts in output: %s", result)
	}

	// View hosts for section without hosts
	result, _ = cmd.Execute(context.Background(), "hosts legacy")
	if !strings.Contains(result, "(none)") {
		t.Errorf("expected (none) for section without hosts: %s", result)
	}
}

func TestSecretsHostsAdd(t *testing.T) {
	store := &mockSecretsStore{
		data:         map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi add api.new.com")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Added") {
		t.Errorf("expected Added message: %s", result)
	}
	if !store.saved {
		t.Error("expected Save() to be called")
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.new.com" {
		t.Errorf("hosts = %v", hosts)
	}
}

func TestSecretsHostsRemove(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com", "api.backup.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi remove api.example.com")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Removed") {
		t.Errorf("expected Removed message: %s", result)
	}
	hosts := store.SectionAllowedHosts("myapi")
	if len(hosts) != 1 || hosts[0] != "api.backup.com" {
		t.Errorf("hosts after remove = %v", hosts)
	}

	// Remove nonexistent
	result, _ = cmd.Execute(context.Background(), "hosts myapi remove nonexistent.com")
	if !strings.Contains(result, "not found") {
		t.Errorf("expected not found message: %s", result)
	}
}

func TestSecretsHostsClear(t *testing.T) {
	store := &mockSecretsStore{
		data: map[string]string{"myapi.token": "x"},
		allowedHosts: map[string][]string{
			"myapi": {"api.example.com"},
		},
	}
	cmd := NewSecretsCommand(store)

	result, err := cmd.Execute(context.Background(), "hosts myapi clear")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Cleared") {
		t.Errorf("expected Cleared message: %s", result)
	}
	if store.SectionAllowedHosts("myapi") != nil {
		t.Error("hosts should be nil after clear")
	}
}

func TestSecretsHostsUsage(t *testing.T) {
	store := &mockSecretsStore{data: map[string]string{}}
	cmd := NewSecretsCommand(store)

	// No args
	result, _ := cmd.Execute(context.Background(), "hosts")
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage: %s", result)
	}

	// Invalid action
	result, _ = cmd.Execute(context.Background(), "hosts myapi invalid")
	if !strings.Contains(result, "Usage") {
		t.Errorf("expected usage for invalid action: %s", result)
	}
}

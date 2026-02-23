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

	// Summary line + 5 detail lines = 6 lines
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 6 {
		t.Errorf("got %d lines, want 6:\n%s", len(lines), result)
	}
	// Summary line with avg hit rate
	if !strings.Contains(result, "Cache — last 5 calls") {
		t.Errorf("missing summary header in:\n%s", result)
	}
	if !strings.Contains(result, "avg") && !strings.Contains(result, "% hit") {
		t.Errorf("missing avg hit rate in:\n%s", result)
	}
	// Comma-formatted numbers
	if !strings.Contains(result, "cR=") {
		t.Errorf("missing cR= in:\n%s", result)
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

	// Total line
	if !strings.Contains(result, "$0.08") {
		t.Errorf("expected today's total in:\n%s", result)
	}
	if !strings.Contains(result, "2 calls") {
		t.Errorf("expected 2 calls in:\n%s", result)
	}
	// Per-session breakdown
	if !strings.Contains(result, "session-a") || !strings.Contains(result, "session-b") {
		t.Errorf("missing session breakdown in:\n%s", result)
	}
	if !strings.Contains(result, "By session:") {
		t.Errorf("missing 'By session:' header in:\n%s", result)
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
	cmd := NewMultiballCommand(func() (string, error) {
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
	cmd := NewMultiballCommand(func() (string, error) {
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
			{ID: "main", SessionKey: "agent:main:main", Model: "opus-4", Busy: false, MessageCount: 31, LastActivity: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
			{ID: "scout", SessionKey: "agent:scout:main", Model: "haiku-4", Busy: true, MessageCount: 12},
		}
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Active Sessions") {
		t.Errorf("missing header in:\n%s", result)
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
	if !strings.Contains(result, "31 msgs") {
		t.Errorf("missing message count in:\n%s", result)
	}
	if !strings.Contains(result, "ago") {
		t.Errorf("missing last activity in:\n%s", result)
	}
}

func TestAgentsCommandEmpty(t *testing.T) {
	cmd := NewAgentsCommand(func() []AgentInfo { return nil })
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

func TestContextCommand(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 50000, CacheRead: 30000, CacheWrite: 10000},
		{Timestamp: now.Add(time.Minute), Session: "agent:main:main", Input: 60000, CacheRead: 40000, CacheWrite: 5000},
		{Timestamp: now, Session: "other:session", Input: 100000, CacheRead: 0, CacheWrite: 0},
	})

	cmd := NewContextCommand(path, func() ContextInfo {
		return ContextInfo{
			SessionKey:       "agent:main:main",
			Model:            "claude-sonnet-4-5",
			CompactionThresh: 0.8,
			ContextLimit:     200000,
		}
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"claude-sonnet-4-5",
		"105000",       // 60000 + 40000 + 5000 = 105000 tokens
		"200000",       // context limit
		"52.5%",        // 105000 / 200000 = 52.5%
		"input: 60000", // last input
		"cache_read: 40000",
		"cache_write: 5000",
		"80%",                          // compaction threshold
		"160000",                       // threshold tokens (200000 * 0.8)
		"55000 tokens until threshold", // 160000 - 105000
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

	cmd := NewContextCommand(path, func() ContextInfo {
		return ContextInfo{
			SessionKey:       "agent:main:main",
			Model:            "claude-haiku-4-5",
			CompactionThresh: 0.8,
			ContextLimit:     200000,
		}
	})

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

	cmd := NewContextCommand(path, func() ContextInfo {
		return ContextInfo{
			SessionKey:       "agent:main:main",
			Model:            "claude-haiku-4-5",
			CompactionThresh: 0.8,
			ContextLimit:     200000,
		}
	})

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

	cmd := NewContextCommand(path, func() ContextInfo {
		return ContextInfo{
			SessionKey:       "agent:main:main",
			Model:            "claude-haiku-4-5",
			CompactionThresh: 0.8,
			ContextLimit:     200000,
		}
	})

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

	cmd := NewContextCommand(path, func() ContextInfo {
		return ContextInfo{
			SessionKey:       "agent:main:main",
			Model:            "claude-sonnet-4-5",
			CompactionThresh: 0.5,
			ContextLimit:     200000,
		}
	})

	result, _ := cmd.Execute(context.Background(), "")

	// 100000 tokens is 50%, at threshold
	if !strings.Contains(result, "at/above threshold") {
		t.Errorf("expected 'at/above threshold' with 50%% threshold:\n%s", result)
	}
}

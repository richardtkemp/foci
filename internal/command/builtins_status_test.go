package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/tools"
)

// TestStatusCommand verifies status output contains all required session info,
// API call stats, and formatting. Uses api.db for session stats — works for
// both API and delegated (CC backend) sessions.
func TestStatusCommand(t *testing.T) {
	now := time.Now().UTC()
	sk := "main/c1/100"
	path := initAPIDB(t, []log.APIEntry{
		{Timestamp: now, Session: sk, Model: "claude-haiku-4-5", Input: 100, Output: 50, CacheRead: 80, CacheWrite: 100, CostUSD: 0.001, CallType: "conversation"},
		{Timestamp: now.Add(time.Minute), Session: sk, Model: "claude-haiku-4-5", Input: 200, Output: 100, CacheRead: 150, CacheWrite: 0, CostUSD: 0.002, CallType: "conversation"},
		{Timestamp: now, Session: "other/c2/200", Model: "claude-haiku-4-5", Input: 500, Output: 200, CostUSD: 0.005, CallType: "conversation"},
	})

	sessDir := t.TempDir()
	store := session.NewStore(sessDir)

	ag := &agent.Agent{Model: "claude-haiku-4-5"}

	startTime := now.Add(-2*time.Hour - 30*time.Minute)
	cc := CommandContext{
		Agent:               ag,
		Sessions:            store,
		APILogPath:          path,
		AgentConfig:         config.AgentConfig{ID: "main"},
		StartTime:           startTime,
		CompactionThreshold: 0.8,
	}

	ctx := tools.WithSessionKey(context.Background(), sk)
	cmd := StatusCommand()
	result, err := cmd.Execute(ctx, Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"main",
		sk,
		"claude-haiku-4-5",
		"2", // 2 conversation turns
		"idle",
		"2h30m",
		"$0.00",   // session cost
		"2 calls", // session call count
		"200,000", // context limit
	}
	for _, check := range checks {
		if !strings.Contains(result.Text, check) {
			t.Errorf("missing %q in:\n%s", check, result.Text)
		}
	}
}

// TestStatusCommandBusy verifies busy status is shown correctly when agent is processing.
func TestStatusCommandBusy(t *testing.T) {
	sk := "test/c1/100"
	path := writeAPILog(t, nil)

	sessDir := t.TempDir()
	store := session.NewStore(sessDir)

	ag := &agent.Agent{Model: "claude-haiku-4-5"}
	ag.SetTurnInFlightForTest(sk, true) // mark this session as busy

	cc := CommandContext{
		Agent:       ag,
		Sessions:    store,
		APILogPath:  path,
		AgentConfig: config.AgentConfig{ID: "test"},
		StartTime:   time.Now(),
	}

	ctx := tools.WithSessionKey(context.Background(), sk)
	cmd := StatusCommand()
	result, _ := cmd.Execute(ctx, Request{}, cc)
	if !strings.Contains(result.Text, "processing") {
		t.Errorf("expected 'processing', got:\n%s", result.Text)
	}
}

// TestCacheCommand verifies cache hit rates and token usage are calculated and displayed correctly.
func TestCacheCommand(t *testing.T) {
	now := time.Now().UTC()
	entries := make([]log.APIEntry, 7)
	for i := range entries {
		entries[i] = log.APIEntry{
			Timestamp:  now.Add(time.Duration(i) * time.Minute),
			Input:      100,
			CacheRead:  50,
			CacheWrite: 100,
			CostUSD:    0.001,
		}
	}
	path := writeAPILog(t, entries)
	cc := CommandContext{APILogPath: path}

	cmd := CacheCommand()

	// Test default (no args) - should show last 5
	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Summary line with avg hit rate
	if !strings.Contains(result.Text, "Cache — last 5 calls") {
		t.Errorf("missing summary header in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "avg") && !strings.Contains(result.Text, "% hit") {
		t.Errorf("missing avg hit rate in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "Time") || !strings.Contains(result.Text, "CacheRead") || !strings.Contains(result.Text, "Hit%") {
		t.Errorf("missing table headers in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator line in:\n%s", result.Text)
	}

	// Test with argument - should show last 3
	result, err = cmd.Execute(context.Background(), Request{Args: "3"}, cc)
	if err != nil {
		t.Fatalf("Execute with arg: %v", err)
	}
	if !strings.Contains(result.Text, "Cache — last 3 calls") {
		t.Errorf("missing summary header with arg in:\n%s", result.Text)
	}

	// Test with argument larger than available entries - should show all 7
	result, err = cmd.Execute(context.Background(), Request{Args: "10"}, cc)
	if err != nil {
		t.Fatalf("Execute with large arg: %v", err)
	}
	if !strings.Contains(result.Text, "Cache — last 7 calls") {
		t.Errorf("missing summary header with large arg in:\n%s", result.Text)
	}

	// Test with invalid argument - should use default of 5
	result, err = cmd.Execute(context.Background(), Request{Args: "invalid"}, cc)
	if err != nil {
		t.Fatalf("Execute with invalid arg: %v", err)
	}
	if !strings.Contains(result.Text, "Cache — last 5 calls") {
		t.Errorf("invalid arg should use default in:\n%s", result.Text)
	}
}

// TestCacheCommandEmpty verifies appropriate message for no API calls.
func TestCacheCommandEmpty(t *testing.T) {
	cc := CommandContext{APILogPath: "/nonexistent/api.jsonl"}
	cmd := CacheCommand()
	result, _ := cmd.Execute(context.Background(), Request{}, cc)
	if result.Text != "No API calls logged yet." {
		t.Errorf("result = %q", result.Text)
	}
}

// TestLastCommand verifies /last shows the most recent API call per agent
// as a table, and supports filtering by agent name.
func TestLastCommand(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []log.APIEntry{
		{Timestamp: now, Session: "main/c1/100", Model: "claude-haiku-4-5", Input: 100, Output: 50, CostUSD: 0.001},
		{Timestamp: now.Add(time.Minute), Session: "main/c1/100", Model: "claude-haiku-4-5", Input: 200, Output: 100, CostUSD: 0.002},
		{Timestamp: now.Add(2 * time.Minute), Session: "helper/c2/200", Model: "claude-sonnet-4-5", Input: 300, Output: 150, CostUSD: 0.005},
	})
	cc := CommandContext{APILogPath: path}

	cmd := LastCommand()

	// No args: should show one row per agent (main and helper).
	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Last API call per agent") {
		t.Errorf("missing title in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "main") || !strings.Contains(result.Text, "helper") {
		t.Errorf("should show both agents in:\n%s", result.Text)
	}
	// main's latest should be the second entry (in=200)
	if !strings.Contains(result.Text, "in=200") {
		t.Errorf("should show main's latest call (in=200) in:\n%s", result.Text)
	}
	// helper's entry
	if !strings.Contains(result.Text, "in=300") {
		t.Errorf("should show helper's call (in=300) in:\n%s", result.Text)
	}

	// Filter to specific agent.
	result, err = cmd.Execute(context.Background(), Request{Args: "helper"}, cc)
	if err != nil {
		t.Fatalf("Execute with filter: %v", err)
	}
	if !strings.Contains(result.Text, "helper") {
		t.Errorf("filtered result should contain helper in:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "in=200") {
		t.Errorf("filtered result should not contain main's call in:\n%s", result.Text)
	}

	// Filter to non-existent agent.
	result, err = cmd.Execute(context.Background(), Request{Args: "nobody"}, cc)
	if err != nil {
		t.Fatalf("Execute with bad filter: %v", err)
	}
	if !strings.Contains(result.Text, "No API calls for agent") {
		t.Errorf("expected no-calls message, got:\n%s", result.Text)
	}
}

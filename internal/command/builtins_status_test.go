package command

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStatusCommand verifies status output contains all required session info, API call stats, and formatting.
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

// TestStatusCommandBusy verifies busy status is shown correctly when agent is processing.
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

// TestCacheCommand verifies cache hit rates and token usage are calculated and displayed correctly.
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

	// Test default (no args) - should show last 5
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
	if !strings.Contains(result, "Time") || !strings.Contains(result, "CacheRead") || !strings.Contains(result, "Hit%") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "---") {
		t.Errorf("missing separator line in:\n%s", result)
	}

	// Test with argument - should show last 3
	result, err = cmd.Execute(context.Background(), "3")
	if err != nil {
		t.Fatalf("Execute with arg: %v", err)
	}
	if !strings.Contains(result, "Cache — last 3 calls") {
		t.Errorf("missing summary header with arg in:\n%s", result)
	}

	// Test with argument larger than available entries - should show all 7
	result, err = cmd.Execute(context.Background(), "10")
	if err != nil {
		t.Fatalf("Execute with large arg: %v", err)
	}
	if !strings.Contains(result, "Cache — last 7 calls") {
		t.Errorf("missing summary header with large arg in:\n%s", result)
	}

	// Test with invalid argument - should use default of 5
	result, err = cmd.Execute(context.Background(), "invalid")
	if err != nil {
		t.Fatalf("Execute with invalid arg: %v", err)
	}
	if !strings.Contains(result, "Cache — last 5 calls") {
		t.Errorf("invalid arg should use default in:\n%s", result)
	}
}

// TestCacheCommandEmpty verifies appropriate message for no API calls.
func TestCacheCommandEmpty(t *testing.T) {
	cmd := NewCacheCommand("/nonexistent/api.jsonl")
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No API calls logged yet." {
		t.Errorf("result = %q", result)
	}
}

// TestLastCommand verifies last API call shows all details correctly.
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

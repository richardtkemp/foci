package command

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStatusCommand(t *testing.T) {
	// Verifies status output contains all required session info, API call stats, and formatting.
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 100, Output: 50, CacheRead: 80, CacheWrite: 100, CostUSD: 0.001},
		{Timestamp: now, Session: "agent:main:main", Model: "claude-haiku-4-5", Input: 200, Output: 100, CacheRead: 150, CacheWrite: 0, CostUSD: 0.002},
		{Timestamp: now, Session: "other:session", Model: "claude-haiku-4-5", Input: 500, Output: 200, CostUSD: 0.005},
	})

	cmd := NewStatusCommand(func(_ context.Context) StatusInfo {
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
	// Verifies busy status is shown correctly when agent is processing.
	path := writeAPILog(t, nil)
	cmd := NewStatusCommand(func(_ context.Context) StatusInfo {
		return StatusInfo{AgentID: "test", AgentBusy: true}
	}, path)

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "processing") {
		t.Errorf("expected 'processing', got:\n%s", result)
	}
}

func TestStatusCommandMultiball(t *testing.T) {
	// Verifies that /status passes the execution context through to the statusFn,
	// so multiball sessions can resolve the correct session key from ChatIDKey
	// instead of always showing the primary session's status.
	now := time.Now().UTC()
	mainSession := "agent/c1/100"
	multiballSession := "agent/c1/100/mb-abc123"

	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: mainSession, Model: "claude-haiku-4-5", Input: 200, Output: 100, CostUSD: 0.002},
		{Timestamp: now, Session: multiballSession, Model: "claude-sonnet-4-5", Input: 500, Output: 200, CostUSD: 0.010},
	})

	// statusFn checks for ChatIDKey in context to resolve the correct session,
	// mimicking the real sessionKeyFromCtx behavior.
	cmd := NewStatusCommand(func(ctx context.Context) StatusInfo {
		sk := mainSession
		if chatID, ok := ctx.Value(ChatIDKey{}).(int64); ok && chatID == 77777 {
			sk = multiballSession
		}
		return StatusInfo{
			AgentID:          "agent",
			SessionKey:       sk,
			MessageCount:     10,
			Model:            "claude-sonnet-4-5",
			Uptime:           time.Hour,
			StartTime:        now.Add(-time.Hour),
			ContextLimit:     200000,
			CompactThreshold: 0.8,
		}
	}, path)

	// Simulate multiball context with ChatIDKey set.
	ctx := context.WithValue(context.Background(), ChatIDKey{}, int64(77777))
	result, err := cmd.Execute(ctx, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should show the multiball session, not the main session.
	if !strings.Contains(result, multiballSession) {
		t.Errorf("expected multiball session key %q in output:\n%s", multiballSession, result)
	}
	if strings.Contains(result, mainSession) && !strings.Contains(result, multiballSession) {
		t.Errorf("should not show main session when run from multiball context:\n%s", result)
	}

	// Should show multiball session's cost ($0.010), not main's ($0.002).
	if !strings.Contains(result, "$0.01") {
		t.Errorf("expected multiball session cost in output:\n%s", result)
	}
	if !strings.Contains(result, "1 call") {
		t.Errorf("expected 1 call for multiball session:\n%s", result)
	}
}

func TestCacheCommand(t *testing.T) {
	// Verifies cache hit rates and token usage are calculated and displayed correctly.
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

func TestCacheCommandEmpty(t *testing.T) {
	// Verifies appropriate message for no API calls.
	cmd := NewCacheCommand("/nonexistent/api.jsonl")
	result, _ := cmd.Execute(context.Background(), "")
	if result != "No API calls logged yet." {
		t.Errorf("result = %q", result)
	}
}

func TestLastCommand(t *testing.T) {
	// Verifies /last shows the most recent API call per agent
	// as a table, and supports filtering by agent name.
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "main/c1/100", Model: "claude-haiku-4-5", Input: 100, Output: 50, CostUSD: 0.001},
		{Timestamp: now.Add(time.Minute), Session: "main/c1/100", Model: "claude-haiku-4-5", Input: 200, Output: 100, CostUSD: 0.002},
		{Timestamp: now.Add(2 * time.Minute), Session: "helper/c2/200", Model: "claude-sonnet-4-5", Input: 300, Output: 150, CostUSD: 0.005},
	})

	cmd := NewLastCommand(path)

	// No args: should show one row per agent (main and helper).
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Last API call per agent") {
		t.Errorf("missing title in:\n%s", result)
	}
	if !strings.Contains(result, "main") || !strings.Contains(result, "helper") {
		t.Errorf("should show both agents in:\n%s", result)
	}
	// main's latest should be the second entry (in=200)
	if !strings.Contains(result, "in=200") {
		t.Errorf("should show main's latest call (in=200) in:\n%s", result)
	}
	// helper's entry
	if !strings.Contains(result, "in=300") {
		t.Errorf("should show helper's call (in=300) in:\n%s", result)
	}

	// Filter to specific agent.
	result, err = cmd.Execute(context.Background(), "helper")
	if err != nil {
		t.Fatalf("Execute with filter: %v", err)
	}
	if !strings.Contains(result, "helper") {
		t.Errorf("filtered result should contain helper in:\n%s", result)
	}
	if strings.Contains(result, "in=200") {
		t.Errorf("filtered result should not contain main's call in:\n%s", result)
	}

	// Filter to non-existent agent.
	result, err = cmd.Execute(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Execute with bad filter: %v", err)
	}
	if !strings.Contains(result, "No API calls for agent") {
		t.Errorf("expected no-calls message, got:\n%s", result)
	}
}

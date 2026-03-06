package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCostCommandUsage verifies usage message shows all available subcommands.
func TestCostCommandUsage(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "s", CostUSD: 0.01},
	})
	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"/cost today", "/cost 24h", "/cost week", "/cost <days>"} {
		if !strings.Contains(result, want) {
			t.Errorf("missing %q in usage:\n%s", want, result)
		}
	}
}

// TestCostCommandToday verifies today's costs are aggregated by session with correct formatting.
func TestCostCommandToday(t *testing.T) {
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	path := writeAPILog(t, []apiEntry{
		{Timestamp: yesterday, Session: "old-session", CostUSD: 0.100},
		{Timestamp: now, Session: "session-a", CostUSD: 0.050},
		{Timestamp: now, Session: "session-b", CostUSD: 0.025},
	})

	cmd := NewCostCommand(path)
	result, err := cmd.Execute(context.Background(), "today")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "$0.08") {
		t.Errorf("expected today's total in:\n%s", result)
	}
	if !strings.Contains(result, "2 calls") {
		t.Errorf("expected 2 calls in:\n%s", result)
	}
	if !strings.Contains(result, "Session") || !strings.Contains(result, "Cost") || !strings.Contains(result, "Calls") {
		t.Errorf("missing table headers in:\n%s", result)
	}
	if !strings.Contains(result, "---") {
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

// TestCostCommandSession verifies sessions are sorted by cost in descending order.
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

// TestCostCommandTop10Limit verifies output is capped at top 10 sessions with overflow indicator.
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
	result, err := cmd.Execute(context.Background(), "today")
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

// TestCostCommandDays verifies costs over specified number of days are summed correctly.
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

// TestCostCommand24h verifies costs from last 24 hours are correctly filtered and categorized.
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
	// Category table headers and rows
	for _, label := range []string{"Category", "Cache reads", "Cache writes", "Input", "Output", "Total"} {
		if !strings.Contains(result, label) {
			t.Errorf("missing %q in:\n%s", label, result)
		}
	}
	if !strings.Contains(result, "---") {
		t.Errorf("missing separator line in:\n%s", result)
	}
}

// TestCostCommandWeek verifies 7-day cost summary with daily breakdown and averages.
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
	// Table headers and summary rows
	for _, want := range []string{"Date", "Cost", "Total", "Mean/day"} {
		if !strings.Contains(result, want) {
			t.Errorf("missing %q in:\n%s", want, result)
		}
	}
	if !strings.Contains(result, "---") {
		t.Errorf("missing separator line in:\n%s", result)
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

package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func costCC(apiLogPath string) CommandContext {
	return CommandContext{APILogPath: apiLogPath}
}

// TestCostCommandUsage verifies usage message shows all available subcommands.
func TestCostCommandUsage(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "s", CostUSD: 0.01},
	})
	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"/cost today", "/cost 24h", "/cost week", "/cost <days>"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("missing %q in usage:\n%s", want, result.Text)
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

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "today"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "$0.08") {
		t.Errorf("expected today's total in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "2 calls") {
		t.Errorf("expected 2 calls in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "Session") || !strings.Contains(result.Text, "Cost") || !strings.Contains(result.Text, "Calls") {
		t.Errorf("missing table headers in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator line in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "session-a") || !strings.Contains(result.Text, "session-b") {
		t.Errorf("missing session breakdown in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "Total") {
		t.Errorf("missing Total row in:\n%s", result.Text)
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

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "session"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "session-a") || !strings.Contains(result.Text, "session-b") {
		t.Errorf("missing sessions in:\n%s", result.Text)
	}
	aIdx := strings.Index(result.Text, "session-a")
	bIdx := strings.Index(result.Text, "session-b")
	if aIdx > bIdx {
		t.Errorf("expected session-a before session-b (sorted by cost desc):\n%s", result.Text)
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

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "today"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "+2 more") {
		t.Errorf("missing '+2 more' overflow indicator in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "session-00") {
		t.Errorf("missing top session in:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "session-10") || strings.Contains(result.Text, "session-11") {
		t.Errorf("low-cost sessions should be truncated:\n%s", result.Text)
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

	cmd := CostCommand()
	result, _ := cmd.Execute(context.Background(), Request{Args: "3"}, costCC(path))
	if !strings.Contains(result.Text, "Last 3 days") {
		t.Errorf("missing 'Last 3 days' in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "$0.0750") {
		t.Errorf("expected $0.0750 in:\n%s", result.Text)
	}
}

// TestCostCommand24h verifies costs from last 24 hours are correctly filtered and categorized.
func TestCostCommand24h(t *testing.T) {
	now := time.Now().UTC()
	entries := []apiEntry{
		{Timestamp: now.Add(-25 * time.Hour), Session: "old", Model: "claude-haiku-4-5",
			Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 1000, CostUSD: 0.050},
		{Timestamp: now.Add(-12 * time.Hour), Session: "recent-a", Model: "claude-haiku-4-5",
			Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 1000, CostUSD: 0.040},
		{Timestamp: now.Add(-1 * time.Hour), Session: "recent-b", Model: "claude-opus-4-6",
			Input: 500, Output: 200, CacheRead: 3000, CacheWrite: 500, CostUSD: 0.100},
	}
	path := writeAPILog(t, entries)

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "24h"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "last 24h") {
		t.Errorf("missing 'last 24h' header in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "$0.14") {
		t.Errorf("expected total $0.14 in:\n%s", result.Text)
	}
	for _, label := range []string{"Category", "Cache reads", "Cache writes", "Input", "Output", "Total"} {
		if !strings.Contains(result.Text, label) {
			t.Errorf("missing %q in:\n%s", label, result.Text)
		}
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator line in:\n%s", result.Text)
	}
}

// TestCostCommandWeek verifies 7-day cost summary with daily breakdown and averages.
func TestCostCommandWeek(t *testing.T) {
	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	entries := []apiEntry{
		{Timestamp: startOfToday.AddDate(0, 0, -10), Session: "old", CostUSD: 1.00},
		{Timestamp: startOfToday.AddDate(0, 0, -5), Session: "s1", CostUSD: 0.50},
		{Timestamp: startOfToday.AddDate(0, 0, -2), Session: "s2", CostUSD: 0.30},
		{Timestamp: startOfToday, Session: "s3", CostUSD: 0.20},
	}
	path := writeAPILog(t, entries)

	cmd := CostCommand()
	result, err := cmd.Execute(context.Background(), Request{Args: "week"}, costCC(path))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "7-day summary") {
		t.Errorf("missing '7-day summary' header in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "$1.00") {
		t.Errorf("expected total $1.00 in:\n%s", result.Text)
	}
	for _, want := range []string{"Date", "Cost", "Total", "Mean/day"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("missing %q in:\n%s", want, result.Text)
		}
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("missing separator line in:\n%s", result.Text)
	}
	todayStr := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(result.Text, todayStr) {
		t.Errorf("missing today's date %s in:\n%s", todayStr, result.Text)
	}
	if !strings.Contains(result.Text, "$0.00") {
		t.Errorf("expected $0.00 for empty days in:\n%s", result.Text)
	}
	fiveDaysAgo := startOfToday.AddDate(0, 0, -5).Format("2006-01-02")
	todayIdx := strings.Index(result.Text, todayStr)
	fiveIdx := strings.Index(result.Text, fiveDaysAgo)
	if todayIdx > fiveIdx {
		t.Errorf("expected newest-first order, today before %s:\n%s", fiveDaysAgo, result.Text)
	}
}

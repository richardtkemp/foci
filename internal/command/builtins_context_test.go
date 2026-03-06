package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestContextCommand verifies context display shows token usage, threshold status, and system prompt breakdown.
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
		"```",      // code block wrapping
		"~105,000", // total tokens (60000 + 40000 + 5000), estimated
		"200,000",  // context limit
		"52.5%",    // 105000 / 200000
		"160,000",  // threshold tokens (200000 * 0.8)
		"80%",      // compaction threshold
		"55,000 tokens until compaction",
		// System prompt sections
		"System prompt:",
		"IDENTITY.md",
		"SOUL.md",
		"MEMORY.md",
		"Environment",
		"Skills",
		"tokens", // all sections show tokens
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
	// Should have 4 separate code blocks (header, system, conversation, last API call)
	if strings.Count(result, "```") != 8 { // 4 opening + 4 closing
		t.Errorf("expected 4 code blocks (8 backtick markers), got %d in:\n%s", strings.Count(result, "```"), result)
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
	// Should NOT contain "chars" — all counts are in tokens now
	if strings.Contains(result, "chars") {
		t.Errorf("should not contain 'chars', all counts should be in tokens:\n%s", result)
	}
}

// TestContextCommandAtThreshold verifies "at/above threshold" message when usage reaches compaction threshold.
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

// TestContextCommandNoApiCalls verifies appropriate message when no API calls exist.
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

// TestContextCommandOtherSession verifies correct message when API calls exist but not for current session.
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

// TestContextCommandCustomThreshold verifies threshold comparison works with custom values.
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

// TestContextCommandNoSkillsOrEnv verifies sections with zero tokens are omitted.
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
		t.Errorf("should not show Environment when 0 tokens:\n%s", result)
	}
	if strings.Contains(result, "Skills") {
		t.Errorf("should not show Skills when 0 tokens:\n%s", result)
	}
	if strings.Contains(result, "Tool results") {
		t.Errorf("should not show Tool results when 0 tokens:\n%s", result)
	}
}

// TestContextCommandExactTokens verifies exact token counts from CountTokensFn are used without estimates.
func TestContextCommandExactTokens(t *testing.T) {
	path := writeAPILog(t, nil) // no API log entries needed

	info := testContextInfo()
	info.CountTokensFn = func(ctx context.Context) (*TokenCounts, error) {
		return &TokenCounts{
			Total:        25000,
			System:       8000,
			Conversation: 15000,
			Tools:        2000,
			Sections: []SectionTokens{
				{Name: "Environment", Tokens: 300},
				{Name: "IDENTITY.md", Tokens: 2500},
				{Name: "SOUL.md", Tokens: 3200},
				{Name: "MEMORY.md", Tokens: 1500},
				{Name: "Skills", Tokens: 500},
			},
		}, nil
	}
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"Context: 25,000 / 200,000 tokens", // exact, no ~
		"System prompt: 8,000 tokens",
		"Environment",
		"IDENTITY.md",
		"SOUL.md",
		"MEMORY.md",
		"Skills",
		"2,500 tokens", // IDENTITY.md exact
		"Tools: 2,000 tokens",
		"Conversation: 15,000 tokens", // exact, no ~
		"User messages",               // per-role still shown
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
	// Header should NOT have ~ prefix (exact)
	if strings.Contains(result, "~25,000") {
		t.Errorf("exact total should not have ~ prefix:\n%s", result)
	}
	// Per-role conversation should have ~ (estimated)
	if !strings.Contains(result, "~") {
		t.Errorf("per-role estimates should have ~ prefix:\n%s", result)
	}
}

// TestContextCommandCountingAPIError verifies fallback to estimates when token counting fails.
func TestContextCommandCountingAPIError(t *testing.T) {
	now := time.Now().UTC()
	path := writeAPILog(t, []apiEntry{
		{Timestamp: now, Session: "agent:main:main", Input: 50000, CacheRead: 30000, CacheWrite: 10000, Output: 500},
	})

	info := testContextInfo()
	info.CountTokensFn = func(ctx context.Context) (*TokenCounts, error) {
		return nil, fmt.Errorf("API error")
	}
	cmd := NewContextCommand(path, func() ContextInfo { return info })

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should fall back to estimates
	if !strings.Contains(result, "~90,000") { // 50000 + 30000 + 10000
		t.Errorf("expected fallback to estimated ~90,000 tokens:\n%s", result)
	}
	if !strings.Contains(result, "System prompt: ~") {
		t.Errorf("expected estimated system prompt:\n%s", result)
	}
}

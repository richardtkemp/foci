package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/anthropic"
	"foci/internal/provider"
	"foci/internal/session"
)

// TestHasToolUse verifies tool_use detection.
func TestHasToolUse(t *testing.T) {
	if hasToolUse(provider.Message{Role: "user", Content: provider.TextContent("hi")}) {
		t.Error("plain user message should not have tool_use")
	}
	if !hasToolUse(toolUseMsg("toolu_1")) {
		t.Error("tool_use message should be detected")
	}
}

// TestToolUseIDs verifies extraction of tool_use IDs.
func TestToolUseIDs(t *testing.T) {
	ids := toolUseIDs(toolUseMsg("toolu_A", "toolu_B"))
	if len(ids) != 2 || ids[0] != "toolu_A" || ids[1] != "toolu_B" {
		t.Errorf("toolUseIDs = %v, want [toolu_A, toolu_B]", ids)
	}
}

// TestToolResultIDs verifies extraction of tool_result IDs.
func TestToolResultIDs(t *testing.T) {
	ids := toolResultIDs(toolResultMsg("toolu_X", "toolu_Y"))
	if !ids["toolu_X"] || !ids["toolu_Y"] || len(ids) != 2 {
		t.Errorf("toolResultIDs = %v, want {toolu_X, toolu_Y}", ids)
	}
}

// TestSafeSplitPointNoToolUse verifies split point calculation without tool_use.
func TestSafeSplitPointNoToolUse(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		{Role: "assistant", Content: provider.TextContent("a0")},
		{Role: "user", Content: provider.TextContent("u1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	// Split at 2 — no tool_use, should stay at 2.
	got := safeSplitPoint(msgs, 2, 25)
	if got != 2 {
		t.Errorf("safeSplitPoint = %d, want 2", got)
	}
}

// TestSafeSplitPointBreaksPair verifies walk-back when split breaks tool pair.
func TestSafeSplitPointBreaksPair(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},      // 0
		{Role: "assistant", Content: provider.TextContent("a0")}, // 1
		{Role: "user", Content: provider.TextContent("u1")},      // 2
		toolUseMsg("toolu_1"),    // 3: assistant tool_use
		toolResultMsg("toolu_1"), // 4: user tool_result
		{Role: "assistant", Content: provider.TextContent("done")}, // 5
	}
	// Split at 4 would separate tool_use (3) from tool_result (4).
	// Should walk back to 3.
	got := safeSplitPoint(msgs, 4, 25)
	if got != 3 {
		t.Errorf("safeSplitPoint = %d, want 3", got)
	}
}

// TestSafeSplitPointConsecutiveToolPairs verifies walk-back with consecutive tool_use.
func TestSafeSplitPointConsecutiveToolPairs(t *testing.T) {
	// In a corrupt session, two assistant tool_use messages in a row.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A"),                                       // 1: assistant tool_use (corrupt — no result follows)
		toolUseMsg("toolu_B"),                                       // 2: assistant tool_use
		toolResultMsg("toolu_B"),                                    // 3: user tool_result
		{Role: "assistant", Content: provider.TextContent("done")}, // 4
	}
	// Split at 3: prev is toolUseMsg("toolu_B") → walk to 2.
	// Split at 2: prev is toolUseMsg("toolu_A") → walk to 1.
	// Split at 1: prev is user text → stop.
	got := safeSplitPoint(msgs, 3, 25)
	if got != 1 {
		t.Errorf("safeSplitPoint = %d, want 1", got)
	}
}

// TestSafeSplitPointBounded verifies walk-back is bounded by maxWalkBack.
func TestSafeSplitPointBounded(t *testing.T) {
	// Walk-back bounded by maxWalkBack.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A"),                                       // 1
		toolUseMsg("toolu_B"),                                       // 2
		toolUseMsg("toolu_C"),                                       // 3
		toolResultMsg("toolu_C"),                                    // 4
		{Role: "assistant", Content: provider.TextContent("done")}, // 5
	}
	// Split at 4, maxWalkBack=2 → walks to 3, then 2, stops (2 steps).
	got := safeSplitPoint(msgs, 4, 2)
	if got != 2 {
		t.Errorf("safeSplitPoint = %d, want 2", got)
	}
}

// TestSafeSplitPointAtZero verifies split point at beginning of message list.
func TestSafeSplitPointAtZero(t *testing.T) {
	msgs := []provider.Message{
		toolUseMsg("toolu_1"),
		toolResultMsg("toolu_1"),
	}
	// Split at 0 — already at start, can't walk back.
	got := safeSplitPoint(msgs, 0, 25)
	if got != 0 {
		t.Errorf("safeSplitPoint = %d, want 0", got)
	}
}

// TestRepairOrphanedToolUseNoOrphans verifies repair skips balanced tool pairs.
func TestRepairOrphanedToolUseNoOrphans(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		toolResultMsg("toolu_1"),
		{Role: "assistant", Content: provider.TextContent("done")},
	}
	repaired := repairOrphanedToolUse(msgs)
	if len(repaired) != len(msgs) {
		t.Errorf("repaired has %d messages, want %d (no change)", len(repaired), len(msgs))
	}
}

// TestRepairOrphanedToolUseMissingResult verifies injection into existing user message.
func TestRepairOrphanedToolUseMissingResult(t *testing.T) {
	// Assistant has tool_use but no tool_result follows at all.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		// Missing: tool_result for toolu_1
		{Role: "user", Content: provider.TextContent("u1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject synthetic result into the existing user message at index 2.
	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}

	// The user message after tool_use should now have synthetic + original content.
	userMsg := repaired[2]
	if userMsg.Role != "user" {
		t.Fatalf("repaired[2].Role = %q, want user", userMsg.Role)
	}
	// Should have 2 blocks: synthetic tool_result + original text.
	if len(userMsg.Content) != 2 {
		t.Fatalf("repaired[2] has %d blocks, want 2", len(userMsg.Content))
	}
	if userMsg.Content[0].Type != "tool_result" || userMsg.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("repaired[2].Content[0] = %+v, want tool_result for toolu_1", userMsg.Content[0])
	}
	if !userMsg.Content[0].IsError {
		t.Error("synthetic tool_result should be is_error=true")
	}
}

// TestRepairOrphanedToolUseNoNextMessage verifies injection of new message when tool_use is last.
func TestRepairOrphanedToolUseNoNextMessage(t *testing.T) {
	// Tool_use is the last message — no following message at all.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject a standalone user message.
	if len(repaired) != 3 {
		t.Fatalf("repaired has %d messages, want 3", len(repaired))
	}
	if repaired[2].Role != "user" {
		t.Errorf("repaired[2].Role = %q, want user", repaired[2].Role)
	}
	if len(repaired[2].Content) != 1 || repaired[2].Content[0].Type != "tool_result" {
		t.Errorf("repaired[2] should be a single tool_result block")
	}
}

// TestRepairOrphanedToolUsePartialMatch verifies partial tool_result matching.
func TestRepairOrphanedToolUsePartialMatch(t *testing.T) {
	// Assistant has 2 tool_use blocks, but only 1 has a result.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_A", "toolu_B"),
		toolResultMsg("toolu_A"), // only A matched
		{Role: "assistant", Content: provider.TextContent("done")},
	}
	repaired := repairOrphanedToolUse(msgs)

	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}

	// User message should now have synthetic result for B + original result for A.
	userMsg := repaired[2]
	if len(userMsg.Content) != 2 {
		t.Fatalf("repaired[2] has %d blocks, want 2", len(userMsg.Content))
	}
	// Synthetic comes first (prepended).
	if userMsg.Content[0].ToolUseID != "toolu_B" || !userMsg.Content[0].IsError {
		t.Errorf("synthetic block = %+v, want tool_result for toolu_B with is_error", userMsg.Content[0])
	}
	if userMsg.Content[1].ToolUseID != "toolu_A" {
		t.Errorf("original block = %+v, want tool_result for toolu_A", userMsg.Content[1])
	}
}

// TestRepairOrphanedToolUseNextIsAssistant verifies insertion when tool_use followed by assistant.
func TestRepairOrphanedToolUseNextIsAssistant(t *testing.T) {
	// Corrupt: assistant tool_use followed by another assistant message.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("u0")},
		toolUseMsg("toolu_1"),
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	repaired := repairOrphanedToolUse(msgs)

	// Should inject a standalone user message between the two assistant messages.
	if len(repaired) != 4 {
		t.Fatalf("repaired has %d messages, want 4", len(repaired))
	}
	if repaired[2].Role != "user" || repaired[2].Content[0].Type != "tool_result" {
		t.Errorf("repaired[2] should be injected tool_result user message")
	}
	if repaired[3].Role != "assistant" {
		t.Errorf("repaired[3] should be original assistant message")
	}
}

// TestCompactSplitBreaksToolUsePair verifies compaction adjusts split point for tool pairs.
func TestCompactSplitBreaksToolUsePair(t *testing.T) {
	server := mockCompactionServer("Summary of tool conversation.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Build session: 5 text pairs + 1 tool pair + 1 text pair = 14 messages
	for i := 0; i < 5; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("u%d", i))})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("a%d", i))})
	}
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("run tool")})
	store.TestAppend(sessionKey, toolUseMsg("toolu_SPLIT"))
	store.TestAppend(sessionKey, toolResultMsg("toolu_SPLIT"))
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("tool done")})

	// preserve=3 would split between tool_use[11] and tool_result[12].
	// safeSplitPoint should adjust to 11, making preserve=3.
	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 3)

	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(sessionKey)

	// Verify no orphaned tool_use: every assistant with tool_use must be followed
	// by a user with matching tool_result.
	for i, msg := range msgs {
		if msg.Role != "assistant" {
			continue
		}
		ids := toolUseIDs(msg)
		if len(ids) == 0 {
			continue
		}
		if i+1 >= len(msgs) {
			t.Fatalf("assistant tool_use at end of compacted session (index %d)", i)
		}
		next := msgs[i+1]
		resultIDs := toolResultIDs(next)
		for _, id := range ids {
			if !resultIDs[id] {
				t.Errorf("orphaned tool_use %s at index %d — no matching tool_result", id, i)
			}
		}
	}

	// Verify role alternation.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
		}
	}
}

// TestCompactOrphanedToolUseInHistory verifies compaction handles orphaned tool_use in history.
func TestCompactOrphanedToolUseInHistory(t *testing.T) {
	server := mockCompactionServer("Summary of corrupt session.")
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	// Build session with an orphaned tool_use deep in history.
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u0")})
	store.TestAppend(sessionKey, toolUseMsg("toolu_ORPHAN"))
	// Missing tool_result — simulate data corruption.
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u1")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a1")})
	store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("u2")})
	store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("a2")})

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithConfig(4096, 4, 0) // no preservation — all messages summarized

	// This should not fail — repairOrphanedToolUse should inject synthetic results.
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact with orphaned tool_use: %v", err)
	}
}

// TestCompactWithEffortOverride verifies effort parameter is included in API request.
func TestCompactWithEffortOverride(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	c.WithEffort("high")
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	body := string(capturedBody)
	if !strings.Contains(body, `"effort":"high"`) {
		t.Errorf("API request body should contain effort=high, got: %s", body)
	}
	if !strings.Contains(body, `"output_config"`) {
		t.Errorf("API request body should contain output_config, got: %s", body)
	}
}

// TestCompactWithoutEffortOverride verifies effort is omitted when not set.
func TestCompactWithoutEffortOverride(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Summary."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, "claude-haiku-4-5", 0.8)
	// Not setting effort — should omit from request
	_, err := c.Compact(context.Background(), noStream(client), sessionKey, nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	body := string(capturedBody)
	if strings.Contains(body, `"effort"`) {
		t.Errorf("API request body should not contain effort when not set, got: %s", body)
	}
	if strings.Contains(body, `"output_config"`) {
		t.Errorf("API request body should not contain output_config when effort not set, got: %s", body)
	}
}

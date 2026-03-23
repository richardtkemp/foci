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

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
)

func TestHasToolUse(t *testing.T) {
	// Verifies that hasToolUse correctly distinguishes between a plain text
	// assistant message (no tool_use) and an assistant message that contains a tool_use
	// content block.
	if hasToolUse(provider.Message{Role: "user", Content: provider.TextContent("hi")}) {
		t.Error("plain user message should not have tool_use")
	}
	if !hasToolUse(toolUseMsg("toolu_1")) {
		t.Error("tool_use message should be detected")
	}
}

func TestToolUseIDs(t *testing.T) {
	// Verifies that toolUseIDs extracts all tool call IDs from an assistant
	// message in order, including messages with multiple tool_use blocks.
	ids := toolUseIDs(toolUseMsg("toolu_A", "toolu_B"))
	if len(ids) != 2 || ids[0] != "toolu_A" || ids[1] != "toolu_B" {
		t.Errorf("toolUseIDs = %v, want [toolu_A, toolu_B]", ids)
	}
}

func TestToolResultIDs(t *testing.T) {
	// Verifies that toolResultIDs returns a set containing all tool_use IDs
	// whose results appear in a user message, enabling O(1) lookup for orphan detection.
	ids := toolResultIDs(toolResultMsg("toolu_X", "toolu_Y"))
	if !ids["toolu_X"] || !ids["toolu_Y"] || len(ids) != 2 {
		t.Errorf("toolResultIDs = %v, want {toolu_X, toolu_Y}", ids)
	}
}

func TestSafeSplitPoint(t *testing.T) {
	// Verifies that safeSplitPoint adjusts the proposed split index to avoid
	// separating tool_use/tool_result pairs, respects maxWalkBack bounds, and
	// handles edge cases (no tool_use, consecutive tool_use, index 0).
	tests := []struct {
		name        string
		msgs        []provider.Message
		splitIdx    int
		maxWalkBack int
		want        int
	}{
		{
			name: "no tool_use stays at proposed split",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				{Role: "assistant", Content: provider.TextContent("a0")},
				{Role: "user", Content: provider.TextContent("u1")},
				{Role: "assistant", Content: provider.TextContent("a1")},
			},
			splitIdx: 2, maxWalkBack: 25, want: 2,
		},
		{
			name: "walks back to keep tool_use/result pair together",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				{Role: "assistant", Content: provider.TextContent("a0")},
				{Role: "user", Content: provider.TextContent("u1")},
				toolUseMsg("toolu_1"),
				toolResultMsg("toolu_1"),
				{Role: "assistant", Content: provider.TextContent("done")},
			},
			splitIdx: 4, maxWalkBack: 25, want: 3,
		},
		{
			name: "consecutive tool_use walks back through chain",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_A"),
				toolUseMsg("toolu_B"),
				toolResultMsg("toolu_B"),
				{Role: "assistant", Content: provider.TextContent("done")},
			},
			splitIdx: 3, maxWalkBack: 25, want: 1,
		},
		{
			name: "walk-back bounded by maxWalkBack",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_A"),
				toolUseMsg("toolu_B"),
				toolUseMsg("toolu_C"),
				toolResultMsg("toolu_C"),
				{Role: "assistant", Content: provider.TextContent("done")},
			},
			splitIdx: 4, maxWalkBack: 2, want: 2,
		},
		{
			name: "split at zero unchanged",
			msgs: []provider.Message{
				toolUseMsg("toolu_1"),
				toolResultMsg("toolu_1"),
			},
			splitIdx: 0, maxWalkBack: 25, want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeSplitPoint(tt.msgs, tt.splitIdx, tt.maxWalkBack); got != tt.want {
				t.Errorf("safeSplitPoint = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRepairOrphanedToolUse(t *testing.T) {
	// Verifies that repairOrphanedToolUse correctly handles: no orphans (no-op),
	// missing results, no next message, partial matches, and next-is-assistant.
	tests := []struct {
		name    string
		msgs    []provider.Message
		wantLen int
		check   func(t *testing.T, repaired []provider.Message)
	}{
		{
			name: "no orphans is no-op",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_1"),
				toolResultMsg("toolu_1"),
				{Role: "assistant", Content: provider.TextContent("done")},
			},
			wantLen: 4,
		},
		{
			name: "missing result injects synthetic into next user msg",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_1"),
				{Role: "user", Content: provider.TextContent("u1")},
				{Role: "assistant", Content: provider.TextContent("a1")},
			},
			wantLen: 4,
			check: func(t *testing.T, repaired []provider.Message) {
				userMsg := repaired[2]
				if userMsg.Role != "user" || len(userMsg.Content) != 2 {
					t.Fatalf("repaired[2] role=%q blocks=%d, want user/2", userMsg.Role, len(userMsg.Content))
				}
				if userMsg.Content[0].Type != "tool_result" || userMsg.Content[0].ToolUseID != "toolu_1" || !userMsg.Content[0].IsError {
					t.Errorf("repaired[2].Content[0] = %+v, want synthetic error tool_result for toolu_1", userMsg.Content[0])
				}
			},
		},
		{
			name: "no next message appends standalone user",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_1"),
			},
			wantLen: 3,
			check: func(t *testing.T, repaired []provider.Message) {
				if repaired[2].Role != "user" || len(repaired[2].Content) != 1 || repaired[2].Content[0].Type != "tool_result" {
					t.Errorf("repaired[2] should be a single tool_result user message")
				}
			},
		},
		{
			name: "partial match injects synthetic for unmatched IDs",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_A", "toolu_B"),
				toolResultMsg("toolu_A"),
				{Role: "assistant", Content: provider.TextContent("done")},
			},
			wantLen: 4,
			check: func(t *testing.T, repaired []provider.Message) {
				userMsg := repaired[2]
				if len(userMsg.Content) != 2 {
					t.Fatalf("repaired[2] has %d blocks, want 2", len(userMsg.Content))
				}
				if userMsg.Content[0].ToolUseID != "toolu_B" || !userMsg.Content[0].IsError {
					t.Errorf("synthetic block = %+v, want error tool_result for toolu_B", userMsg.Content[0])
				}
				if userMsg.Content[1].ToolUseID != "toolu_A" {
					t.Errorf("original block = %+v, want tool_result for toolu_A", userMsg.Content[1])
				}
			},
		},
		{
			name: "next is assistant injects standalone user between",
			msgs: []provider.Message{
				{Role: "user", Content: provider.TextContent("u0")},
				toolUseMsg("toolu_1"),
				{Role: "assistant", Content: provider.TextContent("a1")},
			},
			wantLen: 4,
			check: func(t *testing.T, repaired []provider.Message) {
				if repaired[2].Role != "user" || repaired[2].Content[0].Type != "tool_result" {
					t.Errorf("repaired[2] should be injected tool_result user message")
				}
				if repaired[3].Role != "assistant" {
					t.Errorf("repaired[3] should be original assistant message")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repaired := repairOrphanedToolUse(tt.msgs)
			if len(repaired) != tt.wantLen {
				t.Fatalf("repaired has %d messages, want %d", len(repaired), tt.wantLen)
			}
			if tt.check != nil {
				tt.check(t, repaired)
			}
		})
	}
}

func TestCompactSplitBreaksToolUsePair(t *testing.T) {
	// Verifies end-to-end that when the requested preserve
	// boundary falls between a tool_use and its tool_result, Compact adjusts the split to
	// keep the pair together, and the resulting compacted session has no orphaned tool_use
	// blocks and maintains strict role alternation.
	server := mockCompactionServer("Summary of tool conversation.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
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
	c := NewCompactor(store, 0.8)
	c.WithConfig(4096, 4, 3)

	_, newKey, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs, _ := store.Load(newKey)

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

func TestCompactOrphanedToolUseInHistory(t *testing.T) {
	// Verifies that Compact does not fail when the
	// session history contains a pre-existing orphaned tool_use (e.g. due to data corruption
	// or an aborted tool call) — repairOrphanedToolUse should patch the history before
	// sending it to the summarisation API.
	server := mockCompactionServer("Summary of corrupt session.")
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-key")
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

	c := NewCompactor(store, 0.8)
	c.WithConfig(4096, 4, 0) // no preservation — all messages summarized

	// This should not fail — repairOrphanedToolUse should inject synthetic results.
	_, _, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact with orphaned tool_use: %v", err)
	}
}

func TestCompactWithModelDefaults(t *testing.T) {
	// Verifies that ModelParamsFn provides effort to the compaction API
	// request for models that support it (Sonnet), and that the effort is
	// stripped for models that do not (Haiku).
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

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	// Sonnet supports effort — model defaults should apply
	c := NewCompactor(store, 0.8)
	c.ModelDefaultsFn = func(model string) config.ModelDefaults {
		return config.ModelDefaults{Effort: "high"}
	}
	_, _, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-sonnet-4-6", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	body := string(capturedBody)
	if !strings.Contains(body, `"effort":"high"`) {
		t.Errorf("API request body should contain effort=high from model defaults, got: %s", body)
	}

	// Haiku does not support effort — should be stripped even with model defaults
	store2 := session.NewStore(t.TempDir())
	for i := 0; i < 3; i++ {
		store2.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store2.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}
	c2 := NewCompactor(store2, 0.8)
	c2.ModelDefaultsFn = func(model string) config.ModelDefaults {
		return config.ModelDefaults{Effort: "high"}
	}
	_, _, err = c2.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
	if err != nil {
		t.Fatalf("Compact with haiku: %v", err)
	}
	body2 := string(capturedBody)
	if strings.Contains(body2, `"effort"`) {
		t.Errorf("API request body should NOT contain effort for haiku, got: %s", body2)
	}
}

func TestCompactWithoutEffortOverride(t *testing.T) {
	// Verifies that when no effort level is configured,
	// neither the "effort" field nor the "output_config" wrapper appear in the API request,
	// keeping the request minimal for models that don't use extended thinking.
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

	client := newTestAnthropicClient(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	sessionKey := "test/imain/1000000000"

	for i := 0; i < 3; i++ {
		store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("msg")})
		store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply")})
	}

	c := NewCompactor(store, 0.8)
	// Not setting effort — should omit from request
	_, _, err := c.Compact(context.Background(), noStream(client), sessionKey, "claude-haiku-4-5", "anthropic", nil, "", "", false)
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

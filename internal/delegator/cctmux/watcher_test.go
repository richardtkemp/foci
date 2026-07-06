package cctmux

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// TestProcessLine_ExtractsUsage verifies that usage is extracted from
// assistant messages and included in the TurnResult on end_turn.
func TestProcessLine_ExtractsUsage(t *testing.T) {
	w := &sessionWatcher{}

	var got *delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}
	w.setEvents(handler)

	// Simulate an assistant message with usage and end_turn.
	stopReason := "end_turn"
	entry := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"hello"}]`),
			StopReason: &stopReason,
			Usage: &usagePayload{
				InputTokens:              5000,
				OutputTokens:             300,
				CacheCreationInputTokens: 100,
				CacheReadInputTokens:     4000,
			},
		},
	}

	line, _ := json.Marshal(entry)
	w.processLine(line, handler)

	if got == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want %q", got.Text, "hello")
	}
	if got.Usage == nil {
		t.Fatal("Usage should not be nil")
	}
	if got.Usage.InputTokens != 5000 {
		t.Errorf("InputTokens = %d, want 5000", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", got.Usage.OutputTokens)
	}
	if got.Usage.CacheCreationInputTokens != 100 {
		t.Errorf("CacheCreationInputTokens = %d, want 100", got.Usage.CacheCreationInputTokens)
	}
	if got.Usage.CacheReadInputTokens != 4000 {
		t.Errorf("CacheReadInputTokens = %d, want 4000", got.Usage.CacheReadInputTokens)
	}
}

// TestProcessLine_UsageResetsAcrossTurns verifies that turnUsage doesn't
// leak from one turn to the next.
func TestProcessLine_UsageResetsAcrossTurns(t *testing.T) {
	w := &sessionWatcher{}

	var results []*delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { results = append(results, r) },
	}
	w.setEvents(handler)

	stopReason := "end_turn"

	// Turn 1: has usage.
	entry1 := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"turn1"}]`),
			StopReason: &stopReason,
			Usage:      &usagePayload{InputTokens: 1000, OutputTokens: 100},
		},
	}
	line1, _ := json.Marshal(entry1)
	w.processLine(line1, handler)

	// Turn 2: no usage field.
	entry2 := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"turn2"}]`),
			StopReason: &stopReason,
		},
	}
	line2, _ := json.Marshal(entry2)
	w.processLine(line2, handler)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Usage == nil {
		t.Error("turn 1 should have usage")
	}
	if results[1].Usage != nil {
		t.Error("turn 2 should NOT have usage (leaked from turn 1)")
	}
}

// TestProcessLine_LastUsageWins verifies that during a multi-message turn
// (tool loop), the last assistant message's usage is used.
func TestProcessLine_LastUsageWins(t *testing.T) {
	w := &sessionWatcher{}

	var got *delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}
	w.setEvents(handler)

	toolUse := "tool_use"
	endTurn := "end_turn"

	// First assistant message (tool_use, not end_turn).
	entry1 := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{}}]`),
			StopReason: &toolUse,
			Usage:      &usagePayload{InputTokens: 1000, OutputTokens: 50},
		},
	}
	line1, _ := json.Marshal(entry1)
	w.processLine(line1, handler)

	// Second assistant message (end_turn).
	entry2 := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"done"}]`),
			StopReason: &endTurn,
			Usage:      &usagePayload{InputTokens: 2000, OutputTokens: 100},
		},
	}
	line2, _ := json.Marshal(entry2)
	w.processLine(line2, handler)

	if got == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if got.Usage.InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000 (last message)", got.Usage.InputTokens)
	}
}

// --- handleAssistant tests ---

// TestHandleAssistant_TextEvent verifies that text content blocks are
// forwarded to the OnText callback and accumulated in turnText.
func TestHandleAssistant_TextEvent(t *testing.T) {
	w := &sessionWatcher{}

	var textEvents []string
	handler := &testEvents{
		OnText: func(text string) { textEvents = append(textEvents, text) },
	}

	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:    "assistant",
			Content: json.RawMessage(`[{"type":"text","text":"Hello there"}]`),
		},
	}

	w.handleAssistant(entry, handler)

	if len(textEvents) != 1 || textEvents[0] != "Hello there" {
		t.Fatalf("OnText events = %v, want [\"Hello there\"]", textEvents)
	}
	if w.turnText != "Hello there" {
		t.Errorf("turnText = %q, want %q", w.turnText, "Hello there")
	}
}

// TestHandleAssistant_ToolCallTracking verifies that tool_use blocks
// increment the tool counter and fire OnToolStart.
func TestHandleAssistant_ToolCallTracking(t *testing.T) {
	w := &sessionWatcher{}

	var toolStarts []string
	handler := &testEvents{
		OnToolStart: func(_ string, name, input string) { toolStarts = append(toolStarts, name) },
	}

	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:    "assistant",
			Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/tmp"}},{"type":"tool_use","id":"t2","name":"Write","input":{}}]`),
		},
	}

	w.handleAssistant(entry, handler)

	if w.turnTools != 2 {
		t.Errorf("turnTools = %d, want 2", w.turnTools)
	}
	if len(toolStarts) != 2 {
		t.Fatalf("OnToolStart called %d times, want 2", len(toolStarts))
	}
	if toolStarts[0] != "Read" || toolStarts[1] != "Write" {
		t.Errorf("tool names = %v, want [Read, Write]", toolStarts)
	}
}

// TestHandleAssistant_SyntheticNoResponsePassedThrough verifies that synthetic
// "no response" text blocks are passed through by the watcher — filtering is
// handled downstream by platform.IsSilent (bot SendText/SendTextToChat and Finalize).
func TestHandleAssistant_SyntheticNoResponsePassedThrough(t *testing.T) {
	for _, text := range []string{"No response requested.", "[[NO_RESPONSE]]"} {
		t.Run(text, func(t *testing.T) {
			w := &sessionWatcher{}

			var textEvents []string
			handler := &testEvents{
				OnText: func(text string) { textEvents = append(textEvents, text) },
			}

			content, _ := json.Marshal([]contentBlock{{Type: "text", Text: text}})
			entry := &sessionEntry{
				Type: "assistant",
				Message: &messagePayload{
					Role:    "assistant",
					Content: json.RawMessage(content),
				},
			}

			w.handleAssistant(entry, handler)

			if len(textEvents) != 1 {
				t.Errorf("OnText should fire once, got %d events", len(textEvents))
			}
			if w.turnText != text {
				t.Errorf("turnText = %q, want %q", w.turnText, text)
			}
		})
	}
}

// TestHandleAssistant_NilMessage verifies handleAssistant is a safe no-op
// when the entry has no message payload.
func TestHandleAssistant_NilMessage(t *testing.T) {
	w := &sessionWatcher{}
	handler := &testEvents{}

	entry := &sessionEntry{Type: "assistant", Message: nil}
	// Should not panic.
	w.handleAssistant(entry, handler)
}

// TestHandleAssistant_AgentTracking verifies that Agent tool_use blocks
// are tracked via the shared AgentTracker and status is reported.
func TestHandleAssistant_AgentTracking(t *testing.T) {
	w := &sessionWatcher{}

	var statusMessages []string
	w.agents.OnStatus = func(text string) { statusMessages = append(statusMessages, text) }

	handler := &testEvents{}

	agentInput, _ := json.Marshal(map[string]string{"description": "search files"})
	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_use", ID: "ag1", Name: "Agent", Input: agentInput},
	})
	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:    "assistant",
			Content: json.RawMessage(content),
		},
	}

	w.handleAssistant(entry, handler)

	if w.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", w.agents.Pending())
	}
	if len(statusMessages) != 1 {
		t.Fatalf("OnStatus called %d times, want 1", len(statusMessages))
	}
	want := "search files"
	if statusMessages[0] != want {
		t.Errorf("status = %q, want %q", statusMessages[0], want)
	}
}

// TestHandleAssistant_MultipleAgents verifies that spawning multiple agents
// in one message accumulates them correctly.
func TestHandleAssistant_MultipleAgents(t *testing.T) {
	w := &sessionWatcher{}

	var statusMessages []string
	w.agents.OnStatus = func(text string) { statusMessages = append(statusMessages, text) }

	handler := &testEvents{}

	input1, _ := json.Marshal(map[string]string{"description": "task A"})
	input2, _ := json.Marshal(map[string]string{"description": "task B"})
	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_use", ID: "ag1", Name: "Agent", Input: input1},
		{Type: "tool_use", ID: "ag2", Name: "Agent", Input: input2},
	})
	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:    "assistant",
			Content: json.RawMessage(content),
		},
	}

	w.handleAssistant(entry, handler)

	if w.agents.Pending() != 2 {
		t.Fatalf("Pending() = %d, want 2", w.agents.Pending())
	}
	// Add fires OnStatus for each agent individually.
	if len(statusMessages) != 2 {
		t.Fatalf("OnStatus called %d times, want 2", len(statusMessages))
	}
}

// TestHandleAssistant_EndTurnFiresResult verifies that an assistant message
// with stop_reason "end_turn" fires the turn result and resets turn state.
func TestHandleAssistant_EndTurnFiresResult(t *testing.T) {
	w := &sessionWatcher{}
	// Pre-accumulate some turn state.
	w.turnText = "earlier text"
	w.turnTools = 2

	var got *delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}

	endTurn := "end_turn"
	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"final answer"}]`),
			StopReason: &endTurn,
			Usage:      &usagePayload{InputTokens: 500, OutputTokens: 50},
			Model:      "claude-sonnet-4-20250514",
		},
	}

	w.handleAssistant(entry, handler)

	if got == nil {
		t.Fatal("OnTurnComplete was not called on end_turn")
	}
	if got.Text != "final answer" {
		t.Errorf("Text = %q, want %q", got.Text, "final answer")
	}
	// turnTools should include the pre-accumulated count (2) plus this message (0 tool_use).
	if got.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", got.ToolCalls)
	}
	if got.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-sonnet-4-20250514")
	}

	// Turn state should be reset.
	if w.turnText != "" {
		t.Errorf("turnText should be reset, got %q", w.turnText)
	}
	if w.turnTools != 0 {
		t.Errorf("turnTools should be reset, got %d", w.turnTools)
	}
}

// TestHandleAssistant_NonEndTurnDoesNotFire verifies that assistant messages
// with stop_reason other than "end_turn" (e.g. "tool_use") do not fire
// the turn result.
func TestHandleAssistant_NonEndTurnDoesNotFire(t *testing.T) {
	w := &sessionWatcher{}

	called := false
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { called = true },
	}

	toolUse := "tool_use"
	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{}}]`),
			StopReason: &toolUse,
		},
	}

	w.handleAssistant(entry, handler)

	if called {
		t.Fatal("OnTurnComplete should not fire for stop_reason=tool_use")
	}
}

// TestHandleAssistant_EmptyTextIgnored verifies that empty text blocks
// are not forwarded to OnText.
func TestHandleAssistant_EmptyTextIgnored(t *testing.T) {
	w := &sessionWatcher{}

	var textEvents []string
	handler := &testEvents{
		OnText: func(text string) { textEvents = append(textEvents, text) },
	}

	entry := &sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:    "assistant",
			Content: json.RawMessage(`[{"type":"text","text":""}]`),
		},
	}

	w.handleAssistant(entry, handler)

	if len(textEvents) != 0 {
		t.Errorf("OnText should not fire for empty text, got %v", textEvents)
	}
}

// --- handleUser tests ---

// TestHandleUser_AgentCompletion verifies that a tool_result matching a
// pending agent's tool_use_id removes it from the pending list and fires
// a status update.
func TestHandleUser_AgentCompletion(t *testing.T) {
	w := &sessionWatcher{}
	var statusMessages []string
	w.agents.OnStatus = func(text string) { statusMessages = append(statusMessages, text) }
	w.agents.Add("ag1", "task A")
	w.agents.Add("ag2", "task B")
	statusMessages = nil // clear Add notifications

	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_result", ToolUseID: "ag1"},
	})
	entry := &sessionEntry{
		Type: "user",
		Message: &messagePayload{
			Role:    "user",
			Content: json.RawMessage(content),
		},
	}

	w.handleUser(entry, &testEvents{})

	if w.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (ag1 should be removed)", w.agents.Pending())
	}
	if len(statusMessages) != 1 {
		t.Fatalf("OnStatus called %d times, want 1", len(statusMessages))
	}
	want := "task B"
	if statusMessages[0] != want {
		t.Errorf("status = %q, want %q", statusMessages[0], want)
	}
}

// TestHandleUser_AllAgentsComplete verifies that when the last pending agent
// completes, the status message includes "complete".
func TestHandleUser_AllAgentsComplete(t *testing.T) {
	w := &sessionWatcher{}
	var statusMessages []string
	w.agents.OnStatus = func(text string) { statusMessages = append(statusMessages, text) }
	w.agents.Add("ag1", "only task")
	statusMessages = nil // clear Add notification

	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_result", ToolUseID: "ag1"},
	})
	entry := &sessionEntry{
		Type: "user",
		Message: &messagePayload{
			Role:    "user",
			Content: json.RawMessage(content),
		},
	}

	w.handleUser(entry, &testEvents{})

	if w.agents.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", w.agents.Pending())
	}
	if len(statusMessages) != 1 {
		t.Fatalf("OnStatus called %d times, want 1", len(statusMessages))
	}
	if statusMessages[0] != "" {
		t.Errorf("expected empty cleared detail, got %q", statusMessages[0])
	}
}

// TestHandleUser_NoPendingAgents verifies handleUser is a no-op when
// there are no pending agents (common case for non-agent tool results).
func TestHandleUser_NoPendingAgents(t *testing.T) {
	w := &sessionWatcher{}

	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_result", ToolUseID: "t1"},
	})
	entry := &sessionEntry{
		Type: "user",
		Message: &messagePayload{
			Role:    "user",
			Content: json.RawMessage(content),
		},
	}

	// Should not panic.
	w.handleUser(entry, &testEvents{})
}

// TestHandleUser_NilMessage verifies handleUser is a safe no-op when
// the entry has no message payload.
func TestHandleUser_NilMessage(t *testing.T) {
	w := &sessionWatcher{}
	w.agents.Add("ag1", "")

	entry := &sessionEntry{Type: "user", Message: nil}
	w.handleUser(entry, &testEvents{})

	// Pending agents should be unchanged.
	if w.agents.Pending() != 1 {
		t.Fatalf("Pending() changed unexpectedly: %d", w.agents.Pending())
	}
}

// TestHandleUser_UnmatchedToolResult verifies that a tool_result with an
// ID that doesn't match any pending agent leaves the list unchanged.
func TestHandleUser_UnmatchedToolResult(t *testing.T) {
	w := &sessionWatcher{}
	statusCalled := false
	w.agents.OnStatus = func(text string) { statusCalled = true }
	w.agents.Add("ag1", "task")
	statusCalled = false // clear Add notification

	content, _ := json.Marshal([]contentBlock{
		{Type: "tool_result", ToolUseID: "unrelated-tool-id"},
	})
	entry := &sessionEntry{
		Type: "user",
		Message: &messagePayload{
			Role:    "user",
			Content: json.RawMessage(content),
		},
	}

	w.handleUser(entry, &testEvents{})

	if w.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (should not change)", w.agents.Pending())
	}
	if statusCalled {
		t.Error("OnStatus should not be called for unmatched result")
	}
}

// TestProcessLine_SkipsSidechainEntries proves that sidechain entries (sub-agent
// turns spawned via the Agent tool) are filtered out before dispatch — CC
// writes them into the same JSONL as the parent and we must not fire OnText /
// OnToolStart / OnToolEnd / OnTurnComplete for them on the parent handler.
func TestProcessLine_SkipsSidechainEntries(t *testing.T) {
	w := &sessionWatcher{}

	var textEvents []string
	var toolStarts []string
	var toolEnds []string
	handler := &testEvents{
		OnText:      func(text string) { textEvents = append(textEvents, text) },
		OnToolStart: func(_, name, _ string) { toolStarts = append(toolStarts, name) },
		OnToolEnd:   func(_, name, _ string, _ bool) { toolEnds = append(toolEnds, name) },
		OnTurnComplete: func(_ *delegator.TurnResult) {
			t.Error("OnTurnComplete fired for sidechain entry")
		},
	}

	// A sidechain assistant entry with both text and tool_use blocks.
	assistantLine := `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"sub-agent text"},{"type":"tool_use","id":"tu_nested","name":"Read","input":{}}],"stop_reason":"end_turn"}}`
	w.processLine([]byte(assistantLine), handler)

	// A sidechain user entry with a tool_result block.
	userLine := `{"type":"user","isSidechain":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_nested","content":"nested result"}]}}`
	w.processLine([]byte(userLine), handler)

	// A sidechain turn_duration system entry.
	systemLine := `{"type":"system","subtype":"turn_duration","isSidechain":true}`
	w.processLine([]byte(systemLine), handler)

	if len(textEvents) != 0 {
		t.Errorf("OnText fired for sidechain: %v", textEvents)
	}
	if len(toolStarts) != 0 {
		t.Errorf("OnToolStart fired for sidechain: %v", toolStarts)
	}
	if len(toolEnds) != 0 {
		t.Errorf("OnToolEnd fired for sidechain: %v", toolEnds)
	}
	// Per-turn state must not be touched by sidechain traffic.
	if w.turnText != "" {
		t.Errorf("turnText mutated by sidechain: %q", w.turnText)
	}
	if w.turnTools != 0 {
		t.Errorf("turnTools mutated by sidechain: %d", w.turnTools)
	}
}

// TestHandleUser_FiresToolEndWithName proves that handleAssistant records the
// id→name pairing from tool_use blocks and handleUser looks it up when
// dispatching OnToolEnd, so downstream consumers (tracker hint functions)
// see the originating tool name and can format result hints correctly.
func TestHandleUser_FiresToolEndWithName(t *testing.T) {
	w := &sessionWatcher{}

	type captured struct {
		id      string
		name    string
		output  string
		isError bool
	}
	var events []captured
	handler := &testEvents{
		OnToolStart: func(_ string, _ string, _ string) {},
		OnToolEnd: func(id, name, output string, isErr bool) {
			events = append(events, captured{id: id, name: name, output: output, isError: isErr})
		},
	}

	// Assistant fires first (records id→name).
	assistantContent, _ := json.Marshal([]contentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "Bash", Input: json.RawMessage(`{}`)},
	})
	w.handleAssistant(&sessionEntry{
		Type:    "assistant",
		Message: &messagePayload{Role: "assistant", Content: json.RawMessage(assistantContent)},
	}, handler)

	// User message carries the tool_result back; handleUser should find
	// the recorded name and pass it to OnToolEnd.
	userContent, _ := json.Marshal([]contentBlock{
		{Type: "tool_result", ToolUseID: "tu_1", Content: json.RawMessage(`"exit 0"`)},
	})
	w.handleUser(&sessionEntry{
		Type:    "user",
		Message: &messagePayload{Role: "user", Content: json.RawMessage(userContent)},
	}, handler)

	if len(events) != 1 {
		t.Fatalf("OnToolEnd calls = %d, want 1", len(events))
	}
	if events[0].id != "tu_1" || events[0].name != "Bash" {
		t.Errorf("OnToolEnd = %+v, want {id:tu_1 name:Bash}", events[0])
	}

	// The id→name entry should be removed after firing so it doesn't
	// leak across tool calls within the same turn.
	if _, ok := w.toolNamesByID["tu_1"]; ok {
		t.Error("toolNamesByID entry not cleared after OnToolEnd")
	}
}

// --- handleSystem tests ---

// TestHandleSystem_TurnDuration verifies that a system entry with
// subtype "turn_duration" fires the turn result.
func TestHandleSystem_TurnDuration(t *testing.T) {
	w := &sessionWatcher{}
	w.turnText = "accumulated text"
	w.turnTools = 3

	var got *delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}

	entry := &sessionEntry{
		Type:    "system",
		Subtype: "turn_duration",
	}

	w.handleSystem(entry, handler)

	if got == nil {
		t.Fatal("OnTurnComplete was not called for turn_duration")
	}
	if got.Text != "accumulated text" {
		t.Errorf("Text = %q, want %q", got.Text, "accumulated text")
	}
	if got.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", got.ToolCalls)
	}

	// Turn state should be reset.
	if w.turnText != "" || w.turnTools != 0 {
		t.Error("turn state should be reset after turn_duration")
	}
}

// TestHandleSystem_CompactBoundary verifies that a system entry with
// subtype "compact_boundary" fires the turn result (handles /compact
// which doesn't produce end_turn).
func TestHandleSystem_CompactBoundary(t *testing.T) {
	w := &sessionWatcher{}

	var got *delegator.TurnResult
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { got = r },
	}

	entry := &sessionEntry{
		Type:    "system",
		Subtype: "compact_boundary",
	}

	w.handleSystem(entry, handler)

	if got == nil {
		t.Fatal("OnTurnComplete was not called for compact_boundary")
	}
}

// TestHandleSystem_OtherSubtype verifies that system entries with unknown
// subtypes are ignored (no turn result fired).
func TestHandleSystem_OtherSubtype(t *testing.T) {
	w := &sessionWatcher{}

	called := false
	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { called = true },
	}

	for _, subtype := range []string{"", "init", "login", "unknown"} {
		entry := &sessionEntry{
			Type:    "system",
			Subtype: subtype,
		}
		w.handleSystem(entry, handler)
	}

	if called {
		t.Fatal("OnTurnComplete should not fire for non-turn-completion subtypes")
	}
}

// --- processLine dispatch tests ---

// TestProcessLine_DispatchesCorrectly verifies that processLine routes
// entries to the right handler based on the type field.
func TestProcessLine_DispatchesCorrectly(t *testing.T) {
	w := &sessionWatcher{}

	var textCalled bool
	var turnComplete bool
	handler := &testEvents{
		OnText:         func(text string) { textCalled = true },
		OnTurnComplete: func(r *delegator.TurnResult) { turnComplete = true },
	}

	// assistant type → handleAssistant
	endTurn := "end_turn"
	assistantEntry := sessionEntry{
		Type: "assistant",
		Message: &messagePayload{
			Role:       "assistant",
			Content:    json.RawMessage(`[{"type":"text","text":"hi"}]`),
			StopReason: &endTurn,
		},
	}
	line, _ := json.Marshal(assistantEntry)
	w.processLine(line, handler)

	if !textCalled {
		t.Error("assistant entry should trigger OnText")
	}
	if !turnComplete {
		t.Error("assistant entry with end_turn should trigger OnTurnComplete")
	}

	// system type → handleSystem
	turnComplete = false
	systemEntry := sessionEntry{Type: "system", Subtype: "turn_duration"}
	line, _ = json.Marshal(systemEntry)
	w.processLine(line, handler)

	if !turnComplete {
		t.Error("system/turn_duration should trigger OnTurnComplete")
	}
}

// TestProcessLine_IgnoresUnparseableLines verifies that malformed JSON
// is silently skipped without panicking.
func TestProcessLine_IgnoresUnparseableLines(t *testing.T) {
	w := &sessionWatcher{}
	handler := &testEvents{}

	// Should not panic.
	w.processLine([]byte("not valid json"), handler)
	w.processLine([]byte(""), handler)
	w.processLine([]byte("{broken"), handler)
}

// TestProcessLine_IgnoresUnknownTypes verifies that entry types like
// "progress" or "file-history-snapshot" are silently ignored.
func TestProcessLine_IgnoresUnknownTypes(t *testing.T) {
	w := &sessionWatcher{}

	called := false
	handler := &testEvents{
		OnText:         func(text string) { called = true },
		OnTurnComplete: func(r *delegator.TurnResult) { called = true },
	}

	for _, typ := range []string{"progress", "file-history-snapshot", "queue-operation", "unknown"} {
		entry := sessionEntry{Type: typ}
		line, _ := json.Marshal(entry)
		w.processLine(line, handler)
	}

	if called {
		t.Fatal("handlers should not fire for unknown entry types")
	}
}

// --- fireTurnResult tests ---

// TestFireTurnResult_ResetsAllState verifies that fireTurnResult clears
// all accumulated turn state (text, tools, usage, model).
func TestFireTurnResult_ResetsAllState(t *testing.T) {
	w := &sessionWatcher{}
	w.turnText = "some text"
	w.turnTools = 5
	w.turnUsage = &delegator.TurnUsage{InputTokens: 100}
	w.turnModel = "claude-opus-4-6"

	handler := &testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) {},
	}

	w.fireTurnResult(handler)

	if w.turnText != "" {
		t.Errorf("turnText = %q, want empty", w.turnText)
	}
	if w.turnTools != 0 {
		t.Errorf("turnTools = %d, want 0", w.turnTools)
	}
	if w.turnUsage != nil {
		t.Error("turnUsage should be nil after reset")
	}
	if w.turnModel != "" {
		t.Errorf("turnModel = %q, want empty", w.turnModel)
	}
}

// TestFireTurnResult_NilCallback verifies fireTurnResult doesn't panic
// when OnTurnComplete is nil (handler without completion tracking).
func TestFireTurnResult_NilCallback(t *testing.T) {
	w := &sessionWatcher{}
	w.turnText = "text"

	handler := &testEvents{} // OnTurnComplete is nil

	// Should not panic.
	w.fireTurnResult(handler)

	// State should still be reset.
	if w.turnText != "" {
		t.Errorf("turnText should be reset even with nil callback")
	}
}

// --- setEvents tests ---

// TestSetEvents verifies that setEvents stores the event sink
// thread-safely.
func TestSetEvents(t *testing.T) {
	w := &sessionWatcher{}

	handler := &testEvents{
		OnText: func(text string) {},
	}

	w.setEvents(handler)

	w.mu.Lock()
	h := w.events
	w.mu.Unlock()

	if h != handler {
		t.Fatal("events should be set after setEvents")
	}
}

// TestSetHandler_NilClears verifies that setting a nil sink clears it.
func TestSetEvents_NilClears(t *testing.T) {
	w := &sessionWatcher{}
	w.setEvents(&testEvents{})
	w.setEvents(nil)

	w.mu.Lock()
	h := w.events
	w.mu.Unlock()

	if h != nil {
		t.Fatal("events should be nil after setEvents(nil)")
	}
}

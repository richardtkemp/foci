package claudecode

import (
	"encoding/json"
	"testing"

	"foci/internal/backend"
)

// TestProcessLine_ExtractsUsage verifies that usage is extracted from
// assistant messages and included in the TurnResult on end_turn.
func TestProcessLine_ExtractsUsage(t *testing.T) {
	w := &sessionWatcher{}

	var got *backend.TurnResult
	handler := &backend.EventHandler{
		OnTurnComplete: func(r *backend.TurnResult) { got = r },
	}
	w.setHandler(handler)

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

	var results []*backend.TurnResult
	handler := &backend.EventHandler{
		OnTurnComplete: func(r *backend.TurnResult) { results = append(results, r) },
	}
	w.setHandler(handler)

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

	var got *backend.TurnResult
	handler := &backend.EventHandler{
		OnTurnComplete: func(r *backend.TurnResult) { got = r },
	}
	w.setHandler(handler)

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

package messages

import (
	"encoding/json"
	"testing"

	"foci/internal/provider"
)

func toolUseMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "test_tool",
			Input: json.RawMessage(`{}`),
		})
	}
	return provider.Message{Role: "assistant", Content: blocks}
}

func toolResultMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ToolResultBlock(id, "ok", false))
	}
	return provider.Message{Role: "user", Content: blocks}
}

func TestHasToolUse(t *testing.T) {
	// Verifies that HasToolUse correctly identifies messages with and without
	// tool_use content blocks.
	if HasToolUse(provider.Message{Role: "user", Content: provider.TextContent("hi")}) {
		t.Error("plain user message should not have tool_use")
	}
	if !HasToolUse(toolUseMsg("toolu_1")) {
		t.Error("tool_use message should be detected")
	}
}

func TestBlocksHaveToolUse(t *testing.T) {
	// Verifies the []ContentBlock variant works the same as HasToolUse
	// but accepts raw content blocks instead of a full message.
	if BlocksHaveToolUse(provider.TextContent("hi")) {
		t.Error("plain text blocks should not have tool_use")
	}
	blocks := []provider.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "tool_use", ID: "toolu_1", Name: "test"},
	}
	if !BlocksHaveToolUse(blocks) {
		t.Error("blocks with tool_use should be detected")
	}
}

func TestBlocksHaveToolUseEmpty(t *testing.T) {
	// Verifies that an empty or nil slice returns false.
	if BlocksHaveToolUse(nil) {
		t.Error("nil blocks should not have tool_use")
	}
	if BlocksHaveToolUse([]provider.ContentBlock{}) {
		t.Error("empty blocks should not have tool_use")
	}
}

func TestToolUseIDs(t *testing.T) {
	// Verifies that ToolUseIDs extracts all tool call IDs from a message
	// in order, including messages with multiple tool_use blocks.
	ids := ToolUseIDs(toolUseMsg("toolu_A", "toolu_B"))
	if len(ids) != 2 || ids[0] != "toolu_A" || ids[1] != "toolu_B" {
		t.Errorf("ToolUseIDs = %v, want [toolu_A, toolu_B]", ids)
	}
}

func TestToolUseIDsEmpty(t *testing.T) {
	// Verifies that a message with no tool_use blocks returns nil.
	ids := ToolUseIDs(provider.Message{Role: "user", Content: provider.TextContent("hi")})
	if ids != nil {
		t.Errorf("ToolUseIDs for plain message = %v, want nil", ids)
	}
}

func TestToolResultIDs(t *testing.T) {
	// Verifies that ToolResultIDs returns a set containing all tool_use IDs
	// whose results appear in a user message, enabling O(1) lookup.
	ids := ToolResultIDs(toolResultMsg("toolu_X", "toolu_Y"))
	if !ids["toolu_X"] || !ids["toolu_Y"] || len(ids) != 2 {
		t.Errorf("ToolResultIDs = %v, want {toolu_X, toolu_Y}", ids)
	}
}

func TestToolResultIDsEmpty(t *testing.T) {
	// Verifies that a message with no tool_result blocks returns an empty map.
	ids := ToolResultIDs(provider.Message{Role: "user", Content: provider.TextContent("hi")})
	if len(ids) != 0 {
		t.Errorf("ToolResultIDs for plain message = %v, want empty", ids)
	}
}

package provider

import (
	"encoding/json"
	"testing"
)

func TestTextContent(t *testing.T) {
	blocks := TextContent("hello")
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Errorf("TextContent = %+v", blocks)
	}
}

func TestTextOf(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "tool_use", Name: "exec"},
		{Type: "text", Text: "hello"},
	}
	if got := TextOf(blocks); got != "hello" {
		t.Errorf("TextOf = %q, want %q", got, "hello")
	}
}

func TestToolResultBlock(t *testing.T) {
	block := ToolResultBlock("tu_123", "result", false)
	if block.Type != "tool_result" || block.ToolUseID != "tu_123" || block.Content != "result" {
		t.Errorf("block = %+v", block)
	}
}

func TestContentBlockRoundTrip(t *testing.T) {
	block := ContentBlock{
		Type:  "tool_use",
		ID:    "tu_abc",
		Name:  "exec",
		Input: json.RawMessage(`{"command":"ls"}`),
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "tool_use" || decoded.ID != "tu_abc" || decoded.Name != "exec" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestNewCustomToolName(t *testing.T) {
	td := NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`))
	if td.Name() != "exec" {
		t.Errorf("Name() = %q, want exec", td.Name())
	}
}

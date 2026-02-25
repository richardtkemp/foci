package anthropic

import (
	"encoding/json"
	"testing"
)

func TestTextContent(t *testing.T) {
	blocks := TextContent("hello")
	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Errorf("block = %+v", blocks[0])
	}
	if blocks[0].CacheControl != nil {
		t.Error("unexpected cache control")
	}
}

func TestCachedTextContent(t *testing.T) {
	blocks := CachedTextContent("cached")
	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("cache control = %+v", blocks[0].CacheControl)
	}
}

func TestTextOf(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "tool_use", Name: "exec"},
		{Type: "text", Text: "hello"},
		{Type: "text", Text: "world"},
	}
	got := TextOf(blocks)
	if got != "hello" {
		t.Errorf("TextOf = %q, want %q", got, "hello")
	}
}

func TestTextOfEmpty(t *testing.T) {
	got := TextOf(nil)
	if got != "" {
		t.Errorf("TextOf(nil) = %q, want empty", got)
	}
}

func TestToolResultBlock(t *testing.T) {
	block := ToolResultBlock("tu_123", "result text", false)
	if block.Type != "tool_result" {
		t.Errorf("Type = %q", block.Type)
	}
	if block.ToolUseID != "tu_123" {
		t.Errorf("ToolUseID = %q", block.ToolUseID)
	}
	if block.Content != "result text" {
		t.Errorf("Content = %q", block.Content)
	}
	if block.IsError {
		t.Error("IsError should be false")
	}
}

func TestToolResultBlockError(t *testing.T) {
	block := ToolResultBlock("tu_456", "something failed", true)
	if !block.IsError {
		t.Error("IsError should be true")
	}
}

func TestContentBlockJSON(t *testing.T) {
	// Tool use block should marshal with correct fields
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

	var input struct{ Command string }
	json.Unmarshal(decoded.Input, &input)
	if input.Command != "ls" {
		t.Errorf("input.Command = %q", input.Command)
	}
}

func TestToolResultJSON(t *testing.T) {
	block := ToolResultBlock("tu_999", "file.txt contents", false)

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ContentBlock
	json.Unmarshal(data, &decoded)

	if decoded.Type != "tool_result" || decoded.ToolUseID != "tu_999" || decoded.Content != "file.txt contents" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestMessageRequestJSON(t *testing.T) {
	req := MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		System: []SystemBlock{
			{Type: "text", Text: "You are helpful."},
		},
		Messages: []Message{
			{Role: "user", Content: TextContent("hi")},
		},
		Tools: []ToolDef{
			{Name: "exec", Description: "run cmd", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MessageRequest
	json.Unmarshal(data, &decoded)

	if decoded.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q", decoded.Model)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "exec" {
		t.Errorf("Tools = %+v", decoded.Tools)
	}
}

func TestMessageResponseStopReason(t *testing.T) {
	jsonStr := `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "hello"}],
		"model": "claude-haiku-4-5",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`

	var resp MessageResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, "end_turn")
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d", resp.Usage.InputTokens)
	}
}

func TestImageBlock(t *testing.T) {
	block := ImageBlock("image/jpeg", "dGVzdGRhdGE=")
	if block.Type != "image" {
		t.Errorf("Type = %q, want %q", block.Type, "image")
	}
	if block.Source == nil {
		t.Fatal("Source is nil")
	}
	if block.Source.Type != "base64" {
		t.Errorf("Source.Type = %q, want %q", block.Source.Type, "base64")
	}
	if block.Source.MediaType != "image/jpeg" {
		t.Errorf("Source.MediaType = %q", block.Source.MediaType)
	}
	if block.Source.Data != "dGVzdGRhdGE=" {
		t.Errorf("Source.Data = %q", block.Source.Data)
	}
}

func TestImageBlockJSON(t *testing.T) {
	block := ImageBlock("image/png", "AAAA")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "image" {
		t.Errorf("decoded.Type = %q", decoded.Type)
	}
	if decoded.Source == nil || decoded.Source.MediaType != "image/png" {
		t.Errorf("decoded.Source = %+v", decoded.Source)
	}
}

func TestEphemeral(t *testing.T) {
	cc := Ephemeral()
	if cc.Type != "ephemeral" {
		t.Errorf("Type = %q", cc.Type)
	}
}

func TestThinkingConfigJSON(t *testing.T) {
	cfg := ThinkingConfig{Type: "adaptive"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ThinkingConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "adaptive" {
		t.Errorf("Type = %q, want %q", decoded.Type, "adaptive")
	}
	if decoded.BudgetTokens != 0 {
		t.Errorf("BudgetTokens = %d, want 0", decoded.BudgetTokens)
	}
}

func TestThinkingConfigOmitsEmpty(t *testing.T) {
	req := MessageRequest{
		Model:     "claude-opus-4-6",
		MaxTokens: 8192,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Thinking should be omitted when nil
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if _, ok := m["thinking"]; ok {
		t.Error("thinking should be omitted when nil")
	}
}

func TestThinkingConfigInRequest(t *testing.T) {
	req := MessageRequest{
		Model:     "claude-opus-4-6",
		MaxTokens: 8192,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
		Thinking:  &ThinkingConfig{Type: "adaptive"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MessageRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if decoded.Thinking.Type != "adaptive" {
		t.Errorf("Thinking.Type = %q, want %q", decoded.Thinking.Type, "adaptive")
	}
}

func TestThinkingContentBlock(t *testing.T) {
	jsonStr := `{"type": "thinking", "thinking": "Let me reason about this..."}`
	var block ContentBlock
	if err := json.Unmarshal([]byte(jsonStr), &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if block.Type != "thinking" {
		t.Errorf("Type = %q, want %q", block.Type, "thinking")
	}
	if block.Thinking != "Let me reason about this..." {
		t.Errorf("Thinking = %q", block.Thinking)
	}
}

func TestTextOfIgnoresThinking(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "thinking", Thinking: "internal reasoning"},
		{Type: "text", Text: "visible response"},
	}
	got := TextOf(blocks)
	if got != "visible response" {
		t.Errorf("TextOf = %q, want %q", got, "visible response")
	}
}

func TestTextOfOnlyThinking(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "thinking", Thinking: "only thinking, no text"},
	}
	got := TextOf(blocks)
	if got != "" {
		t.Errorf("TextOf = %q, want empty (thinking-only response)", got)
	}
}

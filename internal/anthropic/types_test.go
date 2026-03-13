package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTextContent(t *testing.T) {
	// Proves that TextContent wraps a string into a single-element content block slice with type "text".
	blocks := TextContent("hello")
	if len(blocks) != 1 {
		t.Fatalf("len = %d, want 1", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Errorf("block = %+v", blocks[0])
	}
}

func TestTextOf(t *testing.T) {
	// Proves that TextOf extracts only the text content from a mixed slice of blocks, ignoring non-text blocks such as tool_use.
	blocks := []ContentBlock{
		{Type: "tool_use", Name: "exec"},
		{Type: "text", Text: "hello"},
	}
	got := TextOf(blocks)
	if got != "hello" {
		t.Errorf("TextOf = %q, want %q", got, "hello")
	}
}

func TestTextOfMultipleBlocks(t *testing.T) {
	// Proves that TextOf joins multiple text blocks with double newlines and skips non-text blocks (server_tool_use, web_search_tool_result) in the middle.
	blocks := []ContentBlock{
		{Type: "text", Text: "before search"},
		{Type: "server_tool_use", Name: "web_search"},
		{Type: "web_search_tool_result"},
		{Type: "text", Text: "after search"},
	}
	got := TextOf(blocks)
	want := "before search\n\nafter search"
	if got != want {
		t.Errorf("TextOf = %q, want %q", got, want)
	}
}

func TestTextOfSkipsEmptyText(t *testing.T) {
	// Proves that TextOf skips text blocks whose text is an empty string when joining, so empty blocks don't produce spurious separators.
	blocks := []ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "text", Text: ""},
		{Type: "text", Text: "world"},
	}
	got := TextOf(blocks)
	want := "hello\n\nworld"
	if got != want {
		t.Errorf("TextOf = %q, want %q", got, want)
	}
}

func TestTextOfEmpty(t *testing.T) {
	// Proves that TextOf returns an empty string for a nil block slice rather than panicking.
	got := TextOf(nil)
	if got != "" {
		t.Errorf("TextOf(nil) = %q, want empty", got)
	}
}

func TestToolResultBlock(t *testing.T) {
	// Proves that ToolResultBlock constructs a correctly-typed tool_result block with the expected tool_use_id, content, and isError=false.
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
	// Proves that ToolResultBlock sets IsError=true when the error flag is passed, allowing callers to signal tool failures to the model.
	block := ToolResultBlock("tu_456", "something failed", true)
	if !block.IsError {
		t.Error("IsError should be true")
	}
}

func TestContentBlockJSON(t *testing.T) {
	// Proves that a tool_use ContentBlock round-trips through JSON with all fields (type, ID, name, and raw input) intact.
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
	// Proves that a tool_result ContentBlock serializes to and from JSON correctly, preserving type, tool_use_id, and content.
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
	// Proves that a full MessageRequest (including system, messages, and tools) serializes and deserializes correctly, with the tool name surviving the round-trip.
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
			NewCustomTool("exec", "run cmd", json.RawMessage(`{"type":"object"}`)),
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
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name() != "exec" {
		t.Errorf("Tools name = %q, want exec", decoded.Tools[0].Name())
	}
}

func TestMessageResponseStopReason(t *testing.T) {
	// Proves that a MessageResponse deserializes correctly from the API JSON format, with stop_reason and usage token counts populated.
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
	// Proves that ImageBlock constructs a correctly-typed image block with a base64 source containing the given MIME type and data.
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
	if block.Source.MimeType != "image/jpeg" {
		t.Errorf("Source.MimeType = %q", block.Source.MimeType)
	}
	if block.Source.Data != "dGVzdGRhdGE=" {
		t.Errorf("Source.Data = %q", block.Source.Data)
	}
}

func TestImageBlockJSON(t *testing.T) {
	// Proves that an image block round-trips through JSON with its type and MIME type preserved.
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
	if decoded.Source == nil || decoded.Source.MimeType != "image/png" {
		t.Errorf("decoded.Source = %+v", decoded.Source)
	}
}

func TestDocumentBlock(t *testing.T) {
	// Proves that DocumentBlock constructs a correctly-typed document block with a base64 source containing the given MIME type and data.
	block := DocumentBlock("application/pdf", "JVBER...")
	if block.Type != "document" {
		t.Errorf("Type = %q, want %q", block.Type, "document")
	}
	if block.Source == nil {
		t.Fatal("Source is nil")
	}
	if block.Source.Type != "base64" {
		t.Errorf("Source.Type = %q, want %q", block.Source.Type, "base64")
	}
	if block.Source.MimeType != "application/pdf" {
		t.Errorf("Source.MimeType = %q", block.Source.MimeType)
	}
	if block.Source.Data != "JVBER..." {
		t.Errorf("Source.Data = %q", block.Source.Data)
	}
}

func TestDocumentBlockJSON(t *testing.T) {
	// Proves that a document block round-trips through JSON with its type and MIME type preserved.
	block := DocumentBlock("application/pdf", "AAAA")
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "document" {
		t.Errorf("decoded.Type = %q", decoded.Type)
	}
	if decoded.Source == nil || decoded.Source.MimeType != "application/pdf" {
		t.Errorf("decoded.Source = %+v", decoded.Source)
	}
}

func TestThinkingConfigJSON(t *testing.T) {
	// Proves that a ThinkingConfig round-trips through JSON with its type preserved and zero-value BudgetTokens omitted.
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
	// Proves that the "thinking" key is omitted entirely from the JSON output when the Thinking field is nil, preventing the API from receiving an unexpected null value.
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
	// Proves that a MessageRequest containing a ThinkingConfig serializes and deserializes correctly, with the config type preserved in the round-trip.
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
	// Proves that a thinking content block from the API JSON is correctly deserialized, populating the Thinking field.
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
	// Proves that TextOf skips thinking blocks and returns only the visible text content, so internal reasoning does not leak into the response text.
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
	// Proves that TextOf returns an empty string when the response contains only a thinking block and no text blocks.
	blocks := []ContentBlock{
		{Type: "thinking", Thinking: "only thinking, no text"},
	}
	got := TextOf(blocks)
	if got != "" {
		t.Errorf("TextOf = %q, want empty (thinking-only response)", got)
	}
}

func TestNewCustomToolJSON(t *testing.T) {
	// Proves that a custom tool definition serializes to JSON with the correct name, description, and input_schema fields.
	td := NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`))
	if td.Name() != "exec" {
		t.Errorf("Name() = %q, want %q", td.Name(), "exec")
	}

	data, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if raw["name"] != "exec" {
		t.Errorf("name = %v", raw["name"])
	}
	if raw["description"] != "run commands" {
		t.Errorf("description = %v", raw["description"])
	}
	if raw["input_schema"] == nil {
		t.Error("input_schema missing")
	}
}

func TestNewServerToolJSON(t *testing.T) {
	// Proves that a server tool definition (which carries arbitrary extra fields like max_uses and allowed_domains) serializes all fields to JSON correctly.
	td := NewServerTool(map[string]interface{}{
		"type":            "web_search_20250305",
		"name":            "web_search",
		"max_uses":        5,
		"allowed_domains": []string{"example.com"},
	})
	if td.Name() != "web_search" {
		t.Errorf("Name() = %q, want %q", td.Name(), "web_search")
	}

	data, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if raw["type"] != "web_search_20250305" {
		t.Errorf("type = %v", raw["type"])
	}
	if raw["name"] != "web_search" {
		t.Errorf("name = %v", raw["name"])
	}
	// max_uses should be present (JSON number)
	if raw["max_uses"] == nil {
		t.Error("max_uses missing")
	}
}

func TestToolDefRoundTrip(t *testing.T) {
	// Proves that a ToolDef (specifically a server tool) survives a full JSON marshal/unmarshal round-trip with identical output, ensuring no data is lost when storing or transmitting tool definitions.
	original := NewServerTool(map[string]interface{}{
		"type": "web_search_20250305",
		"name": "web_search",
	})
	data, _ := json.Marshal(original)

	var decoded ToolDef
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Name() != "web_search" {
		t.Errorf("Name() = %q after round-trip", decoded.Name())
	}

	data2, _ := json.Marshal(decoded)
	if string(data) != string(data2) {
		t.Errorf("round-trip mismatch:\n  got:  %s\n  want: %s", data2, data)
	}
}

func TestContentBlockServerToolPassthrough(t *testing.T) {
	// Proves that a server_tool_use block from the API (with fields beyond what the struct models) round-trips through JSON losslessly using the Raw field, so extra server-tool fields are not discarded.
	serverJSON := `{
		"type": "server_tool_use",
		"id": "srvtoolu_abc",
		"name": "web_search",
		"input": {"query": "golang generics"}
	}`

	var block ContentBlock
	if err := json.Unmarshal([]byte(serverJSON), &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Struct fields should be populated from known JSON keys
	if block.Type != "server_tool_use" {
		t.Errorf("Type = %q", block.Type)
	}
	if block.ID != "srvtoolu_abc" {
		t.Errorf("ID = %q", block.ID)
	}
	if block.Name != "web_search" {
		t.Errorf("Name = %q", block.Name)
	}

	// Raw should be preserved
	if len(block.Raw) == 0 {
		t.Fatal("Raw is empty")
	}

	// Marshal should use Raw (preserving original JSON structure)
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip map[string]interface{}
	json.Unmarshal(data, &roundTrip)
	if roundTrip["type"] != "server_tool_use" {
		t.Errorf("round-trip type = %v", roundTrip["type"])
	}
	if roundTrip["id"] != "srvtoolu_abc" {
		t.Errorf("round-trip id = %v", roundTrip["id"])
	}
}

func TestContentBlockWebSearchResultPassthrough(t *testing.T) {
	// Proves that a web_search_tool_result block preserves all unknown fields (including encrypted_content and page_age) through a JSON round-trip via the Raw field, which is critical for passing server-encrypted content back to the API.
	resultJSON := `{
		"type": "web_search_tool_result",
		"tool_use_id": "srvtoolu_abc",
		"content": [
			{
				"type": "web_search_result",
				"title": "Example",
				"url": "https://example.com",
				"encrypted_content": "ENCRYPTED_BASE64_DATA",
				"page_age": "2025-01-15"
			}
		]
	}`

	var block ContentBlock
	if err := json.Unmarshal([]byte(resultJSON), &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if block.Type != "web_search_tool_result" {
		t.Errorf("Type = %q", block.Type)
	}

	// Marshal should use Raw — preserving encrypted_content
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	output := string(data)
	if !strings.Contains(output, "encrypted_content") {
		t.Error("encrypted_content lost during round-trip")
	}
	if !strings.Contains(output, "ENCRYPTED_BASE64_DATA") {
		t.Error("encrypted_content value lost during round-trip")
	}
	if !strings.Contains(output, "page_age") {
		t.Error("page_age lost during round-trip")
	}
}

func TestContentBlockKnownTypeUsesStruct(t *testing.T) {
	// Proves that well-known content block types (like "text") marshal from struct fields rather than the Raw passthrough, ensuring struct values take precedence.
	block := ContentBlock{Type: "text", Text: "hello"}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	json.Unmarshal(data, &decoded)
	if decoded["type"] != "text" {
		t.Errorf("type = %v", decoded["type"])
	}
	if decoded["text"] != "hello" {
		t.Errorf("text = %v", decoded["text"])
	}
}

func TestContentBlockToolUseRoundTrip(t *testing.T) {
	// Proves that tool_use blocks (a known type) continue to round-trip correctly after the custom marshal/unmarshal logic was added to support unknown types via Raw.
	original := ContentBlock{
		Type:  "tool_use",
		ID:    "tu_123",
		Name:  "exec",
		Input: json.RawMessage(`{"command":"ls"}`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != "tool_use" || decoded.ID != "tu_123" || decoded.Name != "exec" {
		t.Errorf("decoded = %+v", decoded)
	}
}


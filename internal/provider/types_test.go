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

func TestImageBlock(t *testing.T) {
	block := ImageBlock("image/jpeg", "base64data")
	if block.Type != "image" {
		t.Errorf("ImageBlock type = %q, want 'image'", block.Type)
	}
	if block.Source == nil || block.Source.Type != "base64" || block.Source.MimeType != "image/jpeg" {
		t.Errorf("ImageBlock source = %+v", block.Source)
	}
}

func TestDocumentBlock(t *testing.T) {
	block := DocumentBlock("application/pdf", "pdfbase64data")
	if block.Type != "document" {
		t.Errorf("DocumentBlock type = %q, want 'document'", block.Type)
	}
	if block.Source == nil || block.Source.Type != "base64" || block.Source.MimeType != "application/pdf" {
		t.Errorf("DocumentBlock source = %+v", block.Source)
	}
}

func TestNewServerTool(t *testing.T) {
	config := map[string]interface{}{
		"type": "web_search",
		"name": "search",
	}
	td := NewServerTool(config)
	if td.Name() != "search" {
		t.Errorf("NewServerTool Name() = %q, want search", td.Name())
	}
}

func TestToolDefRaw(t *testing.T) {
	td := NewCustomTool("test", "desc", json.RawMessage(`{}`))
	raw := td.Raw()
	if len(raw) == 0 {
		t.Errorf("Raw() returned empty message")
	}
}

func TestAPIErrorError(t *testing.T) {
	err := &APIError{StatusCode: 500, Body: "Internal Server Error"}
	msg := err.Error()
	if msg != "API error (status 500): Internal Server Error" {
		t.Errorf("Error() = %q", msg)
	}
}

func TestAPIErrorIsRateLimit(t *testing.T) {
	tests := []struct {
		status   int
		wantRate bool
	}{
		{429, true},
		{500, false},
		{200, false},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status}
		if err.IsRateLimit() != tc.wantRate {
			t.Errorf("IsRateLimit(%d) = %v, want %v", tc.status, err.IsRateLimit(), tc.wantRate)
		}
	}
}

func TestAPIErrorIsOverloaded(t *testing.T) {
	tests := []struct {
		status    int
		wantOvld  bool
	}{
		{529, true},
		{500, false},
		{200, false},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status}
		if err.IsOverloaded() != tc.wantOvld {
			t.Errorf("IsOverloaded(%d) = %v, want %v", tc.status, err.IsOverloaded(), tc.wantOvld)
		}
	}
}

func TestAPIErrorIsRetryable(t *testing.T) {
	tests := []struct {
		status    int
		wantRetry bool
	}{
		{500, true},
		{502, true},
		{503, true},
		{529, true},
		{429, false},
		{401, false},
		{200, false},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status}
		if err.IsRetryable() != tc.wantRetry {
			t.Errorf("IsRetryable(%d) = %v, want %v", tc.status, err.IsRetryable(), tc.wantRetry)
		}
	}
}

func TestAPIErrorIsAuthError(t *testing.T) {
	tests := []struct {
		status   int
		wantAuth bool
	}{
		{401, true},
		{403, false},
		{500, false},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status}
		if err.IsAuthError() != tc.wantAuth {
			t.Errorf("IsAuthError(%d) = %v, want %v", tc.status, err.IsAuthError(), tc.wantAuth)
		}
	}
}

func TestAPIErrorRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		retryAfter string
		wantSecs   int
	}{
		{"60", 60},
		{"120", 120},
		{"0", 0},
		{"", 0},
		{"invalid", 0},
	}
	for _, tc := range tests {
		err := &APIError{RetryAfter: tc.retryAfter}
		if got := err.RetryAfterSeconds(); got != tc.wantSecs {
			t.Errorf("RetryAfterSeconds(%q) = %d, want %d", tc.retryAfter, got, tc.wantSecs)
		}
	}
}

func TestContentBlockUnmarshalJSON_TextBlock(t *testing.T) {
	data := []byte(`{"type":"text","text":"hello"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "text" || cb.Text != "hello" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ToolResultStringContent(t *testing.T) {
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":"result"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_result" || cb.ToolUseID != "tu_123" || cb.Content != "result" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ToolResultArrayContent(t *testing.T) {
	// SDK format: content as array of text blocks
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":[{"type":"text","text":"result"}]}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_result" || cb.ToolUseID != "tu_123" || cb.Content != "result" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ServerTool(t *testing.T) {
	// Server tool with unknown type
	data := []byte(`{"type":"web_search_tool_result","id":"ws_123","name":"web_search"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "web_search_tool_result" {
		t.Errorf("decoded type = %q, want web_search_tool_result", cb.Type)
	}
	// Raw should be preserved
	if len(cb.Raw) == 0 {
		t.Errorf("Raw not preserved for server tool block")
	}
}

func TestContentBlockMarshalJSON_KnownType(t *testing.T) {
	cb := ContentBlock{
		Type: "text",
		Text: "hello",
	}
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ContentBlock
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "text" || decoded.Text != "hello" {
		t.Errorf("roundtrip = %+v", decoded)
	}
}

func TestContentBlockMarshalJSON_UnknownTypeWithRaw(t *testing.T) {
	rawData := []byte(`{"type":"web_search_tool_result","id":"ws_123"}`)
	var cb ContentBlock
	json.Unmarshal(rawData, &cb)

	// Marshal should use Raw for unknown type
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Should preserve the raw format
	if !json.Valid(data) {
		t.Errorf("marshaled data is not valid JSON: %s", string(data))
	}
}

func TestTextOf_MultipleBlocks(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "tool_use", Name: "exec"},
		{Type: "text", Text: "world"},
	}
	if got := TextOf(blocks); got != "hello\n\nworld" {
		t.Errorf("TextOf = %q, want %q", got, "hello\n\nworld")
	}
}

func TestTextOf_NoTextBlocks(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "tool_use", Name: "exec"},
	}
	if got := TextOf(blocks); got != "" {
		t.Errorf("TextOf with no text = %q, want ''", got)
	}
}

func TestTextOf_EmptyTextBlocks(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "text", Text: ""},
		{Type: "text", Text: "hello"},
	}
	if got := TextOf(blocks); got != "hello" {
		t.Errorf("TextOf with empty block = %q, want 'hello'", got)
	}
}

func TestToolDefMarshalJSON(t *testing.T) {
	td := NewCustomTool("test", "description", json.RawMessage(`{"type":"object"}`))
	data, err := json.Marshal(td)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("marshaled data is not valid JSON")
	}
}

func TestToolDefUnmarshalJSON(t *testing.T) {
	rawData := []byte(`{"name":"test","description":"desc"}`)
	var td ToolDef
	if err := json.Unmarshal(rawData, &td); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if td.Name() != "test" {
		t.Errorf("Name() = %q, want test", td.Name())
	}
}

func TestToolDefRoundTrip(t *testing.T) {
	original := NewCustomTool("myTool", "my description", json.RawMessage(`{"type":"object"}`))

	// Marshal
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal
	var restored ToolDef
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify
	if original.Name() != restored.Name() {
		t.Errorf("Name mismatch: %q vs %q", original.Name(), restored.Name())
	}
}

func TestContentBlockUnmarshalJSON_ImageBlock(t *testing.T) {
	data := []byte(`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"base64data"}}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "image" || cb.Source == nil || cb.Source.MimeType != "image/jpeg" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_DocumentBlock(t *testing.T) {
	data := []byte(`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"pdfdata"}}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "document" || cb.Source == nil || cb.Source.MimeType != "application/pdf" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ThinkingBlock(t *testing.T) {
	data := []byte(`{"type":"thinking","thinking":"internal reasoning"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "thinking" || cb.Thinking != "internal reasoning" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ToolUseBlock(t *testing.T) {
	data := []byte(`{"type":"tool_use","id":"tu_abc","name":"exec","input":{"command":"ls"}}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_use" || cb.ID != "tu_abc" || cb.Name != "exec" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_RedactedThinking(t *testing.T) {
	data := []byte(`{"type":"redacted_thinking","data":"encrypted","signature":"sig"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "redacted_thinking" || cb.Data != "encrypted" || cb.Signature != "sig" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_InvalidJSONForKnownType(t *testing.T) {
	// Known type with invalid JSON should fail
	data := []byte(`{"type":"text","text":123}`) // text should be string
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err == nil {
		t.Errorf("expected error for invalid JSON, got none")
	}
}

func TestContentBlockUnmarshalJSON_UnknownTypeInvalidJSON(t *testing.T) {
	// Unknown type with invalid JSON should still work (Raw is used)
	data := []byte(`{"type":"future_tool","id":"123","content":[1,2,3]}`) // invalid structure
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Errorf("unmarshal should tolerate unknown type with invalid JSON: %v", err)
	}
	if cb.Type != "future_tool" {
		t.Errorf("type not set for unknown block")
	}
}

func TestContentBlockUnmarshalJSON_ToolResultEmptyContent(t *testing.T) {
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":""}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_result" || cb.Content != "" {
		t.Errorf("decoded = %+v", cb)
	}
}

func TestContentBlockUnmarshalJSON_ToolResultMultipleBlocks(t *testing.T) {
	// SDK format with multiple text blocks - should extract first one
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_result" || cb.Content != "first" {
		t.Errorf("decoded = %+v, want Content 'first'", cb)
	}
}

// TestContentBlockUnmarshalJSON_ToolResultEmptyArray tests array format with no blocks
func TestContentBlockUnmarshalJSON_ToolResultEmptyArray(t *testing.T) {
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":[]}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Empty array: extractToolResultContent checks len(blocks) > 0
	// When false, it falls back to string(raw) = string([]) = "[]"
	if cb.Type != "tool_result" {
		t.Errorf("Type = %q, want tool_result", cb.Type)
	}
	if cb.Content != "[]" {
		t.Errorf("Content = %q, want [] (fallback to raw string)", cb.Content)
	}
}

// TestContentBlockUnmarshalJSON_ToolResultInvalidJSON tests fallback to raw string
func TestContentBlockUnmarshalJSON_ToolResultInvalidJSON(t *testing.T) {
	// Content that is neither string nor valid array format - should fallback to raw
	data := []byte(`{"type":"tool_result","tool_use_id":"tu_123","content":"raw-json-fallback"}`)
	var cb ContentBlock
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cb.Type != "tool_result" || cb.Content != "raw-json-fallback" {
		t.Errorf("decoded = %+v, want Content 'raw-json-fallback'", cb)
	}
}

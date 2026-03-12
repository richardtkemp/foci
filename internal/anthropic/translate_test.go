package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

func TestBuildSDKParamsBasic(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		System: []SystemBlock{
			{Type: "text", Text: "You are helpful."},
		},
		Messages: []Message{
			{Role: "user", Content: TextContent("Hello")},
		},
	}

	params := buildSDKParams(req)

	if string(params.Model) != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", params.Model)
	}
	if params.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", params.MaxTokens)
	}
	if len(params.System) != 1 {
		t.Fatalf("system len = %d, want 1", len(params.System))
	}
	if params.System[0].Text != "You are helpful." {
		t.Errorf("system text = %q", params.System[0].Text)
	}
	if len(params.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(params.Messages))
	}
	if string(params.Messages[0].Role) != "user" {
		t.Errorf("message role = %q", params.Messages[0].Role)
	}
}

// TestBuildSDKParamsWithEffort verifies effort is set for models that support it (Sonnet).
func TestBuildSDKParamsWithEffort(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Output:    &OutputConfig{Effort: "high"},
	}

	params := buildSDKParams(req)

	if string(params.OutputConfig.Effort) != "high" {
		t.Errorf("effort = %q, want high", params.OutputConfig.Effort)
	}
}

// TestStripUnsupportedParamsEffort verifies effort is silently dropped for Haiku,
// which does not support the effort parameter and returns a 400 error.
func TestStripUnsupportedParamsEffort(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Output:    &OutputConfig{Effort: "high"},
	}

	stripUnsupportedParams(req)

	if req.Output != nil {
		t.Errorf("Output should be nil for haiku, got %+v", req.Output)
	}
}

// TestBuildSDKParamsWithThinking verifies thinking is set for supported models (Sonnet).
func TestBuildSDKParamsWithThinking(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Thinking:  &ThinkingConfig{Type: "adaptive"},
	}

	params := buildSDKParams(req)

	if params.Thinking.OfAdaptive == nil {
		t.Error("expected adaptive thinking config")
	}
}

// TestStripUnsupportedParamsThinking verifies thinking is silently dropped for Haiku.
func TestStripUnsupportedParamsThinking(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Thinking:  &ThinkingConfig{Type: "adaptive"},
	}

	stripUnsupportedParams(req)

	if req.Thinking != nil {
		t.Error("Thinking should be nil for haiku")
	}
}

// TestStripUnsupportedParamsPreservedForSonnet verifies params are kept for supported models.
func TestStripUnsupportedParamsPreservedForSonnet(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Output:    &OutputConfig{Effort: "high"},
		Thinking:  &ThinkingConfig{Type: "adaptive"},
	}

	stripUnsupportedParams(req)

	if req.Output == nil || req.Output.Effort != "high" {
		t.Errorf("Output should be preserved for sonnet, got %+v", req.Output)
	}
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Errorf("Thinking should be preserved for sonnet, got %+v", req.Thinking)
	}
}

func TestBuildSDKParamsWithAutoCache(t *testing.T) {
	// Verify that CacheStrategy "auto" sets top-level CacheControl and system marker.
	req := &MessageRequest{
		Model:         "claude-haiku-4-5",
		MaxTokens:     1024,
		System:        []SystemBlock{{Type: "text", Text: "sys"}},
		Messages:      []Message{{Role: "user", Content: TextContent("Hi")}},
		CacheStrategy: "auto",
	}

	params := buildSDKParams(req)

	if string(params.CacheControl.Type) != "ephemeral" {
		t.Errorf("cache_control type = %q, want ephemeral", params.CacheControl.Type)
	}
	if string(params.System[0].CacheControl.Type) != "ephemeral" {
		t.Errorf("system block should have cache_control")
	}
}

func TestBuildSDKParamsWithTools(t *testing.T) {
	tool := NewCustomTool("search", "Search the web", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`))
	req := &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("Hi")}},
		Tools:     []ToolDef{tool},
	}

	params := buildSDKParams(req)

	if len(params.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(params.Tools))
	}
}

func TestContentBlockToSDKText(t *testing.T) {
	b := ContentBlock{Type: "text", Text: "hello world"}
	sdk := contentBlockToSDK(b)
	if sdk.OfText == nil {
		t.Fatal("expected OfText")
	}
	if sdk.OfText.Text != "hello world" {
		t.Errorf("text = %q", sdk.OfText.Text)
	}
}

func TestContentBlockToSDKToolResult(t *testing.T) {
	b := ContentBlock{
		Type:      "tool_result",
		ToolUseID: "tool_123",
		Content:   "success",
		IsError:   false,
	}
	sdk := contentBlockToSDK(b)
	if sdk.OfToolResult == nil {
		t.Fatal("expected OfToolResult")
	}
	if sdk.OfToolResult.ToolUseID != "tool_123" {
		t.Errorf("tool_use_id = %q", sdk.OfToolResult.ToolUseID)
	}
}

func TestContentBlockToSDKThinking(t *testing.T) {
	b := ContentBlock{
		Type:      "thinking",
		Thinking:  "let me think...",
		Signature: "sig123",
	}
	sdk := contentBlockToSDK(b)
	if sdk.OfThinking == nil {
		t.Fatal("expected OfThinking")
	}
	if sdk.OfThinking.Thinking != "let me think..." {
		t.Errorf("thinking = %q", sdk.OfThinking.Thinking)
	}
	if sdk.OfThinking.Signature != "sig123" {
		t.Errorf("signature = %q", sdk.OfThinking.Signature)
	}
}

func TestContentBlockToSDKImage(t *testing.T) {
	b := ContentBlock{
		Type: "image",
		Source: &ContentSource{
			Type:      "base64",
			MimeType: "image/jpeg",
			Data:      "abc123",
		},
	}
	sdk := contentBlockToSDK(b)
	if sdk.OfImage == nil {
		t.Fatal("expected OfImage")
	}
}

func TestContentBlockToSDKToolUse(t *testing.T) {
	b := ContentBlock{
		Type:  "tool_use",
		ID:    "tu_123",
		Name:  "search",
		Input: json.RawMessage(`{"query":"test"}`),
	}
	sdk := contentBlockToSDK(b)
	if sdk.OfToolUse == nil {
		t.Fatal("expected OfToolUse")
	}
	if sdk.OfToolUse.ID != "tu_123" {
		t.Errorf("id = %q", sdk.OfToolUse.ID)
	}
	if sdk.OfToolUse.Name != "search" {
		t.Errorf("name = %q", sdk.OfToolUse.Name)
	}
}

func TestContentBlockToSDKRedactedThinking(t *testing.T) {
	b := ContentBlock{Type: "redacted_thinking", Data: "encrypted"}
	sdk := contentBlockToSDK(b)
	if sdk.OfRedactedThinking == nil {
		t.Fatal("expected OfRedactedThinking")
	}
	if sdk.OfRedactedThinking.Data != "encrypted" {
		t.Errorf("data = %q", sdk.OfRedactedThinking.Data)
	}
}

func TestToolToSDKCustomTool(t *testing.T) {
	tool := NewCustomTool("search", "Search things", json.RawMessage(`{"type":"object","properties":{}}`))
	sdk := toolToSDK(tool)
	if sdk.OfTool == nil {
		t.Fatal("expected OfTool")
	}
	if sdk.OfTool.Name != "search" {
		t.Errorf("name = %q", sdk.OfTool.Name)
	}
}

func TestToolToSDKServerTool(t *testing.T) {
	tool := NewServerTool(map[string]interface{}{
		"type": "web_search_20250305",
		"name": "web_search",
	})
	// Server tools should use raw JSON passthrough
	sdk := toolToSDK(tool)
	// For server tools, neither OfTool nor the specialized fields will be set
	// (they use raw JSON override). Check it doesn't panic.
	_ = sdk
}

func TestClassifySDKErrorNil(t *testing.T) {
	if err := classifySDKError(nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestClassifySDKErrorNonAPI(t *testing.T) {
	err := classifySDKError(errors.New("some network error"))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	// Non-SDK errors should pass through unchanged.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Error("expected non-APIError for non-SDK error")
	}
}

func TestClassifySDKErrorStreamingOverloaded(t *testing.T) {
	// SDK streaming uses fmt.Errorf (not *sdk.Error) for SSE "error" events.
	sseErr := fmt.Errorf(`received error while streaming: {"type":"error","error":{"details":null,"type":"overloaded_error","message":"Overloaded"},"request_id":"req_abc123"}`)
	err := classifySDKError(sseErr)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 529 {
		t.Errorf("StatusCode = %d, want 529", apiErr.StatusCode)
	}
	if !apiErr.IsOverloaded() {
		t.Error("expected IsOverloaded() = true")
	}
	if !apiErr.IsRetryable() {
		t.Error("expected IsRetryable() = true")
	}
}

func TestClassifySDKErrorStreamingRateLimit(t *testing.T) {
	sseErr := fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`)
	err := classifySDKError(sseErr)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}

func TestClassifySDKErrorStreamingUnknownType(t *testing.T) {
	// Unknown error types should pass through unchanged.
	sseErr := fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"unknown_error","message":"wat"}}`)
	err := classifySDKError(sseErr)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Error("expected non-APIError for unknown streaming error type")
	}
}

func TestBuildSDKCountParams(t *testing.T) {
	req := &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		System: []SystemBlock{
			{Type: "text", Text: "System prompt"},
		},
		Messages: []Message{
			{Role: "user", Content: TextContent("count this")},
		},
	}

	params := buildSDKCountParams(req)

	if string(params.Model) != "claude-haiku-4-5" {
		t.Errorf("model = %q", params.Model)
	}
	if len(params.Messages) != 1 {
		t.Errorf("messages len = %d", len(params.Messages))
	}
}

func TestBuildSDKParamsWithCacheTTL(t *testing.T) {
	// Verify that CacheTTL propagates to cache markers via CacheStrategy.
	req := &MessageRequest{
		Model:         "claude-haiku-4-5",
		MaxTokens:     1024,
		System:        []SystemBlock{{Type: "text", Text: "sys"}},
		Messages:      []Message{{Role: "user", Content: TextContent("Hi")}},
		CacheStrategy: "auto",
		CacheTTL:      "1h",
	}

	params := buildSDKParams(req)

	if string(params.CacheControl.Type) != "ephemeral" {
		t.Errorf("cache_control type = %q, want ephemeral", params.CacheControl.Type)
	}
	if string(params.CacheControl.TTL) != "1h" {
		t.Errorf("cache_control ttl = %q, want 1h", params.CacheControl.TTL)
	}
	// System block should also have TTL
	if string(params.System[0].CacheControl.TTL) != "1h" {
		t.Errorf("system cache_control ttl = %q, want 1h", params.System[0].CacheControl.TTL)
	}
}

func TestApplyCacheMarkersAuto(t *testing.T) {
	// Auto strategy: top-level CacheControl + last system block marker.
	params := &sdk.MessageNewParams{
		System: []sdk.TextBlockParam{
			{Text: "first"},
			{Text: "second"},
		},
		Messages: []sdk.MessageParam{
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("hi")}},
		},
	}

	applyCacheMarkers(params, "auto", "")

	if string(params.CacheControl.Type) != "ephemeral" {
		t.Errorf("top-level cache_control type = %q, want ephemeral", params.CacheControl.Type)
	}
	if string(params.System[0].CacheControl.Type) != "" {
		t.Error("first system block should not have cache_control")
	}
	if string(params.System[1].CacheControl.Type) != "ephemeral" {
		t.Errorf("last system block should have cache_control")
	}
}

func TestApplyCacheMarkersExplicit(t *testing.T) {
	// Explicit strategy: last system block marker + second-to-last message marker.
	params := &sdk.MessageNewParams{
		System: []sdk.TextBlockParam{
			{Text: "sys"},
		},
		Messages: []sdk.MessageParam{
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("first")}},
			{Role: "assistant", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("reply")}},
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("second")}},
		},
	}

	applyCacheMarkers(params, "explicit", "1h")

	// System block should have marker with TTL
	if string(params.System[0].CacheControl.Type) != "ephemeral" {
		t.Errorf("system cache_control type = %q", params.System[0].CacheControl.Type)
	}
	if string(params.System[0].CacheControl.TTL) != "1h" {
		t.Errorf("system cache_control ttl = %q, want 1h", params.System[0].CacheControl.TTL)
	}
	// No top-level CacheControl for explicit
	if string(params.CacheControl.Type) != "" {
		t.Error("explicit strategy should not set top-level cache_control")
	}
	// Second-to-last message (index 1) should have marker
	msg := params.Messages[1]
	lastBlock := msg.Content[len(msg.Content)-1]
	if lastBlock.OfText == nil || string(lastBlock.OfText.CacheControl.Type) != "ephemeral" {
		t.Error("second-to-last message should have cache_control on last block")
	}
}

func TestApplyCacheMarkersExplicitToolResult(t *testing.T) {
	// Explicit strategy with tool_result as the breakpoint block.
	params := &sdk.MessageNewParams{
		System: []sdk.TextBlockParam{{Text: "sys"}},
		Messages: []sdk.MessageParam{
			{Role: "user", Content: []sdk.ContentBlockParamUnion{
				sdk.NewToolResultBlock("tu_1", "result", false),
			}},
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("next")}},
		},
	}

	applyCacheMarkers(params, "explicit", "")

	// The tool_result block in the first message should get the marker
	block := params.Messages[0].Content[0]
	if block.OfToolResult == nil || string(block.OfToolResult.CacheControl.Type) != "ephemeral" {
		t.Error("tool_result block should have cache_control as breakpoint")
	}
}

func TestApplyCacheMarkersNoSystem(t *testing.T) {
	// No system blocks — should not panic.
	params := &sdk.MessageNewParams{
		Messages: []sdk.MessageParam{
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("hi")}},
		},
	}

	applyCacheMarkers(params, "auto", "")

	if string(params.CacheControl.Type) != "ephemeral" {
		t.Errorf("top-level cache_control should still be set")
	}
}

func TestApplyCacheMarkersExplicitSingleMessage(t *testing.T) {
	// Only one message — no second-to-last, should not panic.
	params := &sdk.MessageNewParams{
		System: []sdk.TextBlockParam{{Text: "sys"}},
		Messages: []sdk.MessageParam{
			{Role: "user", Content: []sdk.ContentBlockParamUnion{sdk.NewTextBlock("only")}},
		},
	}

	applyCacheMarkers(params, "explicit", "")

	// System should still get marker
	if string(params.System[0].CacheControl.Type) != "ephemeral" {
		t.Error("system block should have cache_control")
	}
}

func TestSystemToSDKClean(t *testing.T) {
	// systemToSDK should produce clean blocks without cache markers.
	blocks := []SystemBlock{
		{Type: "text", Text: "first"},
		{Type: "text", Text: "second"},
	}
	result := systemToSDK(blocks)
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	for i, b := range result {
		if string(b.CacheControl.Type) != "" {
			t.Errorf("block[%d] should not have cache_control", i)
		}
	}
}

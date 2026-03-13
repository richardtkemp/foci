package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

func TestBuildSDKParamsBasic(t *testing.T) {
	// Proves that buildSDKParams correctly maps the core request fields (model, max_tokens, system blocks, and messages) to the SDK parameter types.
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

func TestBuildSDKParamsWithEffort(t *testing.T) {
	// Proves that the effort output config is passed through to SDK params for Sonnet, which supports the effort parameter.
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

func TestStripUnsupportedParamsEffort(t *testing.T) {
	// Proves that stripUnsupportedParams removes the Output/effort config for Haiku to avoid sending unsupported parameters that would cause a 400 error.
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

func TestBuildSDKParamsWithThinking(t *testing.T) {
	// Proves that the adaptive thinking config is passed through to SDK params for Sonnet, which supports extended thinking.
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

func TestStripUnsupportedParamsThinking(t *testing.T) {
	// Proves that stripUnsupportedParams removes the Thinking config for Haiku, which does not support extended thinking.
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

func TestStripUnsupportedParamsPreservedForSonnet(t *testing.T) {
	// Proves that stripUnsupportedParams leaves both Output/effort and Thinking configs intact for Sonnet, which supports both features.
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
	// Proves that CacheStrategy "auto" sets a top-level ephemeral CacheControl on the request and marks the last system block with a cache breakpoint.
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
	// Proves that tool definitions in the request are translated to the SDK tools slice.
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
	// Proves that a text ContentBlock converts to the SDK OfText union variant with its text preserved.
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
	// Proves that a tool_result ContentBlock converts to the SDK OfToolResult variant with the correct tool_use_id.
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
	// Proves that a thinking ContentBlock converts to the SDK OfThinking variant with both the thinking text and signature preserved.
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
	// Proves that an image ContentBlock with a base64 source converts to the SDK OfImage variant without error.
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
	// Proves that a tool_use ContentBlock converts to the SDK OfToolUse variant with its ID and name preserved.
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
	// Proves that a redacted_thinking ContentBlock converts to the SDK OfRedactedThinking variant with its encrypted data preserved.
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
	// Proves that a custom tool (with name, description, and input schema) converts to the SDK OfTool union variant with its name intact.
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
	// Proves that a server tool (which uses raw JSON passthrough for unknown fields) can be converted via toolToSDK without panicking.
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
	// Proves that classifySDKError is a no-op when passed nil, returning nil rather than a non-nil error.
	if err := classifySDKError(nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestClassifySDKErrorNonAPI(t *testing.T) {
	// Proves that a plain non-SDK error (e.g. a network error) passes through classifySDKError unchanged and is not wrapped as an *APIError.
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
	// Proves that an overloaded_error embedded in an SSE streaming error string is classified as an *APIError with status 529 and IsOverloaded()/IsRetryable() both true.
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
	// Proves that a rate_limit_error in an SSE streaming error string is classified as an *APIError with status 429.
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
	// Proves that an unrecognised error type in an SSE streaming error string is not classified as an *APIError — it passes through so unknown errors are not silently swallowed.
	sseErr := fmt.Errorf(`received error while streaming: {"type":"error","error":{"type":"unknown_error","message":"wat"}}`)
	err := classifySDKError(sseErr)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Error("expected non-APIError for unknown streaming error type")
	}
}

func TestBuildSDKCountParams(t *testing.T) {
	// Proves that buildSDKCountParams correctly maps the model and messages from a MessageRequest to the SDK count_tokens parameter types.
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
	// Proves that a non-empty CacheTTL is propagated to both the top-level CacheControl and the system block's cache marker when CacheStrategy is "auto".
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
	// Proves the "auto" cache strategy sets an ephemeral top-level CacheControl and marks only the last system block, leaving earlier system blocks untagged.
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
	// Proves the "explicit" cache strategy marks the last system block and the last content block of the second-to-last message (the shared prefix boundary), and does not set a top-level CacheControl.
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
	// Proves that when the second-to-last message ends with a tool_result block, the explicit strategy correctly attaches the cache breakpoint to that tool_result block.
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
	// Proves that applyCacheMarkers does not panic when there are no system blocks, and still sets the top-level CacheControl for the "auto" strategy.
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
	// Proves that applyCacheMarkers does not panic with the "explicit" strategy when there is only one message (no second-to-last message to mark), and still tags the system block.
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
	// Proves that systemToSDK converts system blocks without attaching any cache markers, since marker placement is handled separately by applyCacheMarkers.
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

func TestSdkRequestOptionsNoSpeed(t *testing.T) {
	// Proves that sdkRequestOptions with empty speed produces the default beta header without fast-mode.
	opts := sdkRequestOptions("test-token", "")
	// Should have exactly 2 options: auth token + beta header
	if len(opts) != 2 {
		t.Errorf("expected 2 options, got %d", len(opts))
	}
}

func TestSdkRequestOptionsFastSpeed(t *testing.T) {
	// Proves that sdkRequestOptions with speed="fast" adds the fast-mode beta and an extra WithJSONSet option.
	opts := sdkRequestOptions("test-token", "fast")
	// Should have 3 options: auth token + beta header + WithJSONSet("speed", "fast")
	if len(opts) != 3 {
		t.Errorf("expected 3 options, got %d", len(opts))
	}
}

func TestStripUnsupportedParamsSpeed(t *testing.T) {
	// Proves that stripUnsupportedParams clears Speed for Haiku (which doesn't support it) but preserves it for Opus.
	reqHaiku := &MessageRequest{
		Model: "claude-haiku-4-5",
		Speed: "fast",
	}
	stripUnsupportedParams(reqHaiku)
	if reqHaiku.Speed != "" {
		t.Errorf("Speed should be cleared for haiku, got %q", reqHaiku.Speed)
	}

	reqOpus := &MessageRequest{
		Model: "claude-opus-4-6",
		Speed: "fast",
	}
	stripUnsupportedParams(reqOpus)
	if reqOpus.Speed != "fast" {
		t.Errorf("Speed should be preserved for opus, got %q", reqOpus.Speed)
	}

	reqSonnet := &MessageRequest{
		Model: "claude-sonnet-4-6",
		Speed: "fast",
	}
	stripUnsupportedParams(reqSonnet)
	if reqSonnet.Speed != "" {
		t.Errorf("Speed should be cleared for sonnet, got %q", reqSonnet.Speed)
	}
}

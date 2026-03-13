package gemini

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"foci/internal/provider"

	"google.golang.org/genai"
)

func TestSystemToGenai(t *testing.T) {
	// Proves that multiple text system blocks are correctly converted to a single genai.Content with one Part per block.
	blocks := []provider.SystemBlock{
		{Type: "text", Text: "You are helpful."},
		{Type: "text", Text: "Be concise."},
	}

	content := systemToGenai(blocks)
	if content == nil {
		t.Fatal("nil content")
	}
	if len(content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(content.Parts))
	}
	if content.Parts[0].Text != "You are helpful." {
		t.Errorf("parts[0] = %q", content.Parts[0].Text)
	}
	if content.Parts[1].Text != "Be concise." {
		t.Errorf("parts[1] = %q", content.Parts[1].Text)
	}
}

func TestSystemToGenai_Empty(t *testing.T) {
	// Proves that nil input and blocks with empty text both produce nil, preventing empty cache entries from being created.
	content := systemToGenai(nil)
	if content != nil {
		t.Errorf("expected nil for empty blocks")
	}

	content = systemToGenai([]provider.SystemBlock{{Type: "text", Text: ""}})
	if content != nil {
		t.Errorf("expected nil for empty text blocks")
	}
}

func TestMessagesToGenai(t *testing.T) {
	// Proves that a basic user/assistant/user conversation is correctly translated, including the "assistant" → "model" role rename required by the Gemini API.
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi there")},
		{Role: "user", Content: provider.TextContent("how are you")},
	}

	contents := messagesToGenai(msgs)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}

	if contents[0].Role != "user" {
		t.Errorf("role[0] = %q, want user", contents[0].Role)
	}
	if contents[1].Role != "model" {
		t.Errorf("role[1] = %q, want model", contents[1].Role)
	}
	if contents[0].Parts[0].Text != "hello" {
		t.Errorf("text[0] = %q", contents[0].Parts[0].Text)
	}
}

func TestMessagesToGenai_ToolUse(t *testing.T) {
	// Proves that tool_use blocks are translated to FunctionCall parts and tool_result blocks to FunctionResponse parts, with args and output preserved.
	msgs := []provider.Message{
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "tu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu_1", Content: "file1\nfile2"},
		}},
	}

	contents := messagesToGenai(msgs)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}

	// Tool use → FunctionCall
	fc := contents[0].Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected FunctionCall")
	}
	if fc.Name != "exec" {
		t.Errorf("fc.Name = %q", fc.Name)
	}
	if fc.Args["cmd"] != "ls" {
		t.Errorf("fc.Args = %v", fc.Args)
	}

	// Tool result → FunctionResponse
	fr := contents[1].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("expected FunctionResponse")
	}
	if fr.Response["output"] != "file1\nfile2" {
		t.Errorf("fr.Response = %v", fr.Response)
	}
}

func TestMessagesToGenai_ToolResultError(t *testing.T) {
	// Proves that tool_result blocks marked as errors are translated with an "error" key in the FunctionResponse rather than "output", so the model can distinguish failure from success.
	msgs := []provider.Message{
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu_1", Content: "command not found", IsError: true},
		}},
	}

	contents := messagesToGenai(msgs)
	fr := contents[0].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("expected FunctionResponse")
	}
	if fr.Response["error"] != "command not found" {
		t.Errorf("fr.Response = %v, want error key", fr.Response)
	}
}

func TestMessagesToGenai_ToolResultNameLookup(t *testing.T) {
	// Proves that FunctionResponse.Name is resolved by looking up the matching tool_use ID when the tool_result block has no Name set — a requirement of the Gemini API.
	msgs := []provider.Message{
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "tu_1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			// Note: Name field is empty — should be looked up from previous tool_use
			{Type: "tool_result", ToolUseID: "tu_1", Content: "file1\nfile2"},
		}},
	}

	contents := messagesToGenai(msgs)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(contents))
	}

	fr := contents[1].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("expected FunctionResponse")
	}
	// The critical fix: Name should be "exec" (looked up from tu_1), not empty
	if fr.Name != "exec" {
		t.Errorf("fr.Name = %q, want exec (looked up from tool_use)", fr.Name)
	}
	if fr.Response["output"] != "file1\nfile2" {
		t.Errorf("fr.Response = %v", fr.Response)
	}
}

func TestMessagesToGenai_ToolResultNameFallback(t *testing.T) {
	// Proves that when a tool_result's ToolUseID has no matching tool_use in history, the block's own Name field is used as a safe fallback rather than producing an empty name.
	msgs := []provider.Message{
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu_unknown", Name: "fallback_name", Content: "result"},
		}},
	}

	contents := messagesToGenai(msgs)
	fr := contents[0].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("expected FunctionResponse")
	}
	// Should fall back to block.Name since lookup failed
	if fr.Name != "fallback_name" {
		t.Errorf("fr.Name = %q, want fallback_name", fr.Name)
	}
}

func TestToolsToGenai(t *testing.T) {
	// Proves that custom tool definitions are grouped into a single genai.Tool with multiple FunctionDeclarations, and that parameter schemas (including nested properties and required fields) are faithfully translated.
	defs := []provider.ToolDef{
		provider.NewCustomTool("exec", "run commands", json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "command to run"}
			},
			"required": ["command"]
		}`)),
		provider.NewCustomTool("read", "read files", json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string"}
			}
		}`)),
	}

	tools := toolsToGenai(defs)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1 (single Tool with multiple FunctionDeclarations)", len(tools))
	}

	decls := tools[0].FunctionDeclarations
	if len(decls) != 2 {
		t.Fatalf("decls = %d, want 2", len(decls))
	}

	if decls[0].Name != "exec" {
		t.Errorf("decls[0].Name = %q", decls[0].Name)
	}
	if decls[0].Description != "run commands" {
		t.Errorf("decls[0].Description = %q", decls[0].Description)
	}
	if decls[0].Parameters == nil {
		t.Fatal("decls[0].Parameters is nil")
	}
	if decls[0].Parameters.Type != genai.TypeObject {
		t.Errorf("params.Type = %v, want object", decls[0].Parameters.Type)
	}
	if decls[0].Parameters.Properties["command"] == nil {
		t.Error("missing 'command' property")
	}
}

func TestToolsToGenai_FiltersServerTools(t *testing.T) {
	// Proves that server-side tools (e.g. web_search) are excluded from the Gemini function declarations, since Gemini does not support that tool type natively.
	defs := []provider.ToolDef{
		provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`)),
		provider.NewServerTool(map[string]interface{}{
			"type": "web_search_20250305",
			"name": "web_search",
		}),
	}

	tools := toolsToGenai(defs)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].FunctionDeclarations[0].Name != "exec" {
		t.Errorf("expected exec, got %q", tools[0].FunctionDeclarations[0].Name)
	}
}

func TestToolsToGenai_Empty(t *testing.T) {
	// Proves that nil or empty tool definitions produce a nil result rather than an empty slice, keeping the API request clean.
	tools := toolsToGenai(nil)
	if tools != nil {
		t.Errorf("expected nil for empty defs")
	}
}

func TestThinkingToGenai(t *testing.T) {
	// Proves that thinking config is translated correctly: adaptive mode uses a default budget, explicit budgets are preserved, and nil input yields nil output.
	tc := thinkingToGenai(&provider.ThinkingConfig{Type: "adaptive"})
	if tc == nil {
		t.Fatal("nil config")
	}
	if !tc.IncludeThoughts {
		t.Error("IncludeThoughts should be true")
	}
	if tc.ThinkingBudget == nil || *tc.ThinkingBudget != 10000 {
		t.Errorf("budget = %v, want 10000", tc.ThinkingBudget)
	}

	// Explicit budget
	tc = thinkingToGenai(&provider.ThinkingConfig{BudgetTokens: 5000})
	if tc.ThinkingBudget == nil || *tc.ThinkingBudget != 5000 {
		t.Errorf("budget = %v, want 5000", tc.ThinkingBudget)
	}

	// Nil
	tc = thinkingToGenai(nil)
	if tc != nil {
		t.Error("expected nil for nil input")
	}
}

func TestResponseFromGenai(t *testing.T) {
	// Proves that a standard text response is correctly translated into the provider format, mapping finish reason, token counts, and text content.
	resp := &genai.GenerateContentResponse{
		ResponseID: "resp_123",
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: "Hello world"},
					},
					Role: "model",
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     100,
			CandidatesTokenCount: 20,
		},
	}

	result, err := responseFromGenai(resp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if result.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", result.StopReason)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("input = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 20 {
		t.Errorf("output = %d, want 20", result.Usage.OutputTokens)
	}

	text := provider.TextOf(result.Content)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
}

func TestResponseFromGenai_CachedTokensNonOverlapping(t *testing.T) {
	// Proves that Gemini's overlapping token counts (PromptTokenCount includes CachedContentTokenCount) are normalized so that InputTokens reflects only the non-cached portion, matching Anthropic's non-overlapping semantics.
	resp := &genai.GenerateContentResponse{
		ResponseID: "resp_cached",
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{{Text: "cached response"}},
					Role:  "model",
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:      150000,
			CandidatesTokenCount:  500,
			CachedContentTokenCount: 120000,
		},
	}

	result, err := responseFromGenai(resp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// InputTokens should be the NON-cached portion: 150000 - 120000 = 30000
	if result.Usage.InputTokens != 30000 {
		t.Errorf("InputTokens = %d, want 30000 (non-cached portion)", result.Usage.InputTokens)
	}
	if result.Usage.CacheReadInputTokens != 120000 {
		t.Errorf("CacheReadInputTokens = %d, want 120000", result.Usage.CacheReadInputTokens)
	}
	// Sum should equal the original PromptTokenCount
	total := result.Usage.InputTokens + result.Usage.CacheReadInputTokens
	if total != 150000 {
		t.Errorf("InputTokens + CacheRead = %d, want 150000 (original PromptTokenCount)", total)
	}
}

func TestResponseFromGenai_ToolUse(t *testing.T) {
	// Proves that a FunctionCall in the Gemini response is translated into a tool_use content block with the correct name, ID, and a "tool_use" stop reason for the agent loop.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								Name: "exec",
								Args: map[string]any{"cmd": "ls"},
								ID:   "call_1",
							},
						},
					},
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
	}

	result, err := responseFromGenai(resp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(result.Content) != 1 {
		t.Fatalf("content = %d, want 1", len(result.Content))
	}
	if result.Content[0].Type != "tool_use" {
		t.Errorf("type = %q, want tool_use", result.Content[0].Type)
	}
	if result.Content[0].Name != "exec" {
		t.Errorf("name = %q", result.Content[0].Name)
	}
	if result.Content[0].ID != "call_1" {
		t.Errorf("id = %q", result.Content[0].ID)
	}

	// StopReason should be "tool_use" for the agent loop
	if result.StopReason != "tool_use" {
		t.Errorf("stop = %q, want tool_use", result.StopReason)
	}
}

func TestResponseFromGenai_ToolUseGeneratesUniqueIDs(t *testing.T) {
	// Proves that multiple tool calls without Gemini-provided IDs each receive unique generated IDs, preventing collisions that would break tool result matching in the agent loop.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{FunctionCall: &genai.FunctionCall{Name: "todo", Args: map[string]any{"text": "a"}}},
						{FunctionCall: &genai.FunctionCall{Name: "todo", Args: map[string]any{"text": "b"}}},
						{FunctionCall: &genai.FunctionCall{Name: "todo", Args: map[string]any{"text": "c"}}},
					},
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
	}

	result, err := responseFromGenai(resp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(result.Content) != 3 {
		t.Fatalf("content = %d, want 3", len(result.Content))
	}

	seen := make(map[string]bool)
	for i, block := range result.Content {
		if block.ID == "" {
			t.Errorf("block %d has empty ID", i)
		}
		if seen[block.ID] {
			t.Errorf("block %d has duplicate ID %s", i, block.ID)
		}
		seen[block.ID] = true
		if block.Type != "tool_use" {
			t.Errorf("block %d type = %q, want tool_use", i, block.Type)
		}
	}
}

func TestResponseFromGenai_Thinking(t *testing.T) {
	// Proves that thought parts (Thought=true) are translated into "thinking" content blocks and normal text parts into "text" blocks, preserving their order.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: "Let me think...", Thought: true},
						{Text: "The answer is 42."},
					},
				},
				FinishReason: genai.FinishReasonStop,
			},
		},
	}

	result, err := responseFromGenai(resp, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if len(result.Content) != 2 {
		t.Fatalf("content = %d, want 2", len(result.Content))
	}
	if result.Content[0].Type != "thinking" {
		t.Errorf("type[0] = %q, want thinking", result.Content[0].Type)
	}
	if result.Content[0].Thinking != "Let me think..." {
		t.Errorf("thinking = %q", result.Content[0].Thinking)
	}
	if result.Content[1].Type != "text" {
		t.Errorf("type[1] = %q, want text", result.Content[1].Type)
	}
}

func TestMapFinishReason(t *testing.T) {
	// Proves that Gemini finish reasons map to the expected provider stop reason strings, including that safety and recitation map to "end_turn" rather than exposing internal Gemini codes.
	tests := []struct {
		reason genai.FinishReason
		want   string
	}{
		{genai.FinishReasonStop, "end_turn"},
		{genai.FinishReasonMaxTokens, "max_tokens"},
		{genai.FinishReasonSafety, "end_turn"},
		{genai.FinishReasonRecitation, "end_turn"},
	}

	for _, tt := range tests {
		if got := mapFinishReason(tt.reason); got != tt.want {
			t.Errorf("mapFinishReason(%v) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestJSONSchemaToGenai(t *testing.T) {
	// Proves that a JSON Schema object with nested properties, array items, descriptions, and required fields is faithfully converted into the equivalent genai.Schema structure.
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "user name"},
			"age": {"type": "integer"},
			"tags": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["name"]
	}`)

	schema, err := jsonSchemaToGenai(raw)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if schema.Type != genai.TypeObject {
		t.Errorf("type = %v", schema.Type)
	}
	if len(schema.Properties) != 3 {
		t.Fatalf("properties = %d, want 3", len(schema.Properties))
	}
	if schema.Properties["name"].Type != genai.TypeString {
		t.Errorf("name.type = %v", schema.Properties["name"].Type)
	}
	if schema.Properties["name"].Description != "user name" {
		t.Errorf("name.desc = %q", schema.Properties["name"].Description)
	}
	if schema.Properties["tags"].Type != genai.TypeArray {
		t.Errorf("tags.type = %v", schema.Properties["tags"].Type)
	}
	if schema.Properties["tags"].Items == nil || schema.Properties["tags"].Items.Type != genai.TypeString {
		t.Error("tags.items should be string")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" {
		t.Errorf("required = %v", schema.Required)
	}
}

func TestContextLimit(t *testing.T) {
	// Proves that each known Gemini model variant returns the correct context window size, and that unknown models fall back to the default limit.
	tests := []struct {
		model string
		want  int
	}{
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-2.0-flash", 1_000_000},
		{"gemini-1.5-pro", 2_000_000},
		{"unknown-model", 1_000_000},
	}

	for _, tt := range tests {
		if got := contextLimit(tt.model); got != tt.want {
			t.Errorf("contextLimit(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// Compile-time check: *Client implements provider.Client.
var _ provider.Client = (*Client)(nil)

func TestClassifyError(t *testing.T) {
	// Proves that error strings from the Gemini API (both numeric HTTP codes and gRPC status names) are classified into the correct provider.APIError status codes, nil passes through, and unrecognised errors are returned unchanged without being wrapped as APIError.
	t.Run("nil", func(t *testing.T) {
		if err := classifyError(nil); err != nil {
			t.Errorf("classifyError(nil) should return nil, got %v", err)
		}
	})

	// Cases that should produce a provider.APIError with a specific status code.
	apiErrTests := []struct {
		name   string
		msg    string
		status int
	}{
		{"rate limit 429", "429: too many requests", http.StatusTooManyRequests},
		{"resource exhausted", "RESOURCE_EXHAUSTED", http.StatusTooManyRequests},
		{"server error 500", "500: internal server error", http.StatusInternalServerError},
		{"internal", "INTERNAL: something broke", http.StatusInternalServerError},
		{"service unavailable 503", "503: service unavailable", http.StatusServiceUnavailable},
		{"unavailable", "UNAVAILABLE: service down", http.StatusServiceUnavailable},
		{"bad request 400", "400: bad request", http.StatusBadRequest},
		{"invalid argument", "INVALID_ARGUMENT: bad input", http.StatusBadRequest},
		{"unauthorized 401", "401: unauthorized", http.StatusUnauthorized},
		{"unauthenticated", "UNAUTHENTICATED: invalid key", http.StatusUnauthorized},
		{"forbidden 403", "403: forbidden", http.StatusForbidden},
		{"permission denied", "PERMISSION_DENIED: access not allowed", http.StatusForbidden},
		{"safety error", "SAFETY: content was filtered", http.StatusBadRequest},
		{"safety blocked", "request blocked by safety policy", http.StatusBadRequest},
		{"recitation", "RECITATION: cannot recite training data", http.StatusBadRequest},
	}
	for _, tt := range apiErrTests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyError(errors.New(tt.msg))
			apiErr := &provider.APIError{}
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected provider.APIError, got %T", err)
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tt.status)
			}
		})
	}

	t.Run("unknown", func(t *testing.T) {
		err := classifyError(errors.New("unknown error"))
		if err == nil {
			t.Error("expected non-nil error")
		}
		var apiErr *provider.APIError
		if errors.As(err, &apiErr) {
			t.Error("unknown error should not be classified as APIError")
		}
	})
}

func TestIsSafetyError(t *testing.T) {
	// Proves that safety-related error messages (SAFETY, RECITATION, "blocked") are correctly identified, while generic errors are not, ensuring proper retry/abort decisions in the agent loop.
	tests := []struct {
		msg  string
		want bool
	}{
		{"SAFETY: filtered", true},
		{"safety issue", true},
		{"RECITATION: blocked", true},
		{"blocked by policy", true},
		{"normal error", false},
		{"500 server error", false},
		{"not allowed", false},
	}

	for _, tt := range tests {
		got := isSafetyError(errors.New(tt.msg))
		if got != tt.want {
			t.Errorf("isSafetyError(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

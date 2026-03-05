package gemini

import (
	"encoding/json"
	"testing"

	"foci/internal/provider"

	"google.golang.org/genai"
)

func TestSystemToGenai(t *testing.T) {
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

func TestToolsToGenai(t *testing.T) {
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
	tools := toolsToGenai(nil)
	if tools != nil {
		t.Errorf("expected nil for empty defs")
	}
}

func TestThinkingToGenai(t *testing.T) {
	// Adaptive → default budget
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

func TestResponseFromGenai_ToolUse(t *testing.T) {
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

func TestResponseFromGenai_Thinking(t *testing.T) {
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

func TestStripDeveloperPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"google/gemini-2.5-pro", "gemini-2.5-pro"},
		{"gemini-2.5-flash", "gemini-2.5-flash"}, // no prefix
		{"", ""},                                   // empty
		{"no-slash-here", "no-slash-here"},
		{"foo/bar/baz", "bar/baz"},
	}

	for _, tt := range tests {
		got := stripDeveloperPrefix(tt.input)
		if got != tt.expected {
			t.Errorf("stripDeveloperPrefix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// Compile-time check: *Client implements provider.Client.
var _ provider.Client = (*Client)(nil)

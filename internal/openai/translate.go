package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/config"
	"foci/internal/messages"
	"foci/internal/provider"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// buildParams translates a provider.MessageRequest into OpenAI ChatCompletionNewParams.
func buildParams(req *provider.MessageRequest) openai.ChatCompletionNewParams {
	// Strip developer prefix (e.g., "openai/gpt-4o" → "gpt-4o")
	modelID := config.StripDeveloperPrefix(req.Model)

	params := openai.ChatCompletionNewParams{
		Model:    modelID,
		Messages: messagesToOpenAI(req.System, req.Messages),
	}

	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = param.NewOpt(int64(req.MaxTokens))
	}

	if tools := toolsToOpenAI(req.Tools); len(tools) > 0 {
		params.Tools = tools
	}

	// OpenRouter reasoning support: inject reasoning param when thinking is enabled.
	if req.Thinking != nil {
		params.SetExtraFields(map[string]any{
			"reasoning": map[string]any{"enabled": true},
		})
	}

	return params
}

// messagesToOpenAI converts system blocks and provider messages to OpenAI message params.
func messagesToOpenAI(system []provider.SystemBlock, msgs []provider.Message) []openai.ChatCompletionMessageParamUnion {
	var result []openai.ChatCompletionMessageParamUnion

	// System blocks → developer message (concatenated text)
	if len(system) > 0 {
		var parts []string
		for _, b := range system {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			result = append(result, openai.DeveloperMessage(strings.Join(parts, "\n\n")))
		}
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			result = append(result, userMessageToOpenAI(msg.Content)...)
		case "assistant":
			result = append(result, assistantMessageToOpenAI(msg.Content))
		}
	}

	return result
}

// userMessageToOpenAI converts user content blocks to OpenAI message params.
// tool_result blocks become separate ToolMessage entries.
func userMessageToOpenAI(blocks []provider.ContentBlock) []openai.ChatCompletionMessageParamUnion {
	var result []openai.ChatCompletionMessageParamUnion
	var parts []openai.ChatCompletionContentPartUnionParam

	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, openai.TextContentPart(b.Text))

		case "image":
			if b.Source != nil {
				dataURL := fmt.Sprintf("data:%s;base64,%s", b.Source.MimeType, b.Source.Data)
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL: dataURL,
				}))
			}

		case "tool_result":
			// Flush any accumulated user content first
			if len(parts) > 0 {
				result = append(result, openai.UserMessage(parts))
				parts = nil
			}
			content := b.Content
			if b.IsError {
				content = "Error: " + content
			}
			result = append(result, openai.ToolMessage(content, b.ToolUseID))
		}
	}

	// Flush remaining user content
	if len(parts) > 0 {
		result = append(result, openai.UserMessage(parts))
	}

	return result
}

// assistantMessageToOpenAI converts assistant content blocks to an OpenAI assistant message.
func assistantMessageToOpenAI(blocks []provider.ContentBlock) openai.ChatCompletionMessageParamUnion {
	var contentParts []string
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	var reasoningRaw json.RawMessage

	for _, b := range blocks {
		switch b.Type {
		case "text":
			contentParts = append(contentParts, b.Text)

		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: b.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      b.Name,
						Arguments: args,
					},
				},
			})

		case "thinking":
			if len(b.ReasoningRaw) > 0 {
				reasoningRaw = b.ReasoningRaw
			}

		// Skip redacted_thinking, server_tool_use — OpenAI-incompatible
		}
	}

	msg := openai.ChatCompletionAssistantMessageParam{
		ToolCalls: toolCalls,
	}

	text := strings.Join(contentParts, "\n\n")
	if text != "" {
		msg.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
			OfString: param.NewOpt(text),
		}
	}

	// Pass back reasoning_details for OpenRouter (faithful round-trip).
	if len(reasoningRaw) > 0 {
		msg.SetExtraFields(map[string]any{
			"reasoning_details": json.RawMessage(reasoningRaw),
		})
	}

	return openai.ChatCompletionMessageParamUnion{
		OfAssistant: &msg,
	}
}

// toolsToOpenAI converts provider tool definitions to OpenAI tool params.
// Server tools (web_search, etc.) are filtered out as they have no OpenAI equivalent.
func toolsToOpenAI(defs []provider.ToolDef) []openai.ChatCompletionToolUnionParam {
	var tools []openai.ChatCompletionToolUnionParam

	for _, td := range defs {
		raw := td.Raw()
		if raw == nil {
			continue
		}

		var parsed struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
			Type        string          `json:"type"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			continue
		}

		// Skip server tools (they have a "type" field like "web_search_20250305")
		if parsed.Type != "" {
			continue
		}

		fd := shared.FunctionDefinitionParam{
			Name: parsed.Name,
		}
		if parsed.Description != "" {
			fd.Description = param.NewOpt(parsed.Description)
		}
		if len(parsed.InputSchema) > 0 {
			var params shared.FunctionParameters
			if err := json.Unmarshal(parsed.InputSchema, &params); err == nil {
				fd.Parameters = params
			}
		}

		tools = append(tools, openai.ChatCompletionFunctionTool(fd))
	}

	return tools
}

// responseFromOpenAI translates an OpenAI response to provider format.
func responseFromOpenAI(resp *openai.ChatCompletion, model string) (*provider.MessageResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("openai: nil response")
	}

	result := &provider.MessageResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	// Usage
	result.Usage = provider.Usage{
		InputTokens:  int(resp.Usage.PromptTokens),
		OutputTokens: int(resp.Usage.CompletionTokens),
	}

	// Extract content from first choice
	if len(resp.Choices) == 0 {
		result.StopReason = "end_turn"
		result.Content = provider.TextContent("")
		return result, nil
	}

	choice := resp.Choices[0]

	// Map finish reason
	result.StopReason = mapFinishReason(choice.FinishReason)

	// Extract reasoning_details from OpenRouter responses (if present).
	if f, ok := choice.Message.JSON.ExtraFields["reasoning_details"]; ok && f.Valid() {
		rawJSON := json.RawMessage(f.Raw())
		thinkText := extractReasoningText(rawJSON)
		result.Content = append(result.Content, provider.ContentBlock{
			Type:         "thinking",
			Thinking:     thinkText,
			ReasoningRaw: rawJSON,
		})
	}

	// Text content
	if choice.Message.Content != "" {
		result.Content = append(result.Content, provider.ContentBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}

	// Tool calls
	for _, tc := range choice.Message.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		inputJSON := json.RawMessage(tc.Function.Arguments)
		// Validate it's valid JSON; if not, wrap as string
		if !json.Valid(inputJSON) {
			inputJSON, _ = json.Marshal(tc.Function.Arguments)
		}
		result.Content = append(result.Content, provider.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: inputJSON,
		})
	}

	// If no content blocks, add empty text
	if len(result.Content) == 0 {
		result.Content = provider.TextContent("")
	}

	// Override stop reason when response contains tool calls (same pattern as Gemini).
	if hasToolUse(result.Content) {
		result.StopReason = "tool_use"
	}

	return result, nil
}

// mapFinishReason converts OpenAI finish reasons to Anthropic-style stop reasons.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// hasToolUse checks if any content blocks are tool_use.
func hasToolUse(blocks []provider.ContentBlock) bool { return messages.BlocksHaveToolUse(blocks) }

// extractReasoningText best-effort extracts human-readable thinking text from
// OpenRouter reasoning_details JSON. The format varies by model:
//   - Plain string → use directly
//   - Array of objects → look for "thinking", "content", or "text" fields
//   - Fallback → raw JSON as text
func extractReasoningText(raw json.RawMessage) string {
	// Try plain string first.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}

	// Try array of objects with known text fields.
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		var parts []string
		for _, obj := range arr {
			for _, key := range []string{"thinking", "content", "text"} {
				if v, ok := obj[key]; ok {
					if text, ok := v.(string); ok && text != "" {
						parts = append(parts, text)
						break
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n")
		}
	}

	// Fallback: use raw JSON as text.
	return string(raw)
}

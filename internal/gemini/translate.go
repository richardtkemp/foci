package gemini

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"foci/internal/messages"
	"foci/internal/provider"

	"google.golang.org/genai"
)

// randomToolID generates a unique tool_use ID for Gemini responses that don't
// provide one. Without this, every call to the same tool gets an identical ID
// like "toolu_gemini_todo", which violates the API requirement for unique IDs.
func randomToolID(toolName string) string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("toolu_gemini_%s_%s", toolName, hex.EncodeToString(buf[:]))
}

// systemToGenai concatenates system prompt blocks into a single Gemini Content.
func systemToGenai(blocks []provider.SystemBlock) *genai.Content {
	var parts []*genai.Part
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, &genai.Part{Text: b.Text})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &genai.Content{Parts: parts, Role: "user"}
}

// messagesToGenai converts provider messages to Gemini Content slices.
func messagesToGenai(msgs []provider.Message) []*genai.Content {
	var contents []*genai.Content

	for _, msg := range msgs {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		var parts []*genai.Part
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue // skip empty text blocks — Gemini rejects them
				}
				parts = append(parts, &genai.Part{Text: block.Text})

			case "thinking":
				p := &genai.Part{
					Text:    block.Thinking,
					Thought: true,
				}
				if block.Signature != "" {
					p.ThoughtSignature, _ = base64.StdEncoding.DecodeString(block.Signature)
				}
				parts = append(parts, p)

			case "tool_use":
				args := make(map[string]any)
				if len(block.Input) > 0 {
					_ = json.Unmarshal(block.Input, &args) // best effort parsing, empty args on failure
				}
				p := &genai.Part{
					FunctionCall: &genai.FunctionCall{
						Name: block.Name,
						Args: args,
						ID:   block.ID,
					},
				}
				if block.Signature != "" {
					p.ThoughtSignature, _ = base64.StdEncoding.DecodeString(block.Signature)
				}
				parts = append(parts, p)

			case "tool_result":
				resp := map[string]any{"output": block.Content}
				if block.IsError {
					resp = map[string]any{"error": block.Content}
				}
				// Look up the tool name by matching the ToolUseID to a previous tool_use
				name := toolResultName(msgs, block.ToolUseID)
				if name == "" {
					// Fall back to block.Name if lookup fails (shouldn't happen in normal flow)
					name = block.Name
				}
				parts = append(parts, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						Name:     name,
						Response: resp,
						ID:       block.ToolUseID,
					},
				})

			case "image":
				if block.Source != nil {
					parts = append(parts, &genai.Part{
						InlineData: &genai.Blob{
							MIMEType: block.Source.MimeType,
							Data:     []byte(block.Source.Data),
						},
					})
				}

				// Skip server_tool_use, web_search_tool_result, etc. — Gemini-incompatible
			}
		}

		if len(parts) > 0 {
			contents = append(contents, &genai.Content{Parts: parts, Role: role})
		}
	}

	return contents
}

// toolsToGenai converts provider tool definitions to Gemini tools.
// Server tools (web_search, etc.) are filtered out as they have no Gemini equivalent.
func toolsToGenai(defs []provider.ToolDef) []*genai.Tool {
	var decls []*genai.FunctionDeclaration

	for _, td := range defs {
		raw := td.Raw()
		if raw == nil {
			continue
		}

		// Parse to check if this is a custom tool (has input_schema)
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

		decl := &genai.FunctionDeclaration{
			Name:        parsed.Name,
			Description: parsed.Description,
		}

		// Convert JSON Schema to genai.Schema
		if len(parsed.InputSchema) > 0 {
			schema, err := jsonSchemaToGenai(parsed.InputSchema)
			if err == nil {
				decl.Parameters = schema
			}
		}

		decls = append(decls, decl)
	}

	if len(decls) == 0 {
		return nil
	}

	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// jsonSchemaToGenai converts a JSON Schema (as used by Anthropic) to a genai.Schema.
func jsonSchemaToGenai(raw json.RawMessage) (*genai.Schema, error) {
	var js struct {
		Type        string                     `json:"type"`
		Description string                     `json:"description"`
		Properties  map[string]json.RawMessage `json:"properties"`
		Required    []string                   `json:"required"`
		Items       json.RawMessage            `json:"items"`
		Enum        []string                   `json:"enum"`
	}
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, err
	}

	schema := &genai.Schema{
		Description: js.Description,
		Required:    js.Required,
		Enum:        js.Enum,
	}

	switch js.Type {
	case "string":
		schema.Type = genai.TypeString
	case "number":
		schema.Type = genai.TypeNumber
	case "integer":
		schema.Type = genai.TypeInteger
	case "boolean":
		schema.Type = genai.TypeBoolean
	case "array":
		schema.Type = genai.TypeArray
	case "object":
		schema.Type = genai.TypeObject
	}

	// Recurse into properties
	if len(js.Properties) > 0 {
		schema.Properties = make(map[string]*genai.Schema, len(js.Properties))
		for name, propRaw := range js.Properties {
			propSchema, err := jsonSchemaToGenai(propRaw)
			if err != nil {
				continue
			}
			schema.Properties[name] = propSchema
		}
	}

	// Recurse into items
	if len(js.Items) > 0 {
		itemSchema, err := jsonSchemaToGenai(js.Items)
		if err == nil {
			schema.Items = itemSchema
		}
	}

	return schema, nil
}

// thinkingToGenai converts provider thinking config to Gemini thinking config.
func thinkingToGenai(tc *provider.ThinkingConfig) *genai.ThinkingConfig {
	if tc == nil {
		return nil
	}

	cfg := &genai.ThinkingConfig{
		IncludeThoughts: true,
	}

	if tc.BudgetTokens > 0 {
		budget := int32(tc.BudgetTokens) // #nosec G115 - token budgets are within int32 range
		cfg.ThinkingBudget = &budget
	} else if tc.Type == "adaptive" {
		// Anthropic "adaptive" → sensible default for Gemini
		budget := int32(10000)
		cfg.ThinkingBudget = &budget
	}

	return cfg
}

// responseFromGenai translates a Gemini response to provider format.
func responseFromGenai(resp *genai.GenerateContentResponse, model string) (*provider.MessageResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("gemini: nil response")
	}

	result := &provider.MessageResponse{
		ID:    resp.ResponseID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	// Usage — Gemini's PromptTokenCount is the TOTAL input including cached tokens.
	// CachedContentTokenCount is a subset, not additive. Subtract to match
	// Anthropic's non-overlapping semantics (InputTokens + CacheRead = total).
	if resp.UsageMetadata != nil {
		cached := int(resp.UsageMetadata.CachedContentTokenCount)
		result.Usage = provider.Usage{
			InputTokens:          int(resp.UsageMetadata.PromptTokenCount) - cached,
			OutputTokens:         int(resp.UsageMetadata.CandidatesTokenCount),
			CacheReadInputTokens: cached,
		}
	}

	// Extract content from candidates
	if len(resp.Candidates) == 0 {
		result.StopReason = "end_turn"
		return result, nil
	}

	candidate := resp.Candidates[0]

	// Map finish reason
	result.StopReason = mapFinishReason(candidate.FinishReason)

	// Convert parts to content blocks
	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				fc := part.FunctionCall
				inputJSON, _ := provider.MarshalRaw(fc.Args)
				id := fc.ID
				if id == "" {
					id = randomToolID(fc.Name)
				}
				cb := provider.ContentBlock{
					Type:  "tool_use",
					ID:    id,
					Name:  fc.Name,
					Input: inputJSON,
				}
				if len(part.ThoughtSignature) > 0 {
					cb.Signature = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
				}
				result.Content = append(result.Content, cb)
			} else if part.Thought {
				cb := provider.ContentBlock{
					Type:     "thinking",
					Thinking: part.Text,
				}
				if len(part.ThoughtSignature) > 0 {
					cb.Signature = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
				}
				result.Content = append(result.Content, cb)
			} else if part.Text != "" {
				result.Content = append(result.Content, provider.ContentBlock{
					Type: "text",
					Text: part.Text,
				})
			}
		}
	}

	// If no content blocks, add placeholder text.
	// An empty text block ("") is rejected by both Gemini and Anthropic APIs.
	if len(result.Content) == 0 {
		result.Content = provider.TextContent("(empty response)")
	}

	// Override stop reason to "tool_use" when response contains function calls.
	// The agent loop checks StopReason == "tool_use" to decide whether to
	// execute tools and continue the loop.
	if hasToolUse(result.Content) {
		result.StopReason = "tool_use"
	}

	return result, nil
}

// mapFinishReason converts Gemini finish reasons to Anthropic-style stop reasons.
func mapFinishReason(reason genai.FinishReason) string {
	switch reason {
	case genai.FinishReasonStop:
		return "end_turn"
	case genai.FinishReasonMaxTokens:
		return "max_tokens"
	case genai.FinishReasonSafety:
		return "end_turn" // safety filtered — treat as end
	case genai.FinishReasonRecitation:
		return "end_turn" // recitation filtered — treat as end
	case genai.FinishReasonMalformedFunctionCall:
		return "end_turn"
	default:
		return "end_turn"
	}
}

// hasToolUse reports whether any content block is a tool_use.
func hasToolUse(blocks []provider.ContentBlock) bool { return messages.BlocksHaveToolUse(blocks) }

// toolResultName looks up the tool name for a tool_result block by finding
// the matching tool_use in previous messages. Gemini needs function names
// in FunctionResponse.
func toolResultName(msgs []provider.Message, toolUseID string) string {
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.ID == toolUseID {
				return block.Name
			}
		}
	}
	return ""
}

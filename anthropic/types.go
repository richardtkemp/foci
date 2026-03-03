package anthropic

import (
	"encoding/json"
	"strings"
)

// CacheControl marks a content block for prompt caching.
type CacheControl struct {
	Type string `json:"type"`
}

// ContentSource holds base64-encoded data for image and document content blocks.
type ContentSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", "application/pdf", etc.
	Data      string `json:"data"`       // base64-encoded data
}

// knownBlockTypes lists content block types fully modeled by struct fields.
// Server tool types (server_tool_use, web_search_tool_result, web_fetch_tool_result)
// are NOT in this set — they use Raw passthrough.
var knownBlockTypes = map[string]bool{
	"text": true, "image": true, "document": true, "tool_use": true, "tool_result": true,
	"thinking": true, "redacted_thinking": true,
}

// ContentBlock is a block of content in a message or response.
// Known types (text, image, tool_use, tool_result, thinking, redacted_thinking)
// are modeled by struct fields. Unknown types (server_tool_use, web_search_tool_result,
// web_fetch_tool_result) are preserved verbatim via the Raw field.
type ContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`  // thinking: internal reasoning text
	Signature    string          `json:"signature,omitempty"` // thinking: encrypted verification signature (must be preserved)
	Data         string          `json:"data,omitempty"`      // redacted_thinking: encrypted thinking data
	Source       *ContentSource  `json:"source,omitempty"`    // image/document: base64 source
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	ID           string          `json:"id,omitempty"`        // tool_use / server_tool_use: block ID
	Name         string          `json:"name,omitempty"`      // tool_use / server_tool_use: tool name
	Input        json.RawMessage `json:"input,omitempty"`     // tool_use / server_tool_use: parameters
	ToolUseID    string          `json:"tool_use_id,omitempty"` // tool_result: references tool_use block
	Content      string          `json:"content,omitempty"`   // tool_result: result text
	IsError      bool            `json:"is_error,omitempty"`  // tool_result: error flag
	Raw          json.RawMessage `json:"-"`                   // complete JSON for passthrough (server tool blocks)
}

// contentBlockAlias avoids infinite recursion in custom marshal/unmarshal.
type contentBlockAlias ContentBlock

// UnmarshalJSON stores the raw JSON for all block types, then populates struct fields.
// For unknown types (server tool blocks), Raw is authoritative — struct unmarshal errors
// are tolerated since server tool JSON may have fields that clash with struct tags
// (e.g. "content" is a string in tool_result but an array in web_search_tool_result).
func (cb *ContentBlock) UnmarshalJSON(data []byte) error {
	cb.Raw = append(json.RawMessage(nil), data...)

	// Extract the type first to determine error handling strategy.
	var peek struct{ Type string `json:"type"` }
	_ = json.Unmarshal(data, &peek)

	type alias contentBlockAlias
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		if knownBlockTypes[peek.Type] {
			return err // fail hard for known types
		}
		// Unknown type: struct unmarshal may fail on field type mismatches.
		// Populate only the Type field from peek; Raw is authoritative.
		cb.Type = peek.Type
		// Try to extract common fields (id, name, input) that don't clash.
		var common struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(data, &common)
		cb.ID = common.ID
		cb.Name = common.Name
		cb.Input = common.Input
		return nil
	}
	*cb = ContentBlock(a)
	cb.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON uses Raw for unknown block types (preserves encrypted_content etc.),
// and normal struct marshaling for known types.
func (cb ContentBlock) MarshalJSON() ([]byte, error) {
	if len(cb.Raw) > 0 && !knownBlockTypes[cb.Type] {
		return cb.Raw, nil
	}
	type alias contentBlockAlias
	return json.Marshal(alias(cb))
}

// ToolDef can represent either a custom tool or a server tool.
// Custom tools have name/description/input_schema.
// Server tools have type/name and provider-specific fields.
// Use NewCustomTool() or NewServerTool() constructors.
type ToolDef struct {
	raw json.RawMessage
}

// MarshalJSON serializes the tool definition.
func (t ToolDef) MarshalJSON() ([]byte, error) { return t.raw, nil }

// UnmarshalJSON deserializes a tool definition.
func (t *ToolDef) UnmarshalJSON(data []byte) error {
	t.raw = append(json.RawMessage(nil), data...)
	return nil
}

// Name returns the tool name from the raw JSON (works for both custom and server tools).
func (t ToolDef) Name() string {
	var v struct{ Name string `json:"name"` }
	_ = json.Unmarshal(t.raw, &v)
	return v.Name
}

// NewCustomTool creates a ToolDef for a client-side tool.
func NewCustomTool(name, description string, inputSchema json.RawMessage) ToolDef {
	m := map[string]interface{}{
		"name":         name,
		"description":  description,
		"input_schema": json.RawMessage(inputSchema),
	}
	raw, _ := json.Marshal(m)
	return ToolDef{raw: raw}
}

// NewServerTool creates a ToolDef for an Anthropic server-side tool.
// The config map is serialized directly (e.g. type, name, max_uses, allowed_domains).
func NewServerTool(config map[string]interface{}) ToolDef {
	raw, _ := json.Marshal(config)
	return ToolDef{raw: raw}
}

// OutputConfig controls output generation parameters.
type OutputConfig struct {
	Effort string `json:"effort,omitempty"` // "low", "medium", "high"
}

// ThinkingConfig controls extended thinking / adaptive thinking mode.
type ThinkingConfig struct {
	Type         string `json:"type"`                    // "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // only for "enabled" mode (not yet used)
}

// SystemBlock is a block of content in the system prompt.
type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Message is a single message in a conversation.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// MessageRequest is the request body for the /v1/messages endpoint.
type MessageRequest struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"` // top-level automatic caching
	System       []SystemBlock  `json:"system,omitempty"`
	Messages     []Message      `json:"messages"`
	Tools        []ToolDef      `json:"tools,omitempty"`
	Output       *OutputConfig  `json:"output_config,omitempty"`
	Thinking     *ThinkingConfig `json:"thinking,omitempty"`
}

// Usage contains token usage information from a response.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// CountTokensResponse is the response from the /v1/messages/count_tokens endpoint.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// MessageResponse is the response from the /v1/messages endpoint.
type MessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	Usage      Usage          `json:"usage"`
	StopReason string         `json:"stop_reason"`
}

// Ephemeral returns a CacheControl with type "ephemeral".
func Ephemeral() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

// TextContent creates a single text content block.
func TextContent(text string) []ContentBlock {
	return []ContentBlock{{Type: "text", Text: text}}
}

// CachedTextContent creates a single text content block with cache control.
func CachedTextContent(text string) []ContentBlock {
	return []ContentBlock{{Type: "text", Text: text, CacheControl: Ephemeral()}}
}

// ImageBlock creates an image content block from base64-encoded data.
func ImageBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{
		Type: "image",
		Source: &ContentSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      base64Data,
		},
	}
}

// DocumentBlock creates a document content block from base64-encoded data.
func DocumentBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{
		Type: "document",
		Source: &ContentSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      base64Data,
		},
	}
}

// ToolResultBlock creates a tool_result content block.
func ToolResultBlock(toolUseID string, content string, isError bool) ContentBlock {
	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// TextOf extracts the concatenated text from content blocks.
func TextOf(blocks []ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

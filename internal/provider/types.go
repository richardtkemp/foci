// Package provider defines provider-neutral types for LLM API interactions.
//
// Both Anthropic and Gemini (and future providers) translate between these
// types and their wire formats at the API boundary. The types are structurally
// identical to the original anthropic.* types — session JSONL format is unchanged.
package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MarshalRaw marshals v to JSON without HTML-escaping characters like >, <, &.
// Go's json.Marshal escapes these for HTML safety, but tool call parameters
// displayed in Discord/Telegram code blocks should show the raw characters.
func MarshalRaw(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline; trim it for RawMessage compatibility.
	b := buf.Bytes()
	return json.RawMessage(bytes.TrimRight(b, "\n")), nil
}

// UnescapeUnicodeJSON converts JSON unicode escape sequences like \u003e back
// to their literal characters. Handles the case where json.Marshal has escaped
// HTML-sensitive characters (>, <, &) that appear in tool call parameters.
func UnescapeUnicodeJSON(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); i++ {
		if i+5 < len(s) && s[i] == '\\' && s[i+1] == 'u' {
			hexStr := s[i+2 : i+6]
			if isHexString(hexStr) {
				var cp int64
				if _, err := fmt.Sscanf(hexStr, "%x", &cp); err == nil {
					result.WriteRune(rune(cp))
					i += 5
					continue
				}
			}
		}
		result.WriteByte(s[i])
	}
	return result.String()
}

// isHexString returns true if s is exactly 4 hex digits.
func isHexString(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ModelInfo holds metadata about an available model from any provider.
type ModelInfo struct {
	ID        string
	CreatedAt time.Time
}

// ContentSource holds base64-encoded data for image and document content blocks.
type ContentSource struct {
	Type      string `json:"type"`       // "base64"
	MimeType  string `json:"media_type"` // "image/jpeg", "image/png", "application/pdf", etc.
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
		// tool_result: the SDK serializes "content" as an array of text blocks
		// (e.g. [{"type":"text","text":"..."}]) while our struct expects a string.
		// Handle both formats.
		if peek.Type == "tool_result" {
			var tr struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			}
			if err2 := json.Unmarshal(data, &tr); err2 == nil {
				cb.Type = tr.Type
				cb.ToolUseID = tr.ToolUseID
				cb.IsError = tr.IsError
				cb.Content = extractToolResultContent(tr.Content)
				return nil
			}
		}
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

// extractToolResultContent handles both string and array formats for tool_result content.
// The Anthropic API accepts content as either a plain string or an array of text blocks
// [{"type":"text","text":"..."}]. The SDK uses the array format.
func extractToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of text blocks
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return string(raw)
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

// Raw returns the underlying raw JSON of the tool definition.
func (t ToolDef) Raw() json.RawMessage {
	return t.raw
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
	Type         string `json:"type"`                    // "adaptive" (Anthropic) or "enabled" (Gemini)
	BudgetTokens int    `json:"budget_tokens,omitempty"` // token budget for thinking (Gemini)
}

// SystemBlock is a block of content in the system prompt.
type SystemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Message is a single message in a conversation.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// MessageRequest is the request body for an LLM API call.
type MessageRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        []SystemBlock   `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []ToolDef       `json:"tools,omitempty"`
	Output        *OutputConfig   `json:"output_config,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	CacheStrategy string          `json:"cache_strategy,omitempty"` // "auto" or "explicit" (read by Anthropic translate layer)
	CacheTTL      string          `json:"cache_ttl,omitempty"`      // Anthropic prompt cache TTL: "5m" or "1h" (read by Anthropic translate layer)
	Speed         string          `json:"speed,omitempty"`          // "fast" for Anthropic fast mode (Opus only, 6x pricing)
}

// Usage contains token usage information from a response.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// MessageResponse is the response from an LLM API call.
type MessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	Usage      Usage          `json:"usage"`
	StopReason string         `json:"stop_reason"`

	// WireRequest holds the serialized SDK-level request body (with cache markers).
	// Populated by transport layer for payload logging. Never persisted.
	WireRequest json.RawMessage `json:"-"`
}

// TextContent creates a single text content block.
func TextContent(text string) []ContentBlock {
	return []ContentBlock{{Type: "text", Text: text}}
}

// mediaContentBlock creates a media content block (image/document) from base64-encoded data.
func mediaContentBlock(blockType, mimeType, base64Data string) ContentBlock {
	return ContentBlock{
		Type: blockType,
		Source: &ContentSource{
			Type:      "base64",
			MimeType:  mimeType,
			Data:      base64Data,
		},
	}
}

// ImageBlock creates an image content block from base64-encoded data.
func ImageBlock(mimeType, base64Data string) ContentBlock {
	return mediaContentBlock("image", mimeType, base64Data)
}

// DocumentBlock creates a document content block from base64-encoded data.
func DocumentBlock(mimeType, base64Data string) ContentBlock {
	return mediaContentBlock("document", mimeType, base64Data)
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

// SessionStats holds summary metrics for a session's message history.
type SessionStats struct {
	Messages   int // number of messages
	Blocks     int // total content blocks across all messages
	ApproxBytes int // approximate size in bytes (text + tool content)
}

// ApproxTokens returns a rough token estimate (~4 chars per token).
func (s SessionStats) ApproxTokens() int {
	return s.ApproxBytes / 4
}

// ComputeSessionStats computes summary metrics for a slice of messages.
// The byte estimate counts text, thinking, tool input, tool result content,
// and base64 source data — enough to diagnose memory growth without being exact.
func ComputeSessionStats(msgs []Message) SessionStats {
	var s SessionStats
	s.Messages = len(msgs)
	for _, m := range msgs {
		s.Blocks += len(m.Content)
		for _, b := range m.Content {
			s.ApproxBytes += len(b.Text)
			s.ApproxBytes += len(b.Thinking)
			s.ApproxBytes += len(b.Content)
			s.ApproxBytes += len(b.Input)
			if b.Source != nil {
				s.ApproxBytes += len(b.Source.Data)
			}
		}
	}
	return s
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

// APIError is returned when an LLM API responds with a non-200 status code.
// Use errors.As to check for this type and inspect StatusCode or RetryAfter.
type APIError struct {
	StatusCode int    // HTTP status code
	Body       string // response body
	RetryAfter string // retry-after header value (seconds or date), empty if not present
}

// Anthropic-specific HTTP status code for overloaded service.
const statusOverloaded = 529

func (e *APIError) Error() string {
	return fmt.Sprintf("API error (status %d): %s", e.StatusCode, e.Body)
}

// IsRateLimit returns true if this is a 429 Too Many Requests error.
func (e *APIError) IsRateLimit() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// IsOverloaded returns true if this is a 529 Overloaded error.
func (e *APIError) IsOverloaded() bool {
	return e.StatusCode == statusOverloaded
}

// IsRetryable returns true if the error is a server-side issue worth retrying.
// Covers 500 (Internal Server Error), 502 (Bad Gateway), 503 (Service Unavailable),
// and 529 (Overloaded — Anthropic-specific).
func (e *APIError) IsRetryable() bool {
	switch e.StatusCode {
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,        // 502
		http.StatusServiceUnavailable, // 503
		statusOverloaded:              // 529
		return true
	}
	return false
}

// IsAuthError returns true if this is a 401 Unauthorized error.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized
}

// RetryAfterSeconds parses the retry-after header as seconds.
// Returns 0 if not present or unparseable.
func (e *APIError) RetryAfterSeconds() int {
	if e.RetryAfter == "" {
		return 0
	}
	if secs, err := strconv.Atoi(e.RetryAfter); err == nil {
		return secs
	}
	return 0
}

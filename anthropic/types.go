package anthropic

import "encoding/json"

// CacheControl marks a content block for prompt caching.
type CacheControl struct {
	Type string `json:"type"`
}

// ImageSource holds base64-encoded image data for the API.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", etc.
	Data      string `json:"data"`       // base64-encoded image data
}

// ContentBlock is a block of content in a message or response.
// Covers text, image, tool_use, and tool_result block types.
type ContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *ImageSource    `json:"source,omitempty"`    // image: base64 source
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
	ID           string          `json:"id,omitempty"`        // tool_use: block ID
	Name         string          `json:"name,omitempty"`      // tool_use: tool name
	Input        json.RawMessage `json:"input,omitempty"`     // tool_use: parameters
	ToolUseID    string          `json:"tool_use_id,omitempty"` // tool_result: references tool_use block
	Content      string          `json:"content,omitempty"`   // tool_result: result text
	IsError      bool            `json:"is_error,omitempty"`  // tool_result: error flag
}

// ToolDef defines a tool for the API request.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict,omitempty"`
}

// OutputConfig controls output generation parameters.
type OutputConfig struct {
	Effort string `json:"effort,omitempty"` // "low", "medium", "high"
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
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	CacheControl *CacheControl `json:"cache_control,omitempty"` // top-level automatic caching
	System       []SystemBlock `json:"system,omitempty"`
	Messages     []Message     `json:"messages"`
	Tools        []ToolDef     `json:"tools,omitempty"`
	Output       *OutputConfig `json:"output,omitempty"`
}

// Usage contains token usage information from a response.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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
		Source: &ImageSource{
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
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

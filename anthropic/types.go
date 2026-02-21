package anthropic

// CacheControl marks a content block for prompt caching.
type CacheControl struct {
	Type string `json:"type"`
}

// ContentBlock is a block of content in a message or response.
type ContentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
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
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []SystemBlock `json:"system,omitempty"`
	Messages  []Message     `json:"messages"`
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
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
	Model   string         `json:"model"`
	Usage   Usage          `json:"usage"`
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

// TextOf extracts the concatenated text from content blocks.
func TextOf(blocks []ContentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

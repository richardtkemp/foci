// Package anthropic re-exports provider types for backward compatibility
// within this package. All shared types live in provider/.
package anthropic

import "foci/provider"

// Type aliases — these allow anthropic/ internal code (client.go, tests)
// to use unqualified names while the canonical definitions live in provider/.
type (
	CacheControl   = provider.CacheControl
	ContentSource  = provider.ContentSource
	ContentBlock   = provider.ContentBlock
	ToolDef        = provider.ToolDef
	OutputConfig   = provider.OutputConfig
	ThinkingConfig = provider.ThinkingConfig
	SystemBlock    = provider.SystemBlock
	Message        = provider.Message
	MessageRequest = provider.MessageRequest
	Usage          = provider.Usage
	MessageResponse = provider.MessageResponse
	APIError       = provider.APIError
)

// Re-export constructors and helpers so anthropic-internal code compiles unchanged.
var (
	Ephemeral        = provider.Ephemeral
	TextContent      = provider.TextContent
	CachedTextContent = provider.CachedTextContent
	ImageBlock       = provider.ImageBlock
	DocumentBlock    = provider.DocumentBlock
	ToolResultBlock  = provider.ToolResultBlock
	TextOf           = provider.TextOf
	NewCustomTool    = provider.NewCustomTool
	NewServerTool    = provider.NewServerTool
)

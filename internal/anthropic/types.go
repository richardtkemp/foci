// Package anthropic re-exports provider types for backward compatibility
// within this package. All shared types live in provider/.
package anthropic

import "foci/internal/provider"

// Type aliases — these allow anthropic/ internal code (client.go, tests)
// to use unqualified names while the canonical definitions live in provider/.
type (
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
	UsageWindow    = provider.UsageWindow
	ExtraUsage     = provider.ExtraUsage
	UsageResponse  = provider.UsageResponse
	ModelInfo      = provider.ModelInfo
)

// Re-export constructors and helpers so anthropic-internal code compiles unchanged.
var (
	TextContent   = provider.TextContent
	ImageBlock    = provider.ImageBlock
	DocumentBlock = provider.DocumentBlock
	ToolResultBlock = provider.ToolResultBlock
	TextOf        = provider.TextOf
	NewCustomTool = provider.NewCustomTool
	NewServerTool = provider.NewServerTool
)

package anthropic

// Translation layer between provider.* types and Anthropic SDK types.
// Pattern matches openai/translate.go — translates at the boundary,
// keeping provider-neutral types as canonical throughout foci.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"foci/internal/config"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// cacheControlWithTTL returns an SDK CacheControlEphemeralParam with the given TTL.
// If ttl is empty, returns the default (no TTL field, SDK defaults to 5m).
func cacheControlWithTTL(ttl string) sdk.CacheControlEphemeralParam {
	cc := sdk.NewCacheControlEphemeralParam()
	if ttl != "" {
		cc.TTL = sdk.CacheControlEphemeralTTL(ttl)
	}
	return cc
}

// buildSDKParams translates a provider.MessageRequest into SDK MessageNewParams.
// Cache placement is handled entirely here — provider types carry no cache markers.
func buildSDKParams(req *MessageRequest) sdk.MessageNewParams {
	// Strip developer prefix (e.g., "anthropic/claude-opus-4-6" → "claude-opus-4-6")
	modelID := config.StripDeveloperPrefix(req.Model)

	params := sdk.MessageNewParams{
		Model:     sdk.Model(modelID),
		MaxTokens: int64(req.MaxTokens),
		Messages:  messagesToSDK(req.Messages),
	}

	if len(req.System) > 0 {
		params.System = systemToSDK(req.System)
	}

	if len(req.Tools) > 0 {
		params.Tools = toolsToSDK(req.Tools)
	}

	if req.Output != nil && req.Output.Effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{
			Effort: sdk.OutputConfigEffort(req.Output.Effort),
		}
	}

	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "adaptive":
			params.Thinking = sdk.ThinkingConfigParamUnion{
				OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
			}
		case "enabled":
			params.Thinking = sdk.ThinkingConfigParamUnion{
				OfEnabled: &sdk.ThinkingConfigEnabledParam{
					BudgetTokens: int64(req.Thinking.BudgetTokens),
				},
			}
		}
	}

	// Apply cache markers after clean translation
	if req.CacheStrategy != "" {
		applyCacheMarkers(&params, req.CacheStrategy, req.CacheTTL)
	}

	return params
}

// buildSDKCountParams translates a provider.MessageRequest into SDK MessageCountTokensParams.
func buildSDKCountParams(req *MessageRequest) sdk.MessageCountTokensParams {
	// Strip developer prefix (e.g., "anthropic/claude-opus-4-6" → "claude-opus-4-6")
	modelID := config.StripDeveloperPrefix(req.Model)

	params := sdk.MessageCountTokensParams{
		Model:    sdk.Model(modelID),
		Messages: messagesToSDK(req.Messages),
	}

	if len(req.System) > 0 {
		params.System = sdk.MessageCountTokensParamsSystemUnion{
			OfTextBlockArray: systemToSDK(req.System),
		}
	}

	if len(req.Tools) > 0 {
		params.Tools = countToolsToSDK(req.Tools)
	}

	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "adaptive":
			params.Thinking = sdk.ThinkingConfigParamUnion{
				OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{},
			}
		}
	}

	return params
}

// responseFromSDK translates an SDK Message into a provider.MessageResponse.
func responseFromSDK(msg *sdk.Message) *MessageResponse {
	resp := &MessageResponse{
		ID:         msg.ID,
		Type:       "message",
		Role:       string(msg.Role),
		Model:      string(msg.Model),
		StopReason: string(msg.StopReason),
		Usage: Usage{
			InputTokens:              int(msg.Usage.InputTokens),
			OutputTokens:             int(msg.Usage.OutputTokens),
			CacheCreationInputTokens: int(msg.Usage.CacheCreationInputTokens),
			CacheReadInputTokens:     int(msg.Usage.CacheReadInputTokens),
		},
	}

	for _, block := range msg.Content {
		resp.Content = append(resp.Content, contentBlockFromSDK(block))
	}

	return resp
}

// contentBlockFromSDK translates an SDK ContentBlockUnion into a provider.ContentBlock.
func contentBlockFromSDK(block sdk.ContentBlockUnion) ContentBlock {
	switch v := block.AsAny().(type) {
	case sdk.TextBlock:
		return ContentBlock{
			Type: "text",
			Text: v.Text,
		}
	case sdk.ThinkingBlock:
		return ContentBlock{
			Type:      "thinking",
			Thinking:  v.Thinking,
			Signature: v.Signature,
		}
	case sdk.RedactedThinkingBlock:
		return ContentBlock{
			Type: "redacted_thinking",
			Data: v.Data,
		}
	case sdk.ToolUseBlock:
		return ContentBlock{
			Type:  "tool_use",
			ID:    v.ID,
			Name:  v.Name,
			Input: json.RawMessage(v.Input),
		}
	case sdk.ServerToolUseBlock:
		// Server tool blocks: reconstruct raw JSON for passthrough.
		raw, _ := json.Marshal(block)
		inputRaw, _ := json.Marshal(v.Input)
		return ContentBlock{
			Type:  block.Type,
			ID:    v.ID,
			Name:  string(v.Name),
			Input: inputRaw,
			Raw:   raw,
		}
	default:
		// Unknown block types: preserve as raw JSON for passthrough.
		raw, _ := json.Marshal(block)
		cb := ContentBlock{
			Type: block.Type,
			Raw:  raw,
		}
		// Extract common fields from the union.
		cb.ID = block.ID
		cb.Name = block.Name
		cb.Input = json.RawMessage(block.Input)
		return cb
	}
}

// streamingErrorTypes maps Anthropic SSE error type strings to HTTP status codes.
// The SDK's streaming code uses fmt.Errorf (not *sdk.Error) for SSE "error" events,
// so classifySDKError must parse the embedded JSON to extract the error type.
var streamingErrorTypes = map[string]int{
	"overloaded_error":          529,
	"rate_limit_error":          429,
	"api_error":                 500,
	"internal_server_error":     500,
	"service_unavailable_error": 503,
}

// classifySDKError maps an SDK error into a provider.APIError.
// Handles both *sdk.Error (non-streaming) and plain errors with embedded
// JSON from SSE "error" events (streaming).
func classifySDKError(err error) error {
	if err == nil {
		return nil
	}

	var sdkErr *sdk.Error
	if errors.As(err, &sdkErr) {
		apiErr := &APIError{
			StatusCode: sdkErr.StatusCode,
			Body:       sdkErr.RawJSON(),
		}
		if sdkErr.Response != nil {
			apiErr.RetryAfter = sdkErr.Response.Header.Get("Retry-After")
		}
		return apiErr
	}

	// SDK streaming errors are plain fmt.Errorf with embedded JSON:
	//   "received error while streaming: {json}"
	// Parse the JSON to extract the error type and map to a status code.
	msg := err.Error()
	if idx := strings.Index(msg, "{"); idx >= 0 {
		jsonBody := msg[idx:]
		var envelope struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(jsonBody), &envelope) == nil && envelope.Error.Type != "" {
			if status, ok := streamingErrorTypes[envelope.Error.Type]; ok {
				return &APIError{
					StatusCode: status,
					Body:       jsonBody,
				}
			}
		}
	}

	return err
}

// messagesToSDK translates provider messages to SDK message params.
func messagesToSDK(msgs []Message) []sdk.MessageParam {
	result := make([]sdk.MessageParam, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, sdk.MessageParam{
			Role:    sdk.MessageParamRole(msg.Role),
			Content: contentBlocksToSDK(msg.Content),
		})
	}
	return result
}

// contentBlocksToSDK translates provider content blocks to SDK content block params.
func contentBlocksToSDK(blocks []ContentBlock) []sdk.ContentBlockParamUnion {
	result := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		result = append(result, contentBlockToSDK(b))
	}
	return result
}

// contentBlockToSDK translates a single provider content block to SDK param.
// No cache markers — those are applied by applyCacheMarkers after translation.
func contentBlockToSDK(b ContentBlock) sdk.ContentBlockParamUnion {
	switch b.Type {
	case "text":
		return sdk.NewTextBlock(b.Text)

	case "image":
		if b.Source != nil {
			return sdk.NewImageBlockBase64(b.Source.MimeType, b.Source.Data)
		}

	case "document":
		if b.Source != nil {
			// Documents can have various media types beyond PDF. Use raw JSON
			// passthrough to match the wire format exactly.
			docJSON, _ := json.Marshal(b)
			return rawToSDKContentBlock(docJSON)
		}

	case "tool_use":
		return sdk.NewToolUseBlock(b.ID, json.RawMessage(b.Input), b.Name)

	case "tool_result":
		return sdk.NewToolResultBlock(b.ToolUseID, b.Content, b.IsError)

	case "thinking":
		return sdk.NewThinkingBlock(b.Signature, b.Thinking)

	case "redacted_thinking":
		return sdk.NewRedactedThinkingBlock(b.Data)

	default:
		// Server tool result blocks and other unknown types: use raw JSON passthrough.
		if len(b.Raw) > 0 {
			return rawToSDKContentBlock(b.Raw)
		}
	}

	// Fallback for unhandled types without raw data.
	return sdk.NewTextBlock(fmt.Sprintf("[unsupported block type: %s]", b.Type))
}

// applyCacheMarkers sets cache_control markers on SDK params based on the strategy.
// Called after clean translation of all blocks.
//
// Both strategies mark the last system block (stable prefix).
// "auto": sets top-level params.CacheControl (Anthropic auto-caches the longest prefix).
// "explicit": marks the last content block of the second-to-last message.
func applyCacheMarkers(params *sdk.MessageNewParams, strategy, ttl string) {
	cc := cacheControlWithTTL(ttl)

	// Always mark last system block
	if len(params.System) > 0 {
		params.System[len(params.System)-1].CacheControl = cc
	}

	switch strategy {
	case "auto":
		params.CacheControl = cc
	case "explicit":
		// Mark last content block of second-to-last message
		if len(params.Messages) >= 2 {
			msg := &params.Messages[len(params.Messages)-2]
			if len(msg.Content) > 0 {
				setBlockCacheControl(&msg.Content[len(msg.Content)-1], cc)
			}
		}
	}
}

// setBlockCacheControl sets cache_control on an SDK content block param,
// switching on the variant type (text, image, tool_result).
func setBlockCacheControl(block *sdk.ContentBlockParamUnion, cc sdk.CacheControlEphemeralParam) {
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cc
	case block.OfImage != nil:
		block.OfImage.CacheControl = cc
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cc
	}
}

// rawToSDKContentBlock wraps raw JSON as an SDK content block param via Override.
func rawToSDKContentBlock(raw json.RawMessage) sdk.ContentBlockParamUnion {
	return param.Override[sdk.ContentBlockParamUnion](json.RawMessage(raw))
}

// systemToSDK translates provider system blocks to SDK text block params.
// No cache markers — those are applied by applyCacheMarkers after translation.
func systemToSDK(blocks []SystemBlock) []sdk.TextBlockParam {
	result := make([]sdk.TextBlockParam, 0, len(blocks))
	for _, b := range blocks {
		result = append(result, sdk.TextBlockParam{Text: b.Text})
	}
	return result
}

// toolsToSDK translates provider tool definitions to SDK tool union params.
func toolsToSDK(tools []ToolDef) []sdk.ToolUnionParam {
	result := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		result = append(result, toolToSDK(t))
	}
	return result
}

// toolToSDK translates a single provider ToolDef to an SDK ToolUnionParam.
// Custom tools get typed fields; server tools use raw JSON passthrough.
func toolToSDK(t ToolDef) sdk.ToolUnionParam {
	raw := t.Raw()

	// Peek at the type field to distinguish custom vs server tools.
	var peek struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &peek)

	// Server tools (web_search, web_fetch, etc.): passthrough raw JSON.
	if peek.Type != "" && peek.Type != "custom" {
		return param.Override[sdk.ToolUnionParam](string(raw))
	}

	// Custom tools: extract fields into typed struct.
	var def struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	_ = json.Unmarshal(raw, &def)

	var schema sdk.ToolInputSchemaParam
	_ = json.Unmarshal(def.InputSchema, &schema)

	return sdk.ToolUnionParam{
		OfTool: &sdk.ToolParam{
			Name:        def.Name,
			Description: param.NewOpt(def.Description),
			InputSchema: schema,
		},
	}
}

// countToolsToSDK translates provider tool definitions to SDK count token tool params.
func countToolsToSDK(tools []ToolDef) []sdk.MessageCountTokensToolUnionParam {
	result := make([]sdk.MessageCountTokensToolUnionParam, 0, len(tools))
	for _, t := range tools {
		raw := t.Raw()
		// Use raw JSON passthrough for all tool types in token counting.
		result = append(result, param.Override[sdk.MessageCountTokensToolUnionParam](string(raw)))
	}
	return result
}

// sdkRequestOptions builds per-request SDK options for auth and beta headers.
func sdkRequestOptions(token string) []option.RequestOption {
	opts := []option.RequestOption{
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", "oauth-2025-04-20"),
	}
	return opts
}

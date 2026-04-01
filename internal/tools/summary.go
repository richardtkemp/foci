package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
)

// NewSummaryTool creates a tool that summarizes/extracts information from a file
// via a fast, cheap model call without loading the full content into the agent's context.
// defaultClient is the agent's default provider client.
// clientProvider provides access to clients for different endpoint:format pairs.
// groupResolver resolves the call site to the appropriate model/client.
func NewSummaryTool(defaultClient provider.Client, clientProvider provider.ClientProvider, groupResolver *config.GroupResolver, workspace string, fallbackFn provider.FallbackFunc) *Tool {
	resolveForCall := func() (provider.Client, string, string) {
		resolved := groupResolver.ResolveCall(config.CallSummarizeFile)
		if resolved == nil {
			// Ungrouped — shouldn't happen for CallSummarizeFile, but be safe
			return defaultClient, "", ""
		}
		client := defaultClient
		if clientProvider != nil {
			if c := clientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
				client = c
			}
		}
		return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
	}

	return &Tool{
		Name:        "summary",
		Description: "Summarize or extract specific information from a file using a fast, cheap model call. Do NOT use this to read or dump full file contents — use the read tool for that. This tool is for targeted questions like 'what config options are defined?' or 'summarize the error handling approach', not for retrieving the file text itself.",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"file": {
					"type": "string",
					"description": "Path to the file to summarize"
				},
				"prompt": {
					"type": "string",
					"description": "What to extract or summarize from the file"
				}
			},
			"required": ["file", "prompt"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			client, model, format := resolveForCall()
			return summaryExecute(ctx, params, client, model, workspace, format, fallbackFn, clientProvider)
		},
	}
}

func summaryExecute(ctx context.Context, params json.RawMessage, client provider.Client, model, workspace, format string, fallbackFn provider.FallbackFunc, clientProvider provider.ClientProvider) (ToolResult, error) {
	var p struct {
		File   string `json:"file"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	if p.File == "" {
		return ToolResult{}, fmt.Errorf("file parameter is required")
	}
	if p.Prompt == "" {
		return ToolResult{}, fmt.Errorf("prompt parameter is required")
	}

	filePath := resolveWorkspacePath(p.File, workspace)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read file: %w", err)
	}

	if len(data) == 0 {
		return ToolResult{}, fmt.Errorf("file is empty: %s", p.File)
	}

	// Detect binary files by checking for null bytes in first 512 bytes
	checkLen := len(data)
	if checkLen > 512 {
		checkLen = 512
	}
	for _, b := range data[:checkLen] {
		if b == 0 {
			return ToolResult{}, fmt.Errorf("file appears to be binary: %s", p.File)
		}
	}

	start := time.Now()

	req := &provider.MessageRequest{
		Model:     model,
		MaxTokens: 4096,
		System: []provider.SystemBlock{
			{Type: "text", Text: "You are a file summarization assistant. Read the file content and respond to the user's prompt about it. Be concise and precise. Quote key sections word-for-word where accuracy matters (names, values, instructions, error messages) rather than paraphrasing."},
		},
		Messages: []provider.Message{
			{
				Role: "user",
				Content: provider.TextContent(
					fmt.Sprintf("<file path=%q>\n%s\n</file>\n\n%s", p.File, string(data), p.Prompt),
				),
			},
		},
	}

	resp, err := provider.Send(ctx, client, req, nil,
		fallbackFn, clientProvider, func(f string, args ...any) {
			log.Errorf("summary", f, args...)
		})
	if err != nil {
		return ToolResult{}, fmt.Errorf("summary API call: %w", err)
	}

	duration := time.Since(start)
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	log.Infof("summary", "session=%s model=%s input=%d output=%d cost=$%.4f duration=%s",
		SessionKeyFromContext(ctx), model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	providerFormat := format
	if providerFormat == "" {
		providerFormat = "anthropic"
	}
	log.API(log.APIEntry{
		Timestamp:  start,
		Provider:   providerFormat,
		Session:    SessionKeyFromContext(ctx),
		Model:      model,
		Input:      resp.Usage.InputTokens,
		Output:     resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadInputTokens,
		CacheWrite: resp.Usage.CacheCreationInputTokens,
		CostUSD:    cost,
		DurationMS: duration.Milliseconds(),
		StopReason: resp.StopReason,
		CallType:   "summary",
	})

	text := provider.TextOf(resp.Content)
	if text == "" {
		return TextResult("(empty response)"), nil
	}
	return TextResult(text), nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/anthropic"
	"foci/log"
)

// NewSummaryTool creates a tool that summarizes/extracts information from a file
// via a Haiku call without loading the full content into the agent's context.
// modelAliases maps short names (e.g. "haiku") to full model IDs; used to
// resolve the model for the API call. May be nil (falls back to "claude-haiku-4-5").
func NewSummaryTool(client *anthropic.Client, modelAliases map[string]string) *Tool {
	resolveModel := func(alias string) string {
		if modelAliases != nil {
			if full, ok := modelAliases[strings.ToLower(alias)]; ok {
				return full
			}
		}
		return alias
	}

	return &Tool{
		Name:        "summary",
		Description: "Summarize or extract specific information from a file using a fast Haiku call. Use this instead of read for large files when you only need specific information, not the full content.",
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
			return summaryExecute(ctx, params, client, resolveModel)
		},
	}
}

func summaryExecute(ctx context.Context, params json.RawMessage, client *anthropic.Client, resolveModel func(string) string) (ToolResult, error) {
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

	data, err := os.ReadFile(p.File)
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

	model := resolveModel("haiku")
	start := time.Now()

	req := &anthropic.MessageRequest{
		Model:     model,
		MaxTokens: 4096,
		System: []anthropic.SystemBlock{
			{Type: "text", Text: "You are a file summarization assistant. Read the file content and respond to the user's prompt about it. Be concise and precise. Quote key sections word-for-word where accuracy matters (names, values, instructions, error messages) rather than paraphrasing."},
		},
		Messages: []anthropic.Message{
			{
				Role: "user",
				Content: anthropic.TextContent(
					fmt.Sprintf("<file path=%q>\n%s\n</file>\n\n%s", p.File, string(data), p.Prompt),
				),
			},
		},
	}

	resp, err := client.SendMessage(ctx, req)
	if err != nil {
		return ToolResult{}, fmt.Errorf("summary API call: %w", err)
	}

	duration := time.Since(start)
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	log.Infof("summary", "model=%s input=%d output=%d cost=$%.4f duration=%s",
		model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	log.API(log.APIEntry{
		Timestamp:  start.UTC(),
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

	text := anthropic.TextOf(resp.Content)
	if text == "" {
		return TextResult("(empty response)"), nil
	}
	return TextResult(text), nil
}

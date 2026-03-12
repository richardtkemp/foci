package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
)

// NewSummaryTool creates a tool that summarizes/extracts information from a file
// via a fast, cheap model call without loading the full content into the agent's context.
// defaultClient is the agent's default provider client.
// clientProvider provides access to clients for different endpoint:format pairs.
// agentModel is the agent's configured model (developer/model_id format), used to pick
// the right lightweight model for the summary call (e.g. haiku for Anthropic, flash for Gemini).
// modelAliases maps short names (e.g. "haiku") to full model IDs (with endpoint prefix);
// used to resolve the model for the API call. May be nil (falls back to "claude-haiku-4-5").
func NewSummaryTool(defaultClient provider.Client, clientProvider provider.ClientProvider, agentModel string, modelAliases map[string]string) *Tool {
	// Parse the agent's model to get the bare model ID
	// agentModel is now in developer/model_id format
	_, agentModelID := splitDeveloperModel(agentModel)

	// Pick the cheapest model alias for the agent's provider.
	summaryAlias := "haiku"
	defaultFormat := "anthropic"
	if strings.HasPrefix(agentModelID, "gemini-") {
		summaryAlias = "flash"
		defaultFormat = "gemini"
	} else if isOpenAIModel(agentModelID) {
		summaryAlias = "gpt4o"
		defaultFormat = "openai"
	}

	// Resolve which client to use based on the summary model.
	resolveClient := func() provider.Client {
		// Use config.ResolveModel to handle the summary alias
		resolved, err := resolveModelForSummary(summaryAlias, modelAliases)
		if err != nil || clientProvider == nil {
			return defaultClient
		}
		if c := clientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
			return c
		}
		return defaultClient
	}

	return &Tool{
		Name:        "summary",
		Description: "Summarize or extract specific information from a file using a fast, cheap model call. Use this instead of read for large files when you only need specific information, not the full content.",
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
			return summaryExecute(ctx, params, resolveClient(), summaryAlias, modelAliases, defaultFormat)
		},
	}
}

// resolveModelForSummary is a helper to resolve a model alias for summary tool.
func resolveModelForSummary(alias string, aliases map[string]string) (*config.ResolvedModel, error) {
	// Use empty endpoint to get auto-selection
	return config.ResolveModel(alias, "", aliases)
}

// splitDeveloperModel splits "developer/model_id" into parts.
func splitDeveloperModel(model string) (developer, modelID string) {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

func summaryExecute(ctx context.Context, params json.RawMessage, client provider.Client, summaryAlias string, modelAliases map[string]string, defaultFormat string) (ToolResult, error) {
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

	// Use config.ResolveModel to get the model ID
	resolved, err := config.ResolveModel(summaryAlias, "", modelAliases)
	if err != nil {
		return ToolResult{}, fmt.Errorf("resolve model: %w", err)
	}
	model := resolved.ModelID
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

	resp, err := provider.Send(ctx, client, req, nil)
	if err != nil {
		return ToolResult{}, fmt.Errorf("summary API call: %w", err)
	}

	duration := time.Since(start)
	cost := log.CalculateCost(model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

	log.Infof("summary", "model=%s input=%d output=%d cost=$%.4f duration=%s",
		model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost, duration.Round(time.Millisecond))

	// Use the resolved model's format for provider attribution,
	// falling back to the default format from the parent agent.
	providerFormat := resolved.Format
	if providerFormat == "" {
		providerFormat = defaultFormat
	}
	log.API(log.APIEntry{
		Timestamp:  start.UTC(),
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

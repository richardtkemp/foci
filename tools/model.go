package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"clod/anthropic"
	"clod/log"
)

// SystemBlocksProvider returns the system prompt blocks (for full prompt weight).
type SystemBlocksProvider interface {
	SystemBlocks() []anthropic.SystemBlock
}

// knownModels maps short names to full model IDs.
var knownModels = map[string]string{
	"haiku":  "claude-haiku-4-5",
	"sonnet": "claude-sonnet-4-5",
	"opus":   "claude-opus-4-6",
}

// lightSystemPrompt is the minimal system prompt for "light" weight.
const lightSystemPrompt = "You are an AI assistant. Answer the following question directly and concisely."

func NewRequestModelTool(client *anthropic.Client, bootstrap SystemBlocksProvider) *Tool {
	return &Tool{
		Name:        "request_model",
		Description: "Make a synchronous one-shot call to a different model. The model gets your prompt, thinks about it, and the response comes back as this tool's result. Your session stays on its current model with its warm cache intact. Use for complex reasoning, second opinions, or tasks that need a heavier model.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"model": {
					"type": "string",
					"description": "Model to query: 'opus', 'sonnet', 'haiku', or a full model ID"
				},
				"prompt": {
					"type": "string",
					"description": "Self-contained prompt with all necessary context. The model gets only this — it has no access to your conversation history."
				},
				"prompt_weight": {
					"type": "string",
					"enum": ["full", "light", "none"],
					"description": "How much system context the model gets. 'light' (default): minimal system prompt. 'full': your full character files. 'none': just your prompt, no system context."
				}
			},
			"required": ["model", "prompt"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Model        string `json:"model"`
				Prompt       string `json:"prompt"`
				PromptWeight string `json:"prompt_weight"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}

			if p.Prompt == "" {
				return "", fmt.Errorf("prompt is required")
			}

			// Resolve short name to full model ID
			model := p.Model
			if full, ok := knownModels[model]; ok {
				model = full
			}

			// Default to light weight
			if p.PromptWeight == "" {
				p.PromptWeight = "light"
			}

			// Build system prompt based on weight
			var system []anthropic.SystemBlock
			switch p.PromptWeight {
			case "full":
				if bootstrap != nil {
					system = bootstrap.SystemBlocks()
				}
			case "light":
				system = []anthropic.SystemBlock{
					{Type: "text", Text: lightSystemPrompt},
				}
			case "none":
				// no system prompt
			default:
				return "", fmt.Errorf("invalid prompt_weight: %q (use full, light, or none)", p.PromptWeight)
			}

			log.Infof("request_model", "calling %s (weight=%s, prompt=%d chars)", model, p.PromptWeight, len(p.Prompt))

			req := &anthropic.MessageRequest{
				Model:     model,
				MaxTokens: 8192,
				System:    system,
				Messages: []anthropic.Message{
					{Role: "user", Content: anthropic.TextContent(p.Prompt)},
				},
				// No tools — one-shot cold call
			}

			resp, err := client.SendMessage(ctx, req)
			if err != nil {
				return "", fmt.Errorf("request_model %s: %w", model, err)
			}

			cost := log.CalculateCost(model,
				resp.Usage.InputTokens, resp.Usage.OutputTokens,
				resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

			log.Infof("request_model", "done model=%s input=%d output=%d cost=$%.4f",
				model, resp.Usage.InputTokens, resp.Usage.OutputTokens, cost)

			text := anthropic.TextOf(resp.Content)
			if text == "" {
				return "(empty response)", nil
			}
			return text, nil
		},
	}
}

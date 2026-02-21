package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// ModelEscalator is the interface needed by the request_model tool.
type ModelEscalator interface {
	SetOverrideModel(model string)
}

// knownModels maps short names to full model IDs.
var knownModels = map[string]string{
	"haiku":  "claude-haiku-4-5",
	"sonnet": "claude-sonnet-4-5",
	"opus":   "claude-opus-4-6",
}

func NewRequestModelTool(escalator ModelEscalator) *Tool {
	return &Tool{
		Name:        "request_model",
		Description: "Escalate to a heavier model for the next turn only. Use when you need stronger reasoning (e.g., complex multi-step analysis). The escalation auto-reverts after one turn.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"model": {
					"type": "string",
					"description": "Model to escalate to: 'opus', 'sonnet', 'haiku', or a full model ID"
				},
				"reason": {
					"type": "string",
					"description": "Why you need a heavier model (logged for cost monitoring)"
				}
			},
			"required": ["model", "reason"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Model  string `json:"model"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}

			// Resolve short name to full model ID
			model := p.Model
			if full, ok := knownModels[model]; ok {
				model = full
			}

			escalator.SetOverrideModel(model)
			return fmt.Sprintf("Next turn will use %s. Reason: %s", model, p.Reason), nil
		},
	}
}

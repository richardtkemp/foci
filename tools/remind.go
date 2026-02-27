package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/memory"
)

func NewMemoryRemindTool(rs *memory.ReminderStore, agentID string) *Tool {
	return &Tool{
		Name:        "memory_remind",
		Description: "Defer a thought for later. The reminder will surface as injected context at the specified time. Use this for things you want to think about or follow up on later — not a full task system, just 'future me should think about this.'",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {
					"type": "string",
					"description": "The thought or reminder text"
				},
				"when": {
					"type": "string",
					"description": "When to surface: 'next_heartbeat', 'next_session', 'tomorrow', a date (YYYY-MM-DD), an ISO timestamp (e.g. '2026-02-26T12:00:00Z'), or a duration (e.g. '2h', '30m')"
				}
			},
			"required": ["text", "when"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Text string `json:"text"`
				When string `json:"when"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}

			if p.Text == "" {
				return "", fmt.Errorf("text is required")
			}
			if p.When == "" {
				return "", fmt.Errorf("when is required")
			}

			if err := rs.Add(agentID, p.Text, p.When); err != nil {
				return "", fmt.Errorf("add reminder: %w", err)
			}

			return fmt.Sprintf("Reminder set for %s: %s", p.When, p.Text), nil
		},
	}
}

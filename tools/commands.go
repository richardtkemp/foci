package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/command"
)

// CreateCommandWrapperTool wraps a slash command as a tool.
// This allows the agent to invoke commands programmatically.
// The tool has the same name and description as the command.
// Commands take an optional 'args' string parameter.
func CreateCommandWrapperTool(cmd *command.Command) *Tool {
	return &Tool{
		Name:        cmd.Name,
		Description: cmd.Description + " (slash command exposed as tool)",
		Parameters: json.RawMessage(`{
	"type": "object",
	"properties": {
		"args": {
			"type": "string",
			"description": "Optional arguments for the command"
		}
	}
}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Args string `json:"args"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}

			// Execute the command
			result, err := cmd.Execute(ctx, p.Args)
			if err != nil {
				return "", fmt.Errorf("command failed: %w", err)
			}

			return result, nil
		},
	}
}

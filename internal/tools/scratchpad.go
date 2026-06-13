package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/memory"
)

func NewScratchpadTool(s *memory.Scratchpad, agentID string) *Tool {
	return &Tool{
		Name:        "scratchpad",
		Description: "Working notes that survive compaction. Use for investigation state you don't want to lose. Clear entries when done.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["write", "read", "clear", "list"],
					"description": "Action to perform"
				},
				"key": {
					"type": "string",
					"description": "Key name (required for write, read, clear)"
				},
				"content": {
					"type": "string",
					"description": "Content to store (required for write)"
				}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Action  string `json:"action"`
				Key     string `json:"key"`
				Content string `json:"content"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}

			switch p.Action {
			case "write":
				if p.Key == "" {
					return ToolResult{}, fmt.Errorf("key is required")
				}
				if err := s.Write(agentID, p.Key, p.Content); err != nil {
					return ToolResult{}, fmt.Errorf("write scratchpad: %w", err)
				}
				return TextResult(fmt.Sprintf("Scratchpad '%s' written.", p.Key)), nil

			case "read":
				if p.Key == "" {
					return ToolResult{}, fmt.Errorf("key is required")
				}
				content, err := s.Read(agentID, p.Key)
				if err != nil {
					return ToolResult{}, fmt.Errorf("read scratchpad: %w", err)
				}
				if content == "" {
					return TextResult(fmt.Sprintf("Scratchpad '%s' is empty.", p.Key)), nil
				}
				return TextResult(content), nil

			case "clear":
				if p.Key == "" {
					return ToolResult{}, fmt.Errorf("key is required")
				}
				if err := s.Clear(agentID, p.Key); err != nil {
					return ToolResult{}, fmt.Errorf("clear scratchpad: %w", err)
				}
				return TextResult(fmt.Sprintf("Scratchpad '%s' cleared.", p.Key)), nil

			case "list":
				entries, err := s.List(agentID)
				if err != nil {
					return ToolResult{}, fmt.Errorf("list scratchpad: %w", err)
				}
				if len(entries) == 0 {
					return TextResult("No scratchpad entries."), nil
				}
				var lines []string
				now := time.Now()
				for _, e := range entries {
					sizeStr := formatSize(e.SizeBytes)
					age := now.Sub(e.Updated)
					ageStr := formatAge(age)
					lines = append(lines, fmt.Sprintf("key: %s (%s, %s)", e.Key, sizeStr, ageStr))
				}
				return TextResult(fmt.Sprintf("Scratchpad entries:\n%s", joinLines(lines))), nil

			default:
				return ToolResult{}, fmt.Errorf("unknown action %q (use write, read, clear, or list)", p.Action)
			}
		},
	}
}

func formatSize(bytes int) string {
	if bytes >= 1024 {
		return fmt.Sprintf("%.1fk", float64(bytes)/1024)
	}
	return fmt.Sprintf("%db", bytes)
}

func formatAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		return fmt.Sprintf("%dm ago", mins)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		return fmt.Sprintf("%dh ago", hours)
	}
	days := int(d.Hours() / 24)
	return fmt.Sprintf("%dd ago", days)
}

func joinLines(lines []string) string {
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += "  " + line
	}
	return result
}

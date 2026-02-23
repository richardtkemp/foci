package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"clod/memory"
)

func NewScratchpadWriteTool(s *memory.Scratchpad, agentID string) *Tool {
	return &Tool{
		Name:        "scratchpad_write",
		Description: "Write working notes to the scratchpad. Scratchpad entries survive compaction — use this for working state during investigations that you don't want to lose. Clear entries when done.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"key": {
					"type": "string",
					"description": "Short key name for this entry (e.g. 'investigation', 'task_context', 'debug_notes')"
				},
				"content": {
					"type": "string",
					"description": "The content to store"
				}
			},
			"required": ["key", "content"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Key     string `json:"key"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Key == "" {
				return "", fmt.Errorf("key is required")
			}
			if err := s.Write(agentID, p.Key, p.Content); err != nil {
				return "", fmt.Errorf("write scratchpad: %w", err)
			}
			return fmt.Sprintf("Scratchpad '%s' written.", p.Key), nil
		},
	}
}

func NewScratchpadReadTool(s *memory.Scratchpad, agentID string) *Tool {
	return &Tool{
		Name:        "scratchpad_read",
		Description: "Read a scratchpad entry by key. Returns the stored content, or empty if the key doesn't exist.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"key": {
					"type": "string",
					"description": "Key name to read"
				}
			},
			"required": ["key"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			content, err := s.Read(agentID, p.Key)
			if err != nil {
				return "", fmt.Errorf("read scratchpad: %w", err)
			}
			if content == "" {
				return fmt.Sprintf("Scratchpad '%s' is empty.", p.Key), nil
			}
			return content, nil
		},
	}
}

func NewScratchpadClearTool(s *memory.Scratchpad, agentID string) *Tool {
	return &Tool{
		Name:        "scratchpad_clear",
		Description: "Clear a scratchpad entry. Use this when you're done with working state to keep the scratchpad tidy.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"key": {
					"type": "string",
					"description": "Key name to clear"
				}
			},
			"required": ["key"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if err := s.Clear(agentID, p.Key); err != nil {
				return "", fmt.Errorf("clear scratchpad: %w", err)
			}
			return fmt.Sprintf("Scratchpad '%s' cleared.", p.Key), nil
		},
	}
}

func NewScratchpadListTool(s *memory.Scratchpad, agentID string) *Tool {
	return &Tool{
		Name:        "scratchpad_list",
		Description: "List all scratchpad entries with their keys, sizes, and last-modified times.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {}
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			entries, err := s.List(agentID)
			if err != nil {
				return "", fmt.Errorf("list scratchpad: %w", err)
			}
			if len(entries) == 0 {
				return "No scratchpad entries.", nil
			}
			var lines []string
			now := time.Now()
			for _, e := range entries {
				sizeStr := formatSize(e.SizeBytes)
				age := now.Sub(e.Updated)
				ageStr := formatAge(age)
				lines = append(lines, fmt.Sprintf("key: %s (%s, %s)", e.Key, sizeStr, ageStr))
			}
			return fmt.Sprintf("Scratchpad entries:\n%s", joinLines(lines)), nil
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

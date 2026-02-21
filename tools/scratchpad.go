package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"clod/memory"
)

func NewScratchpadWriteTool(s *memory.Scratchpad) *Tool {
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
			if err := s.Write(p.Key, p.Content); err != nil {
				return "", fmt.Errorf("write scratchpad: %w", err)
			}
			return fmt.Sprintf("Scratchpad '%s' written.", p.Key), nil
		},
	}
}

func NewScratchpadReadTool(s *memory.Scratchpad) *Tool {
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
			content, err := s.Read(p.Key)
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

func NewScratchpadClearTool(s *memory.Scratchpad) *Tool {
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
			if err := s.Clear(p.Key); err != nil {
				return "", fmt.Errorf("clear scratchpad: %w", err)
			}
			return fmt.Sprintf("Scratchpad '%s' cleared.", p.Key), nil
		},
	}
}

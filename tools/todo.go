package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/memory"
)

// NewTodoTool creates the todo management tool.
func NewTodoTool(store *memory.TodoStore, agentID string) *Tool {
	return &Tool{
		Name:        "todo",
		ExecExport:  true,
		Description: "Manage a persistent todo list. Supports adding, listing, searching, getting, completing, editing, and removing items. Items have priority (high/medium/low) and optional tags. Items survive restarts.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["add", "list", "search", "get", "complete", "edit", "remove"],
					"description": "The operation to perform"
				},
				"text": {
					"type": "string",
					"description": "Text for the todo item (required for 'add', optional for 'edit')"
				},
				"priority": {
					"type": "string",
					"enum": ["high", "medium", "low"],
					"description": "Priority level (default: medium, used with 'add' and 'edit')"
				},
				"tag": {
					"type": "string",
					"description": "Comma-separated tags (used with 'add'/'edit' to set tags, with 'list' to filter by tag, e.g. 'background')"
				},
				"id": {
					"type": "integer",
					"description": "Todo item ID (required for 'get', 'complete', 'edit', and 'remove')"
				},
				"ids": {
					"type": "array",
					"items": {"type": "integer"},
					"description": "Array of todo item IDs (alternative to 'id' for batch operations, used with 'complete', 'edit', 'remove')"
				},
				"status": {
					"type": "string",
					"enum": ["open", "done"],
					"description": "Filter by status (used with 'list', default: all)"
				},
				"query": {
					"type": "string",
					"description": "Search query (required for 'search', case-insensitive substring match)"
				},
				"reason": {
					"type": "string",
					"description": "Reason for completing the todo (required for 'complete', e.g. 'implemented in abc1234', 'no longer relevant')"
				}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Action   string  `json:"action"`
				Text     string  `json:"text"`
				Priority string  `json:"priority"`
				Tag      string  `json:"tag"`
				ID       int64   `json:"id"`
				IDs      []int64 `json:"ids"`
				Status   string  `json:"status"`
				Query    string  `json:"query"`
				Reason   string  `json:"reason"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}

			switch p.Action {
			case "add":
				if p.Text == "" {
					return "", fmt.Errorf("text is required for add")
				}
				id, err := store.Add(agentID, p.Text, p.Priority, p.Tag)
				if err != nil {
					return "", fmt.Errorf("add todo: %w", err)
				}
				pri := p.Priority
				if pri == "" {
					pri = "medium"
				}
				return fmt.Sprintf("Added #%d (%s)", id, pri), nil

			case "list":
				items, err := store.List(agentID, p.Status, p.Tag)
				if err != nil {
					return "", fmt.Errorf("list todos: %w", err)
				}
				if len(items) == 0 {
					if p.Status != "" {
						return fmt.Sprintf("No %s todos.", p.Status), nil
					}
					return "No todos.", nil
				}
				var lines []string
				for _, item := range items {
					marker := "[ ]"
					if item.Status == "done" {
						marker = "[x]"
					}
					tags := memory.FormatTags(item.Tags)
					line := fmt.Sprintf("#%d %s [%s]%s %s %s", item.ID, marker, item.Priority, tags, item.Text, formatTodoTimestamp(item))
					if item.Status == "done" && item.CloseReason != "" {
						line += fmt.Sprintf(" — %s", item.CloseReason)
					}
					lines = append(lines, line)
				}
				return strings.Join(lines, "\n"), nil

			case "search":
				if p.Query == "" {
					return "", fmt.Errorf("query is required for search")
				}
				items, err := store.Search(agentID, p.Query)
				if err != nil {
					return "", fmt.Errorf("search todos: %w", err)
				}
				if len(items) == 0 {
					return fmt.Sprintf("No todos matching %q.", p.Query), nil
				}
				var lines []string
				for _, item := range items {
					marker := "[ ]"
					if item.Status == "done" {
						marker = "[x]"
					}
					tags := memory.FormatTags(item.Tags)
					line := fmt.Sprintf("#%d %s [%s]%s %s %s", item.ID, marker, item.Priority, tags, item.Text, formatTodoTimestamp(item))
					if item.Status == "done" && item.CloseReason != "" {
						line += fmt.Sprintf(" — %s", item.CloseReason)
					}
					lines = append(lines, line)
				}
				return strings.Join(lines, "\n"), nil

			case "get":
				if p.ID == 0 {
					return "", fmt.Errorf("id is required for get")
				}
				item, err := store.Get(agentID, p.ID)
				if err != nil {
					return "", fmt.Errorf("get todo: %w", err)
				}
				marker := "[ ]"
				if item.Status == "done" {
					marker = "[x]"
				}
				tags := memory.FormatTags(item.Tags)
				line := fmt.Sprintf("#%d %s [%s]%s %s %s", item.ID, marker, item.Priority, tags, item.Text, formatTodoTimestamp(*item))
				if item.Status == "done" && item.CloseReason != "" {
					line += fmt.Sprintf(" — %s", item.CloseReason)
				}
				return line, nil

			case "complete":
				if p.Reason == "" {
					return "", fmt.Errorf("reason is required for complete (e.g. 'implemented in abc1234', 'no longer relevant')")
				}
				ids, err := resolveIDs(p.ID, p.IDs)
				if err != nil {
					return "", err
				}
				var results []string
				for _, id := range ids {
					if err := store.Complete(agentID, id, p.Reason); err != nil {
						results = append(results, fmt.Sprintf("#%d: error: %v", id, err))
					} else {
						results = append(results, fmt.Sprintf("#%d: completed", id))
					}
				}
				return strings.Join(results, "\n"), nil

			case "edit":
				ids, err := resolveIDs(p.ID, p.IDs)
				if err != nil {
					return "", err
				}
				var raw map[string]json.RawMessage
				json.Unmarshal(params, &raw)
				_, setTags := raw["tag"]

				if p.Text == "" && p.Priority == "" && !setTags {
					return "", fmt.Errorf("edit requires at least one of: text, priority, tag")
				}
				var results []string
				for _, id := range ids {
					item, err := store.Edit(agentID, id, p.Text, p.Priority, p.Tag, setTags)
					if err != nil {
						results = append(results, fmt.Sprintf("#%d: error: %v", id, err))
					} else {
						tags := memory.FormatTags(item.Tags)
						marker := "[ ]"
						if item.Status == "done" {
							marker = "[x]"
						}
						results = append(results, fmt.Sprintf("#%d: updated %s [%s]%s %s", item.ID, marker, item.Priority, tags, item.Text))
					}
				}
				return strings.Join(results, "\n"), nil

			case "remove":
				ids, err := resolveIDs(p.ID, p.IDs)
				if err != nil {
					return "", err
				}
				var results []string
				for _, id := range ids {
					if err := store.Remove(agentID, id); err != nil {
						results = append(results, fmt.Sprintf("#%d: error: %v", id, err))
					} else {
						results = append(results, fmt.Sprintf("#%d: removed", id))
					}
				}
				return strings.Join(results, "\n"), nil

			default:
				return "", fmt.Errorf("unknown action: %s (use add, list, search, get, complete, edit, or remove)", p.Action)
			}
		},
	}
}

func formatTodoTimestamp(item memory.TodoItem) string {
	if item.Status == "done" && item.CompletedAt != nil {
		return "(done " + relativeTime(*item.CompletedAt) + ")"
	}
	if !item.UpdatedAt.IsZero() && !item.CreatedAt.IsZero() && !item.UpdatedAt.Equal(item.CreatedAt) {
		return "(updated " + relativeTime(item.UpdatedAt) + ")"
	}
	return "(created " + relativeTime(item.CreatedAt) + ")"
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1d ago"
	}
	return fmt.Sprintf("%dd ago", days)
}

func resolveIDs(id int64, ids []int64) ([]int64, error) {
	if id != 0 && len(ids) > 0 {
		return nil, fmt.Errorf("use id or ids, not both")
	}
	if id != 0 {
		return []int64{id}, nil
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("id is required")
	}
	return ids, nil
}

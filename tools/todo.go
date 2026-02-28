package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/memory"
)

// NewTodoTool creates the todo management tool.
func NewTodoTool(store *memory.TodoStore, agentID string) *Tool {
	return &Tool{
		Name:        "todo",
		ExecExport:  true,
		Description: "Manage a persistent todo list. Supports adding, listing, searching, completing, editing, and removing items. Items have priority (high/medium/low) and optional tags. Items survive restarts.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["add", "list", "search", "complete", "edit", "remove"],
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
					"description": "Todo item ID (required for 'complete', 'edit', and 'remove')"
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
				Action   string `json:"action"`
				Text     string `json:"text"`
				Priority string `json:"priority"`
				Tag      string `json:"tag"`
				ID       int64  `json:"id"`
				Status   string `json:"status"`
				Query    string `json:"query"`
				Reason   string `json:"reason"`
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
				tagStr := ""
				if p.Tag != "" {
					tagStr = fmt.Sprintf(" [%s]", p.Tag)
				}
				return fmt.Sprintf("Added todo #%d (%s%s): %s", id, pri, tagStr, p.Text), nil

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
					line := fmt.Sprintf("#%d %s [%s]%s %s", item.ID, marker, item.Priority, tags, item.Text)
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
					line := fmt.Sprintf("#%d %s [%s]%s %s", item.ID, marker, item.Priority, tags, item.Text)
					if item.Status == "done" && item.CloseReason != "" {
						line += fmt.Sprintf(" — %s", item.CloseReason)
					}
					lines = append(lines, line)
				}
				return strings.Join(lines, "\n"), nil

			case "complete":
				if p.ID == 0 {
					return "", fmt.Errorf("id is required for complete")
				}
				if p.Reason == "" {
					return "", fmt.Errorf("reason is required for complete (e.g. 'implemented in abc1234', 'no longer relevant')")
				}
				if err := store.Complete(agentID, p.ID, p.Reason); err != nil {
					return "", err
				}
				return fmt.Sprintf("Completed todo #%d: %s", p.ID, p.Reason), nil

			case "edit":
				if p.ID == 0 {
					return "", fmt.Errorf("id is required for edit")
				}
				// Detect whether "tag" key was explicitly provided (allows setting to "").
				var raw map[string]json.RawMessage
				json.Unmarshal(params, &raw)
				_, setTags := raw["tag"]

				if p.Text == "" && p.Priority == "" && !setTags {
					return "", fmt.Errorf("edit requires at least one of: text, priority, tag")
				}
				item, err := store.Edit(agentID, p.ID, p.Text, p.Priority, p.Tag, setTags)
				if err != nil {
					return "", err
				}
				tags := memory.FormatTags(item.Tags)
				marker := "[ ]"
				if item.Status == "done" {
					marker = "[x]"
				}
				return fmt.Sprintf("Updated todo #%d: %s [%s]%s %s", item.ID, marker, item.Priority, tags, item.Text), nil

			case "remove":
				if p.ID == 0 {
					return "", fmt.Errorf("id is required for remove")
				}
				if err := store.Remove(agentID, p.ID); err != nil {
					return "", err
				}
				return fmt.Sprintf("Removed todo #%d.", p.ID), nil

			default:
				return "", fmt.Errorf("unknown action: %s (use add, list, search, complete, edit, or remove)", p.Action)
			}
		},
	}
}

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
				return todoAdd(store, agentID, p.Text, p.Priority, p.Tag)
			case "list":
				return todoList(store, agentID, p.Status, p.Tag)
			case "search":
				return todoSearch(store, agentID, p.Query)
			case "get":
				return todoGet(store, agentID, p.ID)
			case "complete":
				return todoComplete(store, agentID, p.ID, p.IDs, p.Reason)
			case "edit":
				return todoEdit(store, agentID, p.ID, p.IDs, p.Text, p.Priority, p.Tag, params)
			case "remove":
				return todoRemove(store, agentID, p.ID, p.IDs)
			default:
				return "", fmt.Errorf("unknown action: %s (use add, list, search, get, complete, edit, or remove)", p.Action)
			}
		},
	}
}

// formatTodoLine formats a single todo item for display.
func formatTodoLine(item memory.TodoItem) string {
	marker := "[ ]"
	if item.Status == "done" {
		marker = "[x]"
	}
	tags := memory.FormatTags(item.Tags)
	line := fmt.Sprintf("#%d %s [%s]%s %s %s", item.ID, marker, item.Priority, tags, item.Text, formatTodoTimestamp(item))
	if item.Status == "done" && item.CloseReason != "" {
		line += fmt.Sprintf(" — %s", item.CloseReason)
	}
	return line
}

// formatTodoLines formats a slice of todo items, one per line.
func formatTodoLines(items []memory.TodoItem) string {
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = formatTodoLine(item)
	}
	return strings.Join(lines, "\n")
}

func todoAdd(store *memory.TodoStore, agentID, text, priority, tag string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("text is required for add")
	}
	id, err := store.Add(agentID, text, priority, tag)
	if err != nil {
		return "", fmt.Errorf("add todo: %w", err)
	}
	pri := priority
	if pri == "" {
		pri = "medium"
	}
	return fmt.Sprintf("Added #%d (%s)", id, pri), nil
}

func todoList(store *memory.TodoStore, agentID, status, tag string) (string, error) {
	items, err := store.List(agentID, status, tag)
	if err != nil {
		return "", fmt.Errorf("list todos: %w", err)
	}
	if len(items) == 0 {
		if status != "" {
			return fmt.Sprintf("No %s todos.", status), nil
		}
		return "No todos.", nil
	}
	return formatTodoLines(items), nil
}

func todoSearch(store *memory.TodoStore, agentID, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("query is required for search")
	}
	items, err := store.Search(agentID, query)
	if err != nil {
		return "", fmt.Errorf("search todos: %w", err)
	}
	if len(items) == 0 {
		return fmt.Sprintf("No todos matching %q.", query), nil
	}
	return formatTodoLines(items), nil
}

func todoGet(store *memory.TodoStore, agentID string, id int64) (string, error) {
	if id == 0 {
		return "", fmt.Errorf("id is required for get")
	}
	item, err := store.Get(agentID, id)
	if err != nil {
		return "", fmt.Errorf("get todo: %w", err)
	}
	return formatTodoLine(*item), nil
}

func todoComplete(store *memory.TodoStore, agentID string, id int64, ids []int64, reason string) (string, error) {
	if reason == "" {
		return "", fmt.Errorf("reason is required for complete (e.g. 'implemented in abc1234', 'no longer relevant')")
	}
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return "", err
	}
	var results []string
	for _, rid := range resolved {
		if err := store.Complete(agentID, rid, reason); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
		} else {
			results = append(results, fmt.Sprintf("#%d: completed", rid))
		}
	}
	return strings.Join(results, "\n"), nil
}

func todoEdit(store *memory.TodoStore, agentID string, id int64, ids []int64, text, priority, tag string, params json.RawMessage) (string, error) {
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return "", err
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(params, &raw)
	_, setTags := raw["tag"]

	if text == "" && priority == "" && !setTags {
		return "", fmt.Errorf("edit requires at least one of: text, priority, tag")
	}
	var results []string
	for _, rid := range resolved {
		oldItem, getErr := store.Get(agentID, rid)
		item, err := store.Edit(agentID, rid, text, priority, tag, setTags)
		if err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
			continue
		}
		if getErr != nil {
			tags := memory.FormatTags(item.Tags)
			results = append(results, fmt.Sprintf("#%d: updated [%s]%s %s", item.ID, item.Priority, tags, item.Text))
			continue
		}
		var changes []string
		if text != "" && oldItem.Text != item.Text {
			changes = append(changes, fmt.Sprintf("text: %s → %s", oldItem.Text, item.Text))
		}
		if priority != "" && oldItem.Priority != item.Priority {
			changes = append(changes, fmt.Sprintf("priority: %s → %s", oldItem.Priority, item.Priority))
		}
		if setTags && oldItem.Tags != item.Tags {
			oldTags := memory.FormatTags(oldItem.Tags)
			newTags := memory.FormatTags(item.Tags)
			if oldTags == "" {
				oldTags = "(none)"
			}
			if newTags == "" {
				newTags = "(none)"
			}
			changes = append(changes, fmt.Sprintf("tags: %s → %s", oldTags, newTags))
		}
		if len(changes) == 0 {
			results = append(results, fmt.Sprintf("#%d: no changes", rid))
		} else {
			results = append(results, fmt.Sprintf("#%d: %s", rid, strings.Join(changes, ", ")))
		}
	}
	return strings.Join(results, "\n"), nil
}

func todoRemove(store *memory.TodoStore, agentID string, id int64, ids []int64) (string, error) {
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return "", err
	}
	var results []string
	for _, rid := range resolved {
		if err := store.Remove(agentID, rid); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
		} else {
			results = append(results, fmt.Sprintf("#%d: removed", rid))
		}
	}
	return strings.Join(results, "\n"), nil
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

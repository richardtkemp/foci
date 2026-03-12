package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/memory"
)

// FormatTasks renders a list of tasks for display. Exported for use by
// compaction (injecting into handoff message).
func FormatTasks(tasks []memory.Task) string {
	if len(tasks) == 0 {
		return "No tasks."
	}

	completed, total := 0, len(tasks)
	for _, t := range tasks {
		if t.Status == "completed" {
			completed++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Tasks: %d/%d completed", completed, total)
	for _, t := range tasks {
		b.WriteString("\n")
		marker := "  "
		switch t.Status {
		case "completed":
			marker = "✓ "
		case "in_progress":
			marker = "→ "
		}
		fmt.Fprintf(&b, "  %d. %s%s", t.ID, marker, t.Subject)
	}
	return b.String()
}

// FormatTask renders a single task for display.
func FormatTask(t *memory.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s", t.ID, t.Subject)
	fmt.Fprintf(&b, "\nStatus: %s", t.Status)
	if t.Description != "" {
		fmt.Fprintf(&b, "\nDescription: %s", t.Description)
	}
	return b.String()
}

// TaskNotifyFunc is called when a task is created or updated.
// Arguments are (sessionKey, message).
type TaskNotifyFunc func(string, string)

// NewTaskListTool creates the task list management tool.
// If notify is non-nil, it fires on create and status-changing updates.
func NewTaskListTool(store *memory.TaskListStore, agentID string, notify TaskNotifyFunc) *Tool {
	return &Tool{
		Name:        "task_list",
		Description: "Track tasks for the current work. Survives compaction. Use create/get/update/list to manage tasks.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["create", "get", "update", "list"],
					"description": "Action to perform"
				},
				"id": {
					"type": "integer",
					"description": "Task ID (required for 'get' and 'update')"
				},
				"subject": {
					"type": "string",
					"description": "Task title (required for 'create'; optional for 'update')"
				},
				"description": {
					"type": "string",
					"description": "Task details (optional for 'create' and 'update')"
				},
				"status": {
					"type": "string",
					"enum": ["pending", "in_progress", "completed", "deleted"],
					"description": "Task status (optional for 'update'; 'deleted' removes the task)"
				}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Action      string `json:"action"`
				ID          int    `json:"id"`
				Subject     string `json:"subject"`
				Description string `json:"description"`
				Status      string `json:"status"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}

			sk := SessionKeyFromContext(ctx)

			switch p.Action {
			case "create":
				return taskCreate(store, agentID, p.Subject, p.Description, sk, notify)
			case "get":
				return taskGet(store, agentID, p.ID)
			case "update":
				return taskUpdate(store, agentID, p.ID, p.Subject, p.Description, p.Status, sk, notify)
			case "list":
				return taskList(store, agentID)
			default:
				return ToolResult{}, fmt.Errorf("unknown action %q (use create, get, update, or list)", p.Action)
			}
		},
	}
}

func taskCreate(store *memory.TaskListStore, agentID, subject, description, sk string, notify TaskNotifyFunc) (ToolResult, error) {
	if subject == "" {
		return ToolResult{}, fmt.Errorf("subject is required for create")
	}
	id, err := store.Create(agentID, subject, description)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create task: %w", err)
	}
	if notify != nil && sk != "" {
		notify(sk, fmt.Sprintf("📋 Created #%d: %s", id, subject))
	}
	return TextResult(fmt.Sprintf("Created task #%d: %s", id, subject)), nil
}

func taskGet(store *memory.TaskListStore, agentID string, id int) (ToolResult, error) {
	if id <= 0 {
		return ToolResult{}, fmt.Errorf("id is required for get")
	}
	task, err := store.Get(agentID, id)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return ToolResult{}, fmt.Errorf("task #%d not found", id)
	}
	return TextResult(FormatTask(task)), nil
}

func taskUpdate(store *memory.TaskListStore, agentID string, id int, subject, description, status, sk string, notify TaskNotifyFunc) (ToolResult, error) {
	if id <= 0 {
		return ToolResult{}, fmt.Errorf("id is required for update")
	}
	if err := store.Update(agentID, id, subject, description, status); err != nil {
		return ToolResult{}, fmt.Errorf("update task: %w", err)
	}
	if status == "deleted" {
		return TextResult(fmt.Sprintf("Task #%d deleted.", id)), nil
	}
	// Re-read to show updated state
	task, err := store.Get(agentID, id)
	if err != nil {
		return ToolResult{}, fmt.Errorf("re-read task: %w", err)
	}
	if notify != nil && sk != "" && status != "" {
		notify(sk, taskNotifyMessage(store, agentID, task))
	}
	return TextResult(FormatTask(task)), nil
}

// taskNotifyMessage builds a notification string for a task status change,
// including progress counts (e.g. "✅ 3/5: Fixed token counting").
func taskNotifyMessage(store *memory.TaskListStore, agentID string, t *memory.Task) string {
	prefix := "📋"
	switch t.Status {
	case "in_progress":
		prefix = "🔄"
	case "completed":
		prefix = "✅"
	}

	// Include progress counts for in_progress and completed.
	tasks, err := store.List(agentID)
	if err == nil && len(tasks) > 0 {
		completed := 0
		for _, task := range tasks {
			if task.Status == "completed" {
				completed++
			}
		}
		return fmt.Sprintf("%s %d/%d: %s", prefix, completed, len(tasks), t.Subject)
	}

	return fmt.Sprintf("%s %s", prefix, t.Subject)
}

func taskList(store *memory.TaskListStore, agentID string) (ToolResult, error) {
	tasks, err := store.List(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("list tasks: %w", err)
	}
	return TextResult(FormatTasks(tasks)), nil
}

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
		Description: "Track tasks for the current work. Survives compaction. Use create/get/update/list to manage tasks. For batch create, put one task per line in subject. When all tasks are completed, the list is automatically cleared.",
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
					"description": "Task title (required for 'create'; optional for 'update'). For batch create, put one task per line — each line becomes a separate task."
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

	// Split by newlines for batch create.
	lines := strings.Split(subject, "\n")
	var subjects []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			subjects = append(subjects, line)
		}
	}

	if len(subjects) == 1 {
		id, err := store.Create(agentID, subjects[0], description)
		if err != nil {
			return ToolResult{}, fmt.Errorf("create task: %w", err)
		}
		if notify != nil && sk != "" {
			notify(sk, fmt.Sprintf("📋 Created #%d: %s", id, subjects[0]))
		}
		return TextResult(fmt.Sprintf("Created task #%d: %s", id, subjects[0])), nil
	}

	// Batch create — one task per non-empty line, description ignored.
	var b strings.Builder
	for _, subj := range subjects {
		id, err := store.Create(agentID, subj, "")
		if err != nil {
			return ToolResult{}, fmt.Errorf("create task %q: %w", subj, err)
		}
		fmt.Fprintf(&b, "Created task #%d: %s\n", id, subj)
	}
	if notify != nil && sk != "" {
		notify(sk, fmt.Sprintf("📋 Created %d tasks", len(subjects)))
	}
	return TextResult(strings.TrimRight(b.String(), "\n")), nil
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

	// Auto-clear: if all tasks are now completed, wipe the list.
	if status == "completed" {
		if cleared := taskAutoClear(store, agentID); cleared {
			return TextResult(FormatTask(task) + "\n\nAll tasks completed — list cleared."), nil
		}
	}

	return TextResult(FormatTask(task)), nil
}

// taskAutoClear clears the task list if every task is completed.
// Returns true if the list was cleared.
func taskAutoClear(store *memory.TaskListStore, agentID string) bool {
	tasks, err := store.List(agentID)
	if err != nil || len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if t.Status != "completed" {
			return false
		}
	}
	_ = store.Clear(agentID)
	return true
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

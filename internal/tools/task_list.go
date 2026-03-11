package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/memory"
)

// FormatTaskList renders a task list for display. Exported for use by
// compaction (injecting into handoff message).
func FormatTaskList(tl *memory.TaskList) string {
	done, total := 0, len(tl.Steps)
	for _, s := range tl.Steps {
		if s.Status == "done" || s.Status == "skipped" {
			done++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s [%d/%d]", tl.Goal, done, total)

	foundCurrent := false
	for i, s := range tl.Steps {
		b.WriteString("\n")
		marker := "  "
		switch s.Status {
		case "done":
			marker = "✓ "
		case "skipped":
			marker = "~ "
		case "pending":
			if !foundCurrent {
				marker = "→ "
				foundCurrent = true
			}
		}
		fmt.Fprintf(&b, "  %d. %s%s", i+1, marker, s.Text)
	}
	return b.String()
}

// CurrentStepSummary returns the display text for the current (first pending) step,
// or empty string if all steps are done. Exported for use by the state dashboard.
func CurrentStepSummary(tl *memory.TaskList) string {
	for _, s := range tl.Steps {
		if s.Status == "pending" {
			return s.Text
		}
	}
	return ""
}

// NewTaskListTool creates the task list management tool.
func NewTaskListTool(store *memory.TaskListStore, agentID string) *Tool {
	return &Tool{
		Name:        "task_list",
		Description: "Ephemeral step tracker for the current task. Use to decompose work into ordered steps, then advance through them. Survives compaction. One active list per agent — creating a new list replaces the old one.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["create", "advance", "add_step", "remove_step", "revise", "status", "clear"],
					"description": "Action to perform"
				},
				"goal": {
					"type": "string",
					"description": "Goal description (required for 'create')"
				},
				"steps": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Step descriptions (required for 'create')"
				},
				"skip": {
					"type": "boolean",
					"description": "If true, 'advance' marks the current step as skipped instead of done"
				},
				"text": {
					"type": "string",
					"description": "Step text (required for 'add_step' and 'revise')"
				},
				"position": {
					"type": "integer",
					"description": "1-indexed step position (required for 'remove_step' and 'revise'; optional for 'add_step' — default: end)"
				}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Action   string   `json:"action"`
				Goal     string   `json:"goal"`
				Steps    []string `json:"steps"`
				Skip     bool     `json:"skip"`
				Text     string   `json:"text"`
				Position int      `json:"position"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return ToolResult{}, fmt.Errorf("parse params: %w", err)
			}

			switch p.Action {
			case "create":
				return taskListCreate(store, agentID, p.Goal, p.Steps)
			case "advance":
				return taskListAdvance(store, agentID, p.Skip)
			case "add_step":
				return taskListAddStep(store, agentID, p.Text, p.Position)
			case "remove_step":
				return taskListRemoveStep(store, agentID, p.Position)
			case "revise":
				return taskListRevise(store, agentID, p.Position, p.Text)
			case "status":
				return taskListStatus(store, agentID)
			case "clear":
				return taskListClear(store, agentID)
			default:
				return ToolResult{}, fmt.Errorf("unknown action %q (use create, advance, add_step, remove_step, revise, status, or clear)", p.Action)
			}
		},
	}
}

func taskListCreate(store *memory.TaskListStore, agentID, goal string, stepTexts []string) (ToolResult, error) {
	if goal == "" {
		return ToolResult{}, fmt.Errorf("goal is required for create")
	}
	if len(stepTexts) == 0 {
		return ToolResult{}, fmt.Errorf("steps is required for create (provide at least one step)")
	}
	steps := make([]memory.TaskStep, len(stepTexts))
	for i, t := range stepTexts {
		steps[i] = memory.TaskStep{Text: t, Status: "pending"}
	}
	if err := store.Set(agentID, goal, steps); err != nil {
		return ToolResult{}, fmt.Errorf("create task list: %w", err)
	}
	tl := &memory.TaskList{Goal: goal, Steps: steps}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListAdvance(store *memory.TaskListStore, agentID string, skip bool) (ToolResult, error) {
	tl, err := store.Get(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task list: %w", err)
	}
	if tl == nil {
		return ToolResult{}, fmt.Errorf("no active task list")
	}

	advanced := false
	for i := range tl.Steps {
		if tl.Steps[i].Status == "pending" {
			if skip {
				tl.Steps[i].Status = "skipped"
			} else {
				tl.Steps[i].Status = "done"
			}
			advanced = true
			break
		}
	}
	if !advanced {
		return TextResult("All steps already completed.\n" + FormatTaskList(tl)), nil
	}

	if err := store.Set(agentID, tl.Goal, tl.Steps); err != nil {
		return ToolResult{}, fmt.Errorf("save task list: %w", err)
	}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListAddStep(store *memory.TaskListStore, agentID, text string, position int) (ToolResult, error) {
	if text == "" {
		return ToolResult{}, fmt.Errorf("text is required for add_step")
	}
	tl, err := store.Get(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task list: %w", err)
	}
	if tl == nil {
		return ToolResult{}, fmt.Errorf("no active task list")
	}

	step := memory.TaskStep{Text: text, Status: "pending"}
	if position <= 0 || position > len(tl.Steps)+1 {
		// Append to end
		tl.Steps = append(tl.Steps, step)
	} else {
		// Insert at position (1-indexed)
		idx := position - 1
		tl.Steps = append(tl.Steps, memory.TaskStep{})
		copy(tl.Steps[idx+1:], tl.Steps[idx:])
		tl.Steps[idx] = step
	}

	if err := store.Set(agentID, tl.Goal, tl.Steps); err != nil {
		return ToolResult{}, fmt.Errorf("save task list: %w", err)
	}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListRemoveStep(store *memory.TaskListStore, agentID string, position int) (ToolResult, error) {
	tl, err := store.Get(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task list: %w", err)
	}
	if tl == nil {
		return ToolResult{}, fmt.Errorf("no active task list")
	}
	if position <= 0 || position > len(tl.Steps) {
		return ToolResult{}, fmt.Errorf("position %d out of range (1-%d)", position, len(tl.Steps))
	}

	idx := position - 1
	tl.Steps = append(tl.Steps[:idx], tl.Steps[idx+1:]...)

	if err := store.Set(agentID, tl.Goal, tl.Steps); err != nil {
		return ToolResult{}, fmt.Errorf("save task list: %w", err)
	}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListRevise(store *memory.TaskListStore, agentID string, position int, text string) (ToolResult, error) {
	if text == "" {
		return ToolResult{}, fmt.Errorf("text is required for revise")
	}
	tl, err := store.Get(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task list: %w", err)
	}
	if tl == nil {
		return ToolResult{}, fmt.Errorf("no active task list")
	}
	if position <= 0 || position > len(tl.Steps) {
		return ToolResult{}, fmt.Errorf("position %d out of range (1-%d)", position, len(tl.Steps))
	}

	tl.Steps[position-1].Text = text

	if err := store.Set(agentID, tl.Goal, tl.Steps); err != nil {
		return ToolResult{}, fmt.Errorf("save task list: %w", err)
	}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListStatus(store *memory.TaskListStore, agentID string) (ToolResult, error) {
	tl, err := store.Get(agentID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get task list: %w", err)
	}
	if tl == nil {
		return TextResult("No active task list."), nil
	}
	return TextResult(FormatTaskList(tl)), nil
}

func taskListClear(store *memory.TaskListStore, agentID string) (ToolResult, error) {
	if err := store.Clear(agentID); err != nil {
		return ToolResult{}, fmt.Errorf("clear task list: %w", err)
	}
	return TextResult("Task list cleared."), nil
}

package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/memory"
)

func testTaskListTool(t *testing.T) (*Tool, *memory.TaskListStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := memory.NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewTaskListTool(s, "test"), s
}

func execTaskList(t *testing.T, tool *Tool, params any) (ToolResult, error) {
	t.Helper()
	data, _ := json.Marshal(params)
	return tool.Execute(context.Background(), data)
}

// Verifies create produces a formatted task list with all steps pending.
func TestTaskListCreate(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	result, err := execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Boil an egg",
		"steps":  []string{"Fill pot", "Boil water", "Add egg"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(result.Text, "Boil an egg") {
		t.Errorf("missing goal in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[0/3]") {
		t.Errorf("missing progress in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "→ Fill pot") {
		t.Errorf("missing current marker in result: %q", result.Text)
	}
}

// Verifies create requires goal and steps.
func TestTaskListCreateValidation(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "create", "goal": "", "steps": []string{"a"}})
	if err == nil {
		t.Error("expected error for empty goal")
	}
	_, err = execTaskList(t, tool, map[string]any{"action": "create", "goal": "G"})
	if err == nil {
		t.Error("expected error for missing steps")
	}
}

// Verifies advance marks steps done and updates the current pointer.
func TestTaskListAdvance(t *testing.T) {
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Step 1", "Step 2", "Step 3"},
	})

	// Advance first step
	result, err := execTaskList(t, tool, map[string]any{"action": "advance"})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !strings.Contains(result.Text, "[1/3]") {
		t.Errorf("expected [1/3], got: %q", result.Text)
	}
	if !strings.Contains(result.Text, "✓ Step 1") {
		t.Errorf("missing done marker: %q", result.Text)
	}
	if !strings.Contains(result.Text, "→ Step 2") {
		t.Errorf("missing current marker on step 2: %q", result.Text)
	}

	// Verify store was updated
	tl, _ := store.Get("test")
	if tl.Steps[0].Status != "done" {
		t.Errorf("step 0 status = %q, want done", tl.Steps[0].Status)
	}
}

// Verifies advance with skip=true marks the step as skipped.
func TestTaskListAdvanceSkip(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Step 1", "Step 2"},
	})

	result, err := execTaskList(t, tool, map[string]any{"action": "advance", "skip": true})
	if err != nil {
		t.Fatalf("advance skip: %v", err)
	}
	if !strings.Contains(result.Text, "~ Step 1") {
		t.Errorf("missing skip marker: %q", result.Text)
	}
	if !strings.Contains(result.Text, "[1/2]") {
		t.Errorf("expected [1/2], got: %q", result.Text)
	}
}

// Verifies advance when all steps are already done.
func TestTaskListAdvanceAllDone(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Only step"},
	})
	execTaskList(t, tool, map[string]any{"action": "advance"})

	result, err := execTaskList(t, tool, map[string]any{"action": "advance"})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !strings.Contains(result.Text, "All steps already completed") {
		t.Errorf("expected completion message, got: %q", result.Text)
	}
}

// Verifies advance with no active task list returns error.
func TestTaskListAdvanceNoList(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "advance"})
	if err == nil {
		t.Error("expected error for advance with no list")
	}
}

// Verifies add_step appends to end by default.
func TestTaskListAddStep(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Step 1", "Step 2"},
	})

	result, err := execTaskList(t, tool, map[string]any{
		"action": "add_step",
		"text":   "Step 3",
	})
	if err != nil {
		t.Fatalf("add_step: %v", err)
	}
	if !strings.Contains(result.Text, "[0/3]") {
		t.Errorf("expected [0/3], got: %q", result.Text)
	}
	if !strings.Contains(result.Text, "3. ") {
		t.Errorf("missing step 3: %q", result.Text)
	}
}

// Verifies add_step with position inserts at the right place.
func TestTaskListAddStepAtPosition(t *testing.T) {
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"First", "Last"},
	})

	_, err := execTaskList(t, tool, map[string]any{
		"action":   "add_step",
		"text":     "Middle",
		"position": 2,
	})
	if err != nil {
		t.Fatalf("add_step: %v", err)
	}

	tl, _ := store.Get("test")
	if len(tl.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(tl.Steps))
	}
	if tl.Steps[1].Text != "Middle" {
		t.Errorf("step[1] = %q, want Middle", tl.Steps[1].Text)
	}
}

// Verifies remove_step removes the correct step.
func TestTaskListRemoveStep(t *testing.T) {
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Keep", "Remove", "Keep too"},
	})

	result, err := execTaskList(t, tool, map[string]any{
		"action":   "remove_step",
		"position": 2,
	})
	if err != nil {
		t.Fatalf("remove_step: %v", err)
	}
	if !strings.Contains(result.Text, "[0/2]") {
		t.Errorf("expected [0/2], got: %q", result.Text)
	}

	tl, _ := store.Get("test")
	if len(tl.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(tl.Steps))
	}
	if tl.Steps[0].Text != "Keep" || tl.Steps[1].Text != "Keep too" {
		t.Errorf("steps = %+v", tl.Steps)
	}
}

// Verifies remove_step with out-of-range position returns error.
func TestTaskListRemoveStepOutOfRange(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Only"},
	})

	_, err := execTaskList(t, tool, map[string]any{"action": "remove_step", "position": 5})
	if err == nil {
		t.Error("expected error for out-of-range position")
	}
}

// Verifies revise updates step text in place.
func TestTaskListRevise(t *testing.T) {
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"Old text", "Other"},
	})

	result, err := execTaskList(t, tool, map[string]any{
		"action":   "revise",
		"position": 1,
		"text":     "New text",
	})
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if !strings.Contains(result.Text, "New text") {
		t.Errorf("missing revised text: %q", result.Text)
	}

	tl, _ := store.Get("test")
	if tl.Steps[0].Text != "New text" {
		t.Errorf("step[0] = %q, want New text", tl.Steps[0].Text)
	}
}

// Verifies status returns the current list or "no active" message.
func TestTaskListStatus(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	// No list
	result, err := execTaskList(t, tool, map[string]any{"action": "status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(result.Text, "No active task list") {
		t.Errorf("expected no-list message, got: %q", result.Text)
	}

	// With list
	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "My goal",
		"steps":  []string{"A", "B"},
	})
	result, err = execTaskList(t, tool, map[string]any{"action": "status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(result.Text, "My goal") {
		t.Errorf("missing goal: %q", result.Text)
	}
}

// Verifies clear removes the list.
func TestTaskListClear(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Test",
		"steps":  []string{"A"},
	})

	result, err := execTaskList(t, tool, map[string]any{"action": "clear"})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !strings.Contains(result.Text, "cleared") {
		t.Errorf("expected cleared message, got: %q", result.Text)
	}

	result, _ = execTaskList(t, tool, map[string]any{"action": "status"})
	if !strings.Contains(result.Text, "No active") {
		t.Errorf("expected no-list after clear, got: %q", result.Text)
	}
}

// Verifies unknown action returns an error.
func TestTaskListUnknownAction(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "reset"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error = %q", err.Error())
	}
}

// Verifies FormatTaskList renders markers correctly for mixed statuses.
func TestFormatTaskList(t *testing.T) {
	t.Parallel()
	tl := &memory.TaskList{
		Goal: "Boil an egg",
		Steps: []memory.TaskStep{
			{Text: "Fill pot", Status: "done"},
			{Text: "Boil water", Status: "skipped"},
			{Text: "Add egg", Status: "pending"},
			{Text: "Wait", Status: "pending"},
		},
	}

	result := FormatTaskList(tl)
	if !strings.Contains(result, "[2/4]") {
		t.Errorf("missing [2/4]: %q", result)
	}
	if !strings.Contains(result, "✓ Fill pot") {
		t.Errorf("missing done marker: %q", result)
	}
	if !strings.Contains(result, "~ Boil water") {
		t.Errorf("missing skip marker: %q", result)
	}
	if !strings.Contains(result, "→ Add egg") {
		t.Errorf("missing current marker: %q", result)
	}
	// "Wait" should have no marker (plain indent)
	if strings.Contains(result, "→ Wait") {
		t.Errorf("Wait should not have current marker: %q", result)
	}
}

// Verifies creating a new list replaces the old one.
func TestTaskListCreateReplacesExisting(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "Old",
		"steps":  []string{"Old step"},
	})
	result, err := execTaskList(t, tool, map[string]any{
		"action": "create",
		"goal":   "New",
		"steps":  []string{"New step"},
	})
	if err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	if strings.Contains(result.Text, "Old") {
		t.Errorf("old goal still present: %q", result.Text)
	}
	if !strings.Contains(result.Text, "New") {
		t.Errorf("missing new goal: %q", result.Text)
	}
}

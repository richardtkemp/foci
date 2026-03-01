# Todo Tool: Add updated_at and display timestamps

## Problem
Todo items have `created_at` and `completed_at` in the schema but:
1. They're never displayed in list/search output
2. There's no `updated_at` column to track when items were last edited

## Changes needed

### 1. Schema migration — `memory/todo.go`
Add `updated_at TEXT` column via `ALTER TABLE` after the CREATE TABLE (same pattern as any missing column — check if it exists first with PRAGMA table_info, add if missing).

### 2. Set updated_at — `memory/todo.go`
- `Add()`: set `updated_at` = now (same as created_at)
- `Edit()`: set `updated_at` = now in the SET clauses (always, not conditional)
- `Complete()`: set `updated_at` = now alongside completed_at
- Add `UpdatedAt` field to `TodoItem` struct
- Update all SELECT queries and `scanTodos` to include `updated_at`

### 3. Display timestamps — `tools/todo.go`
In list and search output, append a compact timestamp after each line. Format as relative time for readability:
- `(created 2h ago)` for open items
- `(done 3d ago)` for completed items  
- If updated_at differs from created_at on open items: `(updated 1h ago)`

Use a helper like `relativeTime(t time.Time) string` that returns "Xm ago", "Xh ago", "Xd ago", etc.

### 4. Update tests — `memory/todo_test.go`
- Verify updated_at is set on Add
- Verify updated_at changes on Edit
- Verify updated_at changes on Complete

## Files to modify
- `memory/todo.go` — struct, schema migration, queries
- `memory/todo_test.go` — new tests
- `tools/todo.go` — display formatting

## Commit message
`feat: add updated_at timestamp and display relative times in todo output`

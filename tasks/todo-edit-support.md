# Todo tool: add edit action

## Goal
Add an `edit` action to the todo tool that can update tags and priority on existing items.

## Required
- New action: `edit` 
- Parameters: `id` (required), `priority` (optional), `tag` (optional), `text` (optional)
- Only updates fields that are provided — omitted fields stay unchanged
- Returns the updated item

## Example usage
```
todo action:edit id:97 tag:background
todo action:edit id:99 priority:high tag:background
todo action:edit id:42 text:"Updated description" priority:low
```

## Context
- Todo tool is in `tools/todo.go`
- Storage is in `tools/todo_store.go` (or similar)
- Needs to be registered alongside existing actions (add, list, search, complete, remove)
- Update the tool's JSON schema to include the new action in the enum
- Write tests
- Update docs and push when done

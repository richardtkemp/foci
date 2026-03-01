# TODO #154: Batch Operations for Todo Tool

## Problem
Tagging or completing multiple todos requires one tool call per item. With 15+ items, this wastes context and turns.

## Solution
Accept `ids` (array of ints) as alternative to `id` (single int) for `complete`, `edit`, and `remove` actions. When `ids` is provided, apply the operation to all listed items. Return a summary line per item.

## Changes

### tools/todo.go
1. Add `IDs []int` field to the params struct with `json:"ids"` tag
2. For `complete`, `edit`, `remove`: if `p.IDs` is set (len > 0), loop over them. If `p.ID` is also set, error ("use id or ids, not both").
3. Collect results per item, return joined with newlines.
4. If any item fails, include the error in output but continue processing remaining items.

### tool schema (in tools/todo.go, the tool definition)
Add `ids` parameter:
```json
"ids": {
  "description": "Array of todo item IDs (alternative to 'id' for batch operations, used with 'complete', 'edit', 'remove')",
  "type": "array",
  "items": {"type": "integer"}
}
```

### Tests
Add tests in tools/todo_test.go or memory/todo_test.go:
- Batch complete 3 items with same reason
- Batch edit tags on 3 items  
- Batch remove 2 items
- Error: both id and ids provided
- Partial failure: one invalid ID in batch, others succeed

## Not in scope
- Batch add (doesn't make sense)
- Batch search/list (already returns multiple)

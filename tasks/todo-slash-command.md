# Task: /todo slash command (#178)

## Goal
Add a `/todo` slash command that shows open todo items directly, without going through the agent pipeline.

## Behavior
- `/todo` with no args: list open items sorted by priority (high→medium→low), hide items tagged "background", limit 20
- `/todo all`: same but include background-tagged items
- `/todo search <query>`: search open items matching query

## Implementation
Add `NewTodoCommand` in `command/builtins.go` following the pattern of existing commands (e.g. `NewStatusCommand`, `NewManaCommand`).

The command needs access to the todo database. Look at how `tools/todo.go` accesses `memory.TodoStore` — the slash command will need the same store passed in.

### Output format
```
📋 Open todos (showing 20 of N, hiding M background items)

🔴 #214 [high] Security: read tool blocked paths...
🔴 #218 [high] Bug: per-user chat routing...
🟡 #116 [medium] Proactive warning injection...
🟢 #136 [low] /tmux slash command usage...
```

Use 🔴 high, 🟡 medium, 🟢 low emoji prefixes. Truncate text to ~80 chars.

## Registration
Register it in `main.go` wherever other commands are registered (search for `RegisterCommand`).

## Tests
- Test with mock store: correct sorting, background filtering, limit
- Test `/todo all` includes background items
- Test `/todo search` filters correctly

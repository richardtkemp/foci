# Task: Cost Command Fixes

## Three changes:

### 1. `/cost` (no args) — show usage
Currently shows today's costs. Change to show usage:
```
/cost today — today's costs by session
/cost 24h — last 24 hours with category breakdown
/cost week — 7-day summary with daily breakdown
/cost <days> — total for last N days
```

### 2. `/cost today` — current default behaviour
Move the current no-args behaviour to the explicit `today` subcommand. The existing `"today"` case and the default `""` case currently do the same thing — just swap them so `""` shows usage and `"today"` shows the table.

### 3. `/cost 24h` and `/cost week` — render as tables
These currently show plain text. They should render as formatted tables in code blocks, same visual style as `/cost today`. Use the same per-session grouping or per-day grouping as appropriate, with aligned columns in a code block.

## Verification
- `/cost` shows usage help
- `/cost today` shows the per-session table (same as current default)
- `/cost 24h` shows a formatted table in code block
- `/cost week` shows a formatted table in code block
- Update tests, SPEC.md
- `go build && go test ./... && go vet ./...`

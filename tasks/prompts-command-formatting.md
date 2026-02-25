# Task: Prompts Command Formatting

The `/prompts` output needs code blocks to preserve alignment in Telegram. Same pattern as `/config table` and `/context`.

Wrap each section in its own code block (triple backticks) so they render as distinct visual groups with preserved spacing.

## Verification
- `/prompts` renders with proper alignment in Telegram
- Update tests to expect backtick wrapping
- `go build && go test ./... && go vet ./...`

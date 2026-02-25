# Task: Context Command Formatting

The `/context` output needs to be wrapped in code blocks to preserve alignment in Telegram. Use separate code blocks for each section so they render as distinct visual groups.

## Example output structure

```
Context: 45,231 / 200,000 tokens (22.6%)
Compaction at 160,000 (80%)
```

```
System prompt: 18,400 chars
  SOUL.md          3,200
  COHERENCE.md     2,100
  CRAFT.md         4,800
  USER.md          2,300
  MEMORY.md        3,500
  Skills list      1,200
  Environment      1,300
```

```
Conversation: 26,831 chars
  User (12 msgs)       8,200
  Assistant (11 msgs) 12,400
  Tool results (8)     6,231
```

Each section in its own code block (triple backticks). The sections should be visually distinct when rendered in Telegram.

## Verification
- `/context` renders with proper alignment in Telegram
- Each section is a separate code block
- Update tests to expect the backtick wrapping
- `go build && go test ./... && go vet ./...`

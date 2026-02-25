# Task: Improve /context Command (#49)

## Current behaviour
`/context` shows context size and compaction threshold — just a couple of numbers.

## New behaviour
Show a full breakdown of what's in the context window:

1. **System prompt breakdown:**
   - Total system prompt tokens
   - Per-section breakdown: each character/system file (by filename), skills list, environment block
   - Compaction summary (if present, show its size)

2. **Conversation breakdown:**
   - Total conversation tokens
   - Split by role: user messages, assistant messages, tool results

3. **Overall:**
   - Total context used vs model max
   - Compaction threshold and how close we are
   - Percentage used

Use token counts where feasible (the anthropic token counting from the API response metadata). If exact token counts aren't available for subsections, character counts with a note are acceptable.

## Format
Readable text output, not a wall of numbers. Something like:
```
Context: 45,231 / 200,000 tokens (22.6%)
Compaction at: 160,000 (80%)

System prompt: 18,400 tokens
  SOUL.md          3,200
  COHERENCE.md     2,100
  CRAFT.md         4,800
  USER.md          2,300
  MEMORY.md        3,500
  Skills list      1,200
  Environment      1,300

Conversation: 26,831 tokens
  User messages    8,200
  Assistant        12,400
  Tool results     6,231
```

## Implementation
- Find where `/context` is currently handled
- The system prompt sections are assembled in the agent/session layer — their sizes should be accessible
- Token counts from API responses are tracked somewhere — use those for conversation totals
- Update SPEC.md and any command reference docs

## Verification
- `/context` shows the full breakdown
- Numbers are plausible (not zeros or obviously wrong)
- `go build && go test ./... && go vet ./...`

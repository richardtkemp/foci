# Task: Fix Telegram polling panic on /agents command

## Bug
`/agents` command triggers panic: `runtime error: index out of range [1] with length 1`

The panic is in the Telegram polling goroutine and recovers, but the command fails.

## Root cause (likely)
`telegram/markdown.go` has multiple `FindStringSubmatch(match)[1]` calls (lines 143, 159, 397, 401, 405) without checking the slice length. If a regex matches but the capture group doesn't capture, `FindStringSubmatch` returns a slice of length 1 (just the full match, no capture groups), and `[1]` panics.

## Fixes needed

### 1. Guard all FindStringSubmatch[1] accesses
In `telegram/markdown.go`, every `FindStringSubmatch(match)[1]` should check length first:
```go
parts := re.FindStringSubmatch(match)
if len(parts) < 2 {
    return match // leave unchanged
}
inner := parts[1]
```

Apply to lines: 143, 159, 397, 401, 405.

### 2. Add stack trace to panic recovery
In `telegram/bot.go` line 370, add stack trace to the log:
```go
if r := recover(); r != nil {
    log.Errorf("telegram", "panic in polling: %v\n%s", r, debug.Stack())
}
```
Import `runtime/debug` if not already imported.

### 3. Test
Write a test that passes edge-case markdown through ConvertToTelegramHTML:
- Code block with `─` unicode characters
- Empty code block (` ```\n``` `)
- Code block with no language tag and no trailing newline

## Update docs
None needed — bug fix only.

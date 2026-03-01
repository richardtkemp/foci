# Review: Uncommitted Changes in telegram/bot.go

Reviewing `git diff telegram/bot.go` — the three changes: new `sendReplyWithFullThinking`, moved `show_thinking == "true"` branch, updated `formatThinkingExpanded`.

---

## 1. Thinking silently dropped when tool-call preview edit succeeds

**Severity: Bug**

`bot.go:1006-1033` — When `showToolCalls == "preview"` and the edit succeeds, the function returns at line 1030 before reaching the thinking branches at lines 1035-1045. Thinking text is accumulated in `thinkingBuf` but never displayed.

This means: if an agent has both `show_tool_calls = "preview"` and `show_thinking = "compact"` (or `"true"`), thinking is silently dropped whenever the final response fits in a single edit.

**Fix:** Move the thinking branches before the tool-call edit block, or integrate thinking into the edit path.

---

## 2. `formatThinkingExpanded` truncation can split HTML entities

**Severity: Bug**

`bot.go:2084-2085`:
```go
escaped = escaped[:budget] + "\n... (truncated)"
```

After `htmlEscapeBot`, `escaped` contains entities like `&amp;`, `&lt;`, `&gt;`. Slicing at an arbitrary byte offset can split an entity mid-way (e.g. `&am` from `&amp;`), producing broken HTML that Telegram will reject.

**Fix:** After truncating, trim any trailing partial entity. Something like:
```go
// Trim any trailing partial HTML entity
if idx := strings.LastIndex(escaped[:budget], "&"); idx != -1 {
    if !strings.Contains(escaped[idx:budget], ";") {
        budget = idx
    }
}
```

Same issue exists in the `budget <= 100` fallback path (line 2087) — `escaped[:100]` has the same entity-splitting risk.

---

## 3. `sendReplyWithFullThinking` ignores send errors

**Severity: Minor**

`bot.go:1089`:
```go
b.client.SendMessage(msg.Chat.Id, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
```

The error return is discarded. If the first chunk fails (e.g. HTML parse error), subsequent chunks still send, producing garbled output. The conversation log at line 1091 also logs the full un-sent HTML regardless of whether any sends succeeded.

Compare with `sendReplyWithThinking` (line 1123) which at least logs the error.

---

## 4. `thinkingStore` / `toolResults` never cleaned up — unbounded memory growth

**Severity: Latent issue (pre-existing)**

Neither `thinkingStore` nor `toolResults` (`sync.Map`) are ever cleaned up. Every compact-mode response and every full-mode tool call stores data permanently. Over days/weeks of uptime this grows without bound.

Not introduced by this diff (same pattern as `toolResults`), but worth noting since it's now doubled with `thinkingStore`.

---

## 5. Consistency: divider mismatch with SPEC

**Severity: Cosmetic**

SPEC.md line 290 says `"true"` mode uses `~~~~` as separator. The code now uses `————————————————` (em dashes). The SPEC should be updated to match, or vice versa.

---

## 6. Message flow correctness — the moved branch

The move from before the tool-call edit to after it is correct in intent: it avoids prepending thinking text before attempting a preview edit (which would break the preview pattern). But see issue #1 — the preview edit's early return now means thinking is dropped in that path.

---

## 7. HTML safety

`htmlEscapeBot` correctly escapes `&`, `<`, `>`. The italic wrapping `<i>...</i>` is safe as long as the escaped content doesn't contain unmatched tags — which it can't after escaping. `splitMessage`/`splitChunk` properly tracks and re-opens HTML tags across chunk boundaries. This is sound.

---

## Summary

| # | Issue | Severity | Status |
|---|-------|----------|--------|
| 1 | Thinking dropped when preview edit succeeds | Bug | **Fixed** |
| 2 | Truncation splits HTML entities | Bug | **Fixed** |
| 3 | Send errors ignored in `sendReplyWithFullThinking` | Minor | Not fixed |
| 4 | `thinkingStore` never cleaned up | Latent | Not fixed (pre-existing pattern) |
| 5 | Divider mismatch with SPEC | Cosmetic | **Fixed** |

## Fixes applied

- **Issue 1:** Added `hasThinking` guard to skip the preview-edit path when thinking needs to be displayed. The thinking-aware send functions handle the full reply instead.
- **Issue 2:** Extracted `truncateHTMLSafe` helper that checks for partial `&` entities after truncation and backs up to before the `&`.
- **Issue 5:** Updated SPEC.md to say "separated by a divider" instead of `~~~~`.

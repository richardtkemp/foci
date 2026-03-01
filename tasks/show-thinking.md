# Task: show_thinking config — control thinking block display in Telegram

## Config

`show_thinking` — per-agent and global (in `[defaults]`). String type, consistent with `show_tool_calls` pattern. Values:
- `"off"` (default) — thinking blocks stripped, not shown to user
- `"compact"` — message sent without thinking, but with an inline keyboard button "Show thinking". Pressing it edits the message to prepend the thinking content. Button toggles to "Hide thinking". Pressing again removes the thinking content.
- `"full"` — messages sent with full thinking content, no button

Accept bool for backwards compat: `true` → `"full"`, `false` → `"off"`.

## Display format

When thinking is shown (either via compact toggle or true mode):

```
$thinking_thoughts
~~~~
$final_response
```

The `~~~~` is a visual separator between thinking and response.

## Important rules

- `<thinking>` tags must NEVER appear in Telegram messages — always strip the XML tags, only show the inner content
- The thinking content itself is plain text (the model's internal reasoning)
- This is similar to `show_tool_calls` pattern — per-agent pointer type overrides global default

## Compact mode implementation

Same pattern as show_tool_calls "full" button:
- Store thinking content keyed by message ID
- Inline keyboard button "Show thinking" / "Hide thinking"
- Edit message on button press to prepend/remove thinking block
- Thinking content should persist across restarts (same 48h TTL as tool call details, see TODO #171)

## Update docs

- Update SPEC.md and docs/CONFIG.md with the new config option
- Commit and push when done

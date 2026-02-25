# Task: Context warning for oneshot/no_compact sessions (#46)

## Problem

There's a "consider compacting" warning that fires when message count > 80. This is useless — compaction is automatic. What's actually needed: oneshot and no_compact sessions CAN'T compact, so they need a warning when their context is running low, letting the agent wrap up gracefully.

## Change

Replace the message-count-based "consider compacting" warning with a useful context usage warning for sessions that have compaction disabled (oneshot, no_compact).

- Check context usage from the API response (token counts)
- When a no_compact/oneshot session exceeds ~80% of the model's context window, inject a warning
- Something like: "[SYSTEM] Context is at ~X% capacity. This session cannot compact. Consider wrapping up."
- Remove the old message-count-based warning entirely

## Files to check

- Look for the existing "consider compacting" warning and replace it
- Check how token usage is available from the API response
- Check how session type (oneshot/no_compact) is determined

## When done: write/update tests, update SPEC.md and docs/ as needed, commit with descriptive message, push.

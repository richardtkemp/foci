# Bug: compaction_preserve_messages not preserving messages

## Problem

After compaction, the agent only sees messages that arrived after the compaction summary. The `compaction_preserve_messages` setting (default 25) should preserve the last 25 messages from before compaction, appended verbatim after the summary. This doesn't seem to be working.

## Expected behavior

After compaction, conversation should contain:
1. The compaction summary (as a user message)
2. The last 25 messages from before compaction (verbatim)
3. Any new messages after compaction

## Actual behavior

After compaction, the agent only sees the summary + new messages. The 25 preserved messages are missing.

## Where to look

- `compaction/compact.go` — the `Compact()` method, specifically the `preserveMessages` / `preserveN` logic
- Check how preserved messages are appended after the summary
- Check whether the session file is written correctly with preserved messages
- Check whether the session is reloaded correctly after compaction

## Config

The default is 25 (set in config.go when `compaction_preserve_messages` is not explicitly configured). No per-agent override is set.

## Instructions

- Investigate the bug — read the compaction code, trace the flow, check tests
- Fix it if you find the issue
- Add a test that verifies preserved messages survive compaction
- Update docs if needed
- Commit and push

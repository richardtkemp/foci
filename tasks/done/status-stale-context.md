# Task: Fix stale context tokens in /status after compaction (#207)

## Problem
After compaction, `/status` shows inflated context token count because it reads the last API log entry for the session — which is the compaction summary call (made with pre-compaction context). The actual post-compaction context is much smaller.

## Root Cause
In `command/builtins.go`, `NewStatusCommand` iterates API log entries and takes the last one's tokens:
```go
contextTokens = e.Input + e.CacheRead + e.CacheWrite
```
After compaction, the last entry is the compaction API call, which had the full pre-compaction context.

## Fix
Add a way to detect compaction API calls in the log and skip them.

**Option A (simple):** Add a `compaction` or `is_compaction` field to the API log entry. When computing context tokens in /status, skip entries where `is_compaction == true`. The compactor already has access to the logger — just need to mark the entry.

**Option B (simpler):** After compaction, if the last API log entry for this session is a compaction call, show "context: refreshing..." or similar instead of stale numbers. The next regular API call will have accurate numbers.

**Recommended: Option A** — it's cleaner and makes compaction calls filterable for other purposes too (cost analysis, etc.).

## Files to Modify
- `anthropic/api.go` or wherever API log entries are written — add `is_compaction` bool field
- `compaction/compactor.go` — mark the API call as compaction
- `command/builtins.go` — skip compaction entries when computing context tokens

## Tests
- After a compaction log entry, /status should show context from the previous non-compaction entry (or "unavailable" if none)

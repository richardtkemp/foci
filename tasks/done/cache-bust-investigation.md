# Investigation: Cache bust caused by flushed in-flight messages

## READ ONLY — do not make any changes

## What happened

On 2026-02-26, session `agent:clutch:chat:5970082313` experienced a cache bust:
- Last successful cache hit: 10:02:20 (77,283 tokens read from cache)
- Next API call: 10:05:35 (0 tokens read from cache, $1.46 cost)
- Gap: 3 minutes 15 seconds (well within Anthropic's 5-minute cache TTL)
- No restarts between these two calls
- Only one cache bust all day despite multiple restarts/deploys

## Key event between the two calls

At 10:01:39, a `/stop` command cancelled an in-flight agent turn. The shutdown defer in `agent.go` (~line 686) flushed 3 unsaved messages to the session file:
```
2026-02-26T10:01:39Z INFO  [telegram] cancelling agent turn via /stop
2026-02-26T10:01:39Z INFO  [agent] flushed 3 in-flight messages for agent:clutch:chat:5970082313
```

Note: there were successful API calls AFTER the flush (10:01:52, 10:01:55, 10:01:59, 10:02:20) that still had cache hits. So the flush alone didn't immediately cause the bust.

## Hypothesis

The flushed messages may have been partial/incomplete (e.g. tool_use without matching tool_result, or an assistant turn that was interrupted). When the session was next loaded for an API call, these incomplete messages may have changed the conversation structure in a way that altered the prompt prefix, busting the cache.

But this is complicated by the fact that 4 more successful cached calls happened after the flush. So maybe the flush isn't the cause at all.

## What to investigate

1. Read `agent/agent.go` — understand what `newMessages` contains when flushed on cancel. Are they complete message pairs or potentially orphaned?
2. Read `session/store.go` or wherever sessions are loaded — how are messages loaded and does it handle incomplete turns?
3. Read the compaction code if relevant — did a compaction happen between 10:02 and 10:05?
4. Check if `RepairOrphans` or any message cleanup runs between calls
5. Look at how the Anthropic cache prefix is constructed — what exactly needs to match?
6. Consider: could something else have changed the prefix in that 3-minute gap?

## Report

Write your findings to `/home/rich/git/clod/tasks/cache-bust-findings.md`. Do NOT modify any source files.

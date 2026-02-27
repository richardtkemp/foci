# Task: Block compaction while async tool results are pending

## Problem

Compaction currently fires at end-of-turn when `StopReason != "tool_use"`. But some tools run asynchronously — `spawn` with `clone_current`, `http_request` with `background: true`, `tmux watch` notifications. These return an immediate ack to the agent, the turn ends, compaction fires, and then the async result arrives into a compacted context that's lost the original request.

Example flow:
1. Agent calls `spawn` with `clone_current` 
2. Tool returns "running in background" immediately
3. Agent's turn ends (StopReason = end_turn)
4. Compaction fires — summarises everything including the spawn request
5. Spawn result arrives later via `async_notify` — context has been compacted, spawn context is lost

## Required Changes

1. **Track pending async results per session** — When a tool dispatches an async result (spawn clone_current, background http_request), increment a counter for that session. When the async result is delivered, decrement it.

2. **Block compaction while async results are pending** — In the compaction check (agent.go ~line 877), skip compaction if the session has pending async results. Compaction will naturally trigger on a later turn once all results have been delivered and the agent has responded to them.

3. **Don't over-engineer this** — A simple atomic counter per session is fine. No need for tracking individual result IDs or timeouts.

## What NOT to do

- Don't add any "proactive compaction" or cross-session compaction
- Don't change the compaction threshold or timing logic
- Don't try to handle restart interruption (restarts are restarts, file rotation already protects data)

## Files to check

- `agent/agent.go` — HandleTurn compaction block (~line 876)
- `compaction/compact.go` — Compact() function, context handling
- `session/store.go` — Replace() function, file rotation
- `main.go` — gracefulShutdown()

## Testing

- Verify IsProcessing() is true during compaction
- Verify Compact() respects context cancellation
- Verify interrupted compaction doesn't write partial session
- Update any relevant docs

---

# Task 2: Investigate and fix OAuth token refresh (#135)

## Problem

OAuth tokens expire every ~2 hours. The OAuthManager in `anthropic/oauth.go` has proactive refresh (background ticker every 5min, refreshes within 30min of expiry) and reactive refresh (on 401). But tokens still expire overnight, causing 401 errors on heartbeats.

The credentials file is `~/.claude/.credentials.json`, shared with Claude Code. CC refreshes tokens when it runs, but when CC isn't running (overnight, idle periods), foci needs to handle refresh independently.

## Current state

- OAuthManager reads `~/.claude/.credentials.json` on startup
- Background ticker checks every 5 minutes (`RefreshInterval`)
- Refreshes proactively within 30 minutes of expiry (`RefreshWindow`)
- On 401, reactive refresh via `RefreshIfNeeded()` in client.go
- Token currently expires at 2026-02-27T17:33:58Z
- Log shows NO proactive refresh firing — ever. Only startup reads.
- 401 at 10:13 today, with refresh URL = `https://console.anthropic.com/api/oauth/token`

## What to investigate

1. **Is the background ticker actually running?** Check that `Start()` is called in main.go. Look for any error paths that prevent it.

2. **Is `maybeRefresh()` being called?** Add or check for logging in the ticker loop to confirm it fires.

3. **Does the refresh endpoint actually work?** The refresh URL is `https://console.anthropic.com/api/oauth/token` with `grant_type=refresh_token`. Test this manually or add better error logging. The 401s suggest the refresh token itself may be expiring or the endpoint rejecting our requests.

4. **Is the credentials file being written back correctly?** After refresh, `writeCredentials()` does a read-modify-write with flock. Check this works when CC is also potentially writing to the same file.

5. **Race condition with CC?** Both foci and CC write to the same credentials file. CC might overwrite foci's refreshed token with a stale one, or vice versa. The flock should prevent corruption but not logical races.

## Files

- `anthropic/oauth.go` — OAuthManager, refresh logic
- `anthropic/oauth_test.go` — existing tests
- `anthropic/client.go` — 401 handling, `refreshFunc`
- `main.go` — OAuthManager creation and Start()

## Expected outcome

Foci should be able to keep its own OAuth token alive indefinitely without CC running. The proactive refresh should demonstrably fire in the logs when approaching token expiry.

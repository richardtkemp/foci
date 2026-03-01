# TODO #130: Multiball Routing Bug Fix

## Root Cause

In `main.go` `sessionNotifyFn` (line ~1493), when delivering a response to a multiball session's Telegram chat via `reply_to="session"`, it always uses `p.botMgr.PrimaryBot(targetAgentID)`. This means responses meant for a multiball bot's chat get sent through the primary bot instead.

Example: A spawn with `reply_to="session"` targeting `agent:clutch:multiball:mb-123456` sends the response through `@clutch_bot` instead of `@clutchling_bot`.

## Fix

In `sessionNotifyFn`, detect multiball session keys and route through the correct bot:

```go
// After extracting targetAgentID from session key parts...

// Find the right bot for delivery
var deliveryBot *telegram.Bot
if len(parts) >= 3 && parts[2] == "multiball" {
    // Multiball session — find the bot that owns this session key
    deliveryBot = p.botMgr.BotForSession(targetSessionKey)
}
if deliveryBot == nil {
    deliveryBot = p.botMgr.PrimaryBot(targetAgentID)
}
```

`BotForSession()` already exists in `telegram/manager.go` and is already used by `send_telegram` (line ~1448). Just apply the same pattern to the two broken call sites.

## Affected Call Sites (all in main.go)

1. **`sessionNotifyFn`** (line ~1489) — `reply_to="session"` delivery. Uses `PrimaryBot()`. ← **This is the reported bug.**
2. **`async_notify`** (line ~1337) — spawn results, tmux watch callbacks. Uses `PrimaryBot()`. ← **Same bug, different path.**
3. ~~`send_telegram`~~ — **Already fixed** (line ~1448), uses `BotForSession()` with `PrimaryBot()` fallback.

## Files to Change
- `main.go` — `sessionNotifyFn` 
- `telegram/manager.go` — add `BotForSession()` method
- Tests for both

## Severity
High — release blocker. This causes visible user confusion when multiball sessions send messages to the wrong Telegram chat.

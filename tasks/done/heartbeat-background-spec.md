# Spec: Heartbeat & Background Work

## Overview

Split the current monolithic heartbeat into two distinct mechanisms:

1. **Heartbeat** — cache keepalive ping, nothing more
2. **Background work** — mana-gated task execution from background todo queue (includes email, calendar, and other periodic checks)

Both run as branch sessions from main. Both use "time since cache last warmed" triggers, not fixed crontab intervals.

## Config

```toml
[heartbeat]
enabled = true
interval = "55m"                    # time since cache last warmed
prompt = "prompts/heartbeat.md"     # file path to prompt

[background]
enabled = true
interval = "5m"                     # time since last interaction OR last background task finished
prompt = "prompts/background.md"    # file path to prompt
invest_interval = "30m"             # after mana reset, don't spend — let cache invest
```

### Validation

- Warn if `background.interval` > `heartbeat.interval` (heartbeat resets the cache timer — if it fires before background interval elapses, background work will never trigger)
- Warn if `heartbeat.interval` > 1h (Anthropic cache TTL is 1 hour; longer interval means cache expires between heartbeats)

## Cache Warmth Tracking

Both timers care about the same thing: **when was the main session's cache prefix last warmed?**

Any of these warm the cache:
- API call on main session (user message → assistant response)
- Heartbeat branch started (first API call shares main's prefix)
- Background branch started (first API call shares main's prefix)

Track as `lastCacheWarmed time.Time`, reset on any of the above.

## Trigger Logic

### Heartbeat

```
on tick (every ~30s):
  if not heartbeat.enabled: skip
  if time_since(lastCacheWarmed) < heartbeat.interval: skip
  if heartbeat_branch_running: skip
  → create branch from main session with heartbeat.prompt
  → update lastCacheWarmed
  → branch options: no_compact, oneshot, silent
```

### Background Work

```
on tick (every ~30s):
  if not background.enabled: skip
  if time_since(last_interaction) < background.interval: skip
  if background_branch_running: skip
  if no todo items tagged "background": skip
  if not mana_is_good(): skip        # manamometer check
  → create branch from main session with background.prompt
  → update lastCacheWarmed
  → branch options: no_compact=false (may need compaction for long tasks)
```

**Continuous re-evaluation:** Once `background.interval` has elapsed, the mana check runs every tick (~30s). If mana is bad at the interval boundary but improves on the next tick, background work fires immediately. We do not wait another full interval.

"Last interaction" = last of:
- Last inbound user message on main
- Last background branch completion (success or failure)

This means background work self-chains: finish task → wait interval → check mana → dispatch next task.

## Manamometer

Mana is a 5-hour budget that resets to 100% every 5 hours. The manamometer decides whether spending mana on background work is wise.

### Data available

The usage API (`anthropic/usage.go`) already provides:
- `FiveHour.Utilization` — current usage as percentage (mana = 100 - utilization)
- `FiveHour.ResetsAt` — ISO timestamp of next reset

### Linear interpolation

```
now = current time
resets_at = next reset time (from usage API)
window = 5 hours
time_since_reset = window - (resets_at - now)

if time_since_reset < invest_interval:
    → mana_is_good = false (investing period — building cache, don't spend)

// Linear from 100% at invest_interval to 0% at reset
expected_mana = 100% × (window - time_since_reset) / (window - invest_interval)

mana_is_good = (actual_mana > expected_mana)
```

### Intuition

```
100% ┤
     │╲  invest_interval (30m)
     │ ╲  ← no work here, building cache
     │  ╲
     │   ╲.............. expected mana line
     │    ╲
     │     ╲
     │      ╲
     │       ╲
  0% ┤────────╲──────────────────────
     0       30m                    5h
         time since reset →

If actual mana is ABOVE the line → in credit → do work
If actual mana is BELOW the line → in debt → conserve
```

### Edge case: 2 minutes to reset, 5% mana

```
time_since_reset = 4h58m
expected_mana = 100% × (5h - 4h58m) / (5h - 30m) = 100% × 2m / 270m = 0.7%
actual_mana = 5%
5% > 0.7% → in credit → fire background work ✅
```

This is correct — near the end of the window, even tiny mana is "in credit" because you're about to reset anyway.

## Branch Behavior

### Heartbeat Branch

- **Prompt:** Minimal. Just enough to trigger an API call and keep the cache warm.
- **Flags:** `no_compact`, `oneshot`, `silent`
- **Purpose:** Cache keepalive only. No real work.

### Background Branch

- **Prompt:** Configurable. Tells the agent to check background todos and work on one.
- **Expected behavior:** Agent reads todo list, picks highest priority background item, does the work, marks it complete.
- **Flags:** `no_compact` initially, but may need compaction for longer tasks. Not `silent` — agent may need to report results.
- **Cost:** Variable. Manamometer prevents overspend.

## Todo Tool Changes

Add tag support to the existing todo tool:

```
todo add "Run errcheck on foci codebase" --priority medium --tag background
todo list --tag background
todo list --tag background --status open
```

### Schema change

Current: `id, text, priority, status, created_at, completed_at`
New: `id, text, priority, status, tags, created_at, completed_at`

`tags` = comma-separated string or JSON array. Start simple with comma-separated.

### Tag filtering

- `todo list --tag background` — show only items tagged "background"
- `todo list` — shows all items (unchanged)
- Background work trigger checks: `SELECT COUNT(*) FROM todos WHERE status='open' AND tags LIKE '%background%'`

## Timer Implementation

Single goroutine with a ~30s tick:

```go
func (g *Gateway) runTimers() {
    ticker := time.NewTicker(30 * time.Second)
    for {
        select {
        case <-ticker.C:
            g.maybeHeartbeat()
            g.maybeBackgroundWork()
        case <-g.stop:
            return
        }
    }
}
```

Both `maybeHeartbeat()` and `maybeBackgroundWork()` are non-blocking — they check conditions and dispatch branches if appropriate. The branch runs asynchronously.

### State tracking

```go
type timerState struct {
    lastCacheWarmed     time.Time  // reset on any branch start or main session API call
    lastInteraction     time.Time  // reset on user message or background branch completion
    heartbeatRunning    bool       // prevent concurrent heartbeats
    backgroundRunning   bool       // prevent concurrent background work
}
```

### Mana data

The manamometer needs current mana % and reset time. Options:
1. **Poll usage API** — call on each tick that passes the interval check (not every tick). Cheap HTTP call to Anthropic's usage endpoint.
2. **Cache from last API response** — if the main API response headers include rate limit info, cache it. Stale but free.
3. **Read from last known state** — the `/mana` command already queries this. Store the result.

Recommend option 1 for accuracy, with a rate limit (max once per 60s).

## Migration from Current Heartbeats

Current heartbeats are crontab-driven wake messages with a full agent prompt (check email, calendar, weather, etc.). Migration:

1. Remove heartbeat crontab entries
2. Add `[heartbeat]` and `[background]` config sections
3. Current heartbeat prompt content → move to background tasks:
   - Email/calendar/weather checks → background todo items (tagged "background")
   - Cache keepalive → heartbeat (new, cheap)
4. `HEARTBEAT.md` and `heartbeat-state.json` — keep for now, background work prompt can reference them

## What This Replaces

| Current | New |
|---------|-----|
| Crontab `*/7 * * * *` wake | Timer-based, adaptive |
| Full agent turn every heartbeat | Cheap keepalive + mana-gated work |
| Always runs regardless of mana | Manamometer gates background work |
| Fires during active conversation | Self-suppresses during activity |
| Email/calendar in heartbeat | Email/calendar as background tasks |
| Single prompt does everything | Separated concerns |

## Out of Scope

- Manamometer as a standalone tool/command (can be added later on top of the core logic)
- Multiple concurrent background branches (one at a time for now)

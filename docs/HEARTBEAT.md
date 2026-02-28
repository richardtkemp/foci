# Keepalive & Background Work

Two timer-driven mechanisms keep the agent productive without wasting mana.

## Overview

| Mechanism | Purpose | Trigger | Cost |
|-----------|---------|---------|------|
| **Keepalive** | Cache keepalive | Cache not warmed within interval | Minimal (1 API call) |
| **Background work** | Task execution | User idle + open tasks + mana available | Variable (full agent turn) |

Both run on a single goroutine per agent with ~30-second ticks. Neither fires during active conversation.

## Keepalive

The keepalive keeps the Anthropic cache prefix warm. Anthropic's cache TTL is ~1 hour, so the default interval is 55 minutes.

When the keepalive fires, it creates a branch session from the agent's default session. The branch shares the parent's cache prefix, so the API call warms the cache for the next real interaction. The branch runs with `no_compact` (returns immediately if context limit is hit) and does no real work.

**When it fires:**
```
if keepalive.enabled
   AND time_since(last_cache_warm) >= keepalive.interval
   AND no keepalive already running
```

**What warms the cache:**
- Any API call on the main session (user message -> response)
- Keepalive branch starting
- Background branch starting

## Background Work

Background work executes tasks from the todo list while the user is away, gated by the manamometer to prevent overspending.

**When it fires:**
```
if background.enabled
   AND time_since(last_interaction) >= background.interval
   AND no background work already running
   AND open todos tagged "background" exist
   AND manamometer says mana is good
```

**Self-chaining:** When a background task completes, it resets `last_interaction` to the completion time. After the interval elapses again, the next task can fire. This creates a chain: finish task -> wait interval -> check mana -> dispatch next task.

**Interaction** = last of:
- Last inbound user message (via Telegram)
- Last background branch completion

## Manamometer

The manamometer decides whether spending mana on background work is wise. It uses linear interpolation over the 5-hour budget window.

### How it works

Mana is a 5-hour budget that resets to 100% every 5 hours. The manamometer compares actual mana against an expected mana line:

```
100% +
     |\.  invest_interval (30m)
     | \   <- no work here, building cache
     |  \
     |   \.............. expected mana line
     |    \
     |     \
     |      \
  0% +------\--------------------------
     0      30m                        5h
         time since reset ->

Above the line = in credit = do work
Below the line = in debt  = conserve
```

### Algorithm

```
time_since_reset = 5h - (resets_at - now)

if time_since_reset < invest_interval:
    return false  // investing period -- building cache, don't spend

expected_mana = 100% * (5h - time_since_reset) / (5h - invest_interval)

return actual_mana > expected_mana
```

### Edge cases

**2 minutes to reset, 5% mana:**
```
time_since_reset = 4h58m
expected_mana = 100% * 2m / 270m = 0.74%
5% > 0.74% -> in credit -> fire background work
```
Correct: near the end of the window, even tiny mana is "in credit" because the budget resets soon.

**Just after reset, 95% mana:**
```
time_since_reset = 10m (within 30m invest_interval)
-> investing period -> no work
```
Correct: right after reset, we want to let the cache build up before spending.

**Continuous re-evaluation:** Once `background.interval` has elapsed, the mana check runs every tick (~30s). If mana is bad at the interval boundary but improves on the next tick, background work fires immediately. We don't wait another full interval.

### Usage API

The manamometer calls the Anthropic usage API (`/api/oauth/usage`) to get current mana and reset time. Calls are rate-limited to at most once per 60 seconds.

## Todo Tags

Background work uses the todo system's tag feature. Add tags when creating todos:

```
todo add "Check email" --tag background
todo add "Run linter" --tag background,daily
todo list --tag background
todo list --tag background --status open
```

Tags are stored as comma-separated strings. Filtering uses whole-word matching to prevent partial matches (e.g., "back" won't match "background").

The background work trigger checks: `SELECT COUNT(*) FROM todos WHERE agent_id = ? AND status = 'open' AND tags LIKE '%background%'`

## Config

```toml
[keepalive]
enabled = true
interval = "55m"                    # time since cache last warmed
prompt = "prompts/keepalive.md"     # path to prompt file

[background]
enabled = true
interval = "5m"                     # time since last interaction
prompt = "prompts/background.md"    # path to prompt file
invest_interval = "30m"             # quiet period after mana reset
```

### Validation

- **Warning:** `background.interval > keepalive.interval` -- keepalive resets the cache timer before background interval elapses, so background work may never trigger.
- **Warning:** `keepalive.interval > 1h` -- Anthropic cache TTL is ~1 hour; longer interval means cache expires between keepalives, defeating the purpose.

## Branch Behavior

### Keepalive branches

- **Prompt:** Minimal cache keepalive.
- **Flags:** `no_compact`, `no_reset_hook`
- **Trigger context:** `"keepalive"`
- **Telegram delivery:** None (silent).

### Background branches

- **Prompt:** Tells the agent to check background todos and work on one.
- **Flags:** `no_reset_hook` (may need compaction for longer tasks)
- **Trigger context:** `"background"`
- **Telegram delivery:** None (silent).
- **Cost:** Variable. Manamometer prevents overspend.

## Shutdown

Keepalive runners are stopped first during graceful shutdown, before the HTTP server is closed. This prevents new timer-triggered branches from starting while in-flight agent turns complete.

## Package

The implementation lives in `keepalive/keepalive.go`:

- `Runner` — manages timer state and tick loop
- `ManaIsGood()` — exported manamometer function (used in tests)
- `BuildBranchFunc()` — creates the bridge between keepalive package and main's agent/session infrastructure
- `RunnerConfig` — dependency injection struct

Tests in `keepalive/keepalive_test.go` cover manamometer edge cases (invest period, mid-window, near-reset, past-reset, zero data).

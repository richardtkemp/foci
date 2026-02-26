# Spec: Heartbeat & Background Work

## Overview

Split the current monolithic heartbeat into two distinct mechanisms:

1. **Heartbeat** — cache keepalive ping, nothing more
2. **Background work** — useful task execution from background todo queue (includes email, calendar, and other periodic checks)

Both run as branch sessions from main. Both use "time since last activity" triggers, not fixed crontab intervals.

## Config

```toml
[heartbeat]
enabled = true
interval = "55m"             # time since last session activity
prompt = "prompts/heartbeat.md"  # file path to prompt

[background]
enabled = true
interval = "5m"              # time since last interaction OR last background task finished
prompt = "prompts/background.md"  # file path to prompt
```

### Validation

- Warn if `background.interval` > `heartbeat.interval` (heartbeat resets the "time since activity" timer — if it fires before the background interval elapses, background work will never trigger)
- Warn if `heartbeat.interval` > 1h (Anthropic cache TTL is 1 hour; longer interval means cache expires between heartbeats)

## Trigger Logic

### Heartbeat

```
on tick (every ~30s):
  if not heartbeat.enabled: skip
  if time_since(last_activity_on_main) < heartbeat.interval: skip
  if heartbeat_branch_running: skip
  → create branch from main session with heartbeat.prompt
  → branch options: no_compact, oneshot, silent
```

"Last activity on main" = last inbound user message OR last assistant response on the main session. Any API call on main resets the timer.

### Background Work

```
on tick (every ~30s):
  if not background.enabled: skip
  if time_since(last_interaction) < background.interval: skip
  if background_branch_running: skip
  if no todo items tagged "background": skip
  → create branch from main session with background.prompt
  → branch options: no_compact=false (may need compaction for long tasks)
```

"Last interaction" = last of:
- Last inbound user message on main
- Last background branch completion (success or failure)

This means background work self-chains: finish task → wait interval → check conditions → dispatch next task. Natural throttling via the interval + mana check.

## Branch Behavior

### Heartbeat Branch

- **Prompt:** Minimal. Just enough to trigger an API call and keep the cache warm.
- **Flags:** `no_compact`, `oneshot`, `silent`
- **Purpose:** Cache keepalive only. No real work.

### Background Branch

- **Prompt:** Configurable. Tells the agent to check background todos and work on one.
- **Expected behavior:** Agent reads todo list, picks highest priority background item, does the work, marks it complete.
- **Flags:** `no_compact` initially, but may need compaction for longer tasks. Not `silent` — agent may need to report results.
- **Cost:** Variable. Mana floor prevents overspend.

## Todo Tool Changes

Add tag support to the existing todo tool:

```
todo add "Run errcheck on clod codebase" --priority medium --tag background
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

## Mana Assessment

No Go-level mana gating. The agent makes the call — it has `/mana`, the mana skill, and can reason about the sliding window. "5% left but 4h50m of old calls about to drop off" = go ahead. A static floor would prevent exactly this kind of smart decision.

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
    lastMainActivity    time.Time  // reset on any main session API call
    lastBackgroundDone  time.Time  // reset on background branch completion
    heartbeatRunning    bool       // prevent concurrent heartbeats
    backgroundRunning   bool       // prevent concurrent background work
}
```

## Migration from Current Heartbeats

Current heartbeats are crontab-driven wake messages with a full agent prompt (check email, calendar, weather, etc.). Migration:

1. Remove heartbeat crontab entries
2. Add `[heartbeat]` and `[background]` config sections
3. Current heartbeat prompt content → can go in the background prompt or be split:
   - Email/calendar/weather checks → background tasks (tagged "background")
   - Cache keepalive → heartbeat (new, cheap)
4. `HEARTBEAT.md` and `heartbeat-state.json` — keep for now, background work prompt can reference them

## What This Replaces

| Current | New |
|---------|-----|
| Crontab `*/7 * * * *` wake | Timer-based, adaptive |
| Full agent turn every heartbeat | Cheap keepalive + gated work |
| Always runs regardless of mana | Agent decides based on mana |
| Fires during active conversation | Self-suppresses during activity |
| Email/calendar in heartbeat | Email/calendar as background tasks |
| Single prompt does everything | Separated concerns |

## Out of Scope

- Manamometer as a standalone tool/command (can be added later)
- Recovery estimation (agent handles this in background prompt)
- Task cost estimation (agent decides in background prompt)
- Multiple concurrent background branches (one at a time for now)

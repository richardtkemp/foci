# Keepalive & Background Work

Two timer-driven mechanisms keep the agent productive without wasting effort.

## Overview

| Mechanism | Purpose | Trigger | Cost |
|-----------|---------|---------|------|
| **Keepalive** | Cache keepalive | Cache not warmed within interval | Minimal (1 API call) |
| **Background work** | Task execution | User idle + open tasks + `can_run_background` allows | Variable (full agent turn) |

Both run on a single goroutine per agent with ~30-second ticks. Neither fires during active conversation.

## Keepalive

The keepalive keeps the prompt cache warm. For Anthropic, the cache TTL is ~1 hour and the default interval is 55 minutes. For OpenAI and DeepSeek models, keepalive is auto-detected by developer name — these models have a 5-minute prompt cache TTL, so keepalive fires every ~4m45s (95% of TTL).

When the keepalive fires, it creates a branch session from the agent's default session. The branch shares the parent's cache prefix, so the API call warms the cache for the next real interaction. The branch runs with `no_compact` (returns immediately if context limit is hit) and does no real work.

**Which session is warmed.** Keepalive is per-agent (one scheduler per agent) and warms exactly **one** session — the agent's resolved default, via `route.Resolver` → `SessionIndex.DefaultSessionKeyForAgentOn`. First match wins:

1. `default_platform`'s `is_default` chat
2. `default_platform`'s most-recently-active registered chat
3. any `is_default` chat (ordered by activity)
4. the most-recently-active **root** session

Every candidate is filtered to `is_root = 1`, so branches, children, facets, and inactive sessions are never targeted. An agent active across many chats therefore only keeps **one** chat's cache warm (the default / most-active); other idle chats' caches expire naturally on the provider's TTL.

**Warming the app's open chats.** Set `[keepalive] warm_open_app_chats = true` (default false, global or per-agent) to instead warm **every chat the Android app currently has open** (its pager tabs). The app reports its open-set to the server — an `open` flag on each `hello` resume point, plus a `conversation.openSet` frame on change — and keepalive fires one warming branch per open session (deduped, in-flight-filtered). When no chats are open it falls back to the default session. Cost scales with the number of open chats (N warming calls per interval), so it is opt-in.

**When it fires:**
```
if keepalive.enabled
   AND caching available (Anthropic model, or per-model cachingOverride)
   AND time_since(last_cache_warm) >= keepalive.interval
   AND no keepalive already running
   AND a default session exists
   AND no turn is in flight on that session
```

**What warms the cache:**
- Any API call on the main session (user message -> response)
- Keepalive branch starting
- Background branch starting

## Background Work

Background work executes tasks from the todo list while the user is away, gated by the `can_run_background` check so an operator can decide when it's worth spending.

**When it fires:**
```
if background.enabled
   AND time_since(last_interaction) >= background.interval
   AND no background work already running
   AND open todos tagged "background" exist
   AND can_run_background allows (exit 0, or unset)
```

**Self-chaining:** When a background task completes, it resets `last_interaction` to the completion time. After the interval elapses again, the next task can fire. This creates a chain: finish task -> wait interval -> check `can_run_background` -> dispatch next task.

**Interaction** = last of:
- Last inbound user message (via Telegram)
- Last background branch completion

## The `can_run_background` gate

Whether background work should fire is delegated to a user-provided executable, configured as `can_run_background` under `[background]` (global) and `[agents.background]` (per-agent). This lets an operator plug in their own affordability policy — quota checks, time-of-day windows, load checks, whatever.

### How it works

Before each background operation, foci runs the configured executable:

- **Exit 0** → background work is allowed this tick.
- **Any non-zero exit** → skip this tick.
- **Empty/unset** → always allowed.
- **Fails to execute** (missing/not executable) → treated as allowed and logged as a warning, so a broken script never wedges all background work.

The script runs via `procx.Spawn` (the `foci-secrets` group is stripped) with a 10-second timeout, and receives environment variables:

- `FOCI_SESSION_KEY`
- `FOCI_AGENT_ID`
- `FOCI_ENDPOINT`

The real-429 `RateLimitGate` (queue + replay on an API rate-limit response) still gates background work first and is independent of `can_run_background`.

**Continuous re-evaluation:** Once `background.interval` has elapsed, the `can_run_background` check runs every tick (~30s). If it says no at the interval boundary but yes on the next tick, background work fires immediately. We don't wait another full interval.

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
can_run_background = "check.sh"     # optional gate executable (exit 0 = allowed)
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
- **Cost:** Variable. The `can_run_background` gate can prevent overspend.

### Gating against outstanding background work (#1068/#1070, spec §4)

Reflection, keepalive, and memory passes are **system injects**: for a delegated (CC) agent they route through `EnqueueInjectWait` into the main session's inbox worker, which holds them until no delivering work is active *or pending*. "Pending" covers the whole background-work window — from the moment a turn backgrounds a subagent or `run_in_background` Bash until the resulting autonomous run completes — reported by the backend's `AwaitingAutonomousRun()`. Running an inject during that window would rebind the shared session sink and swallow the autonomous run's output, so the worker waits.

Consequence for the schedulers: these fires use the process-lifetime context, not a per-fire timeout, so a fire can block for as long as background work is outstanding. This does **not** wedge the runner — each branch type has its own in-flight flag (`keepaliveRunning`, `reflectionRunning`, …) that simply skips re-firing that type while one is parked, and the other timers keep ticking. The worst case (a completion notification that never arrives) is bounded by the tracker's max-age prune (`[cc_backend].background_task_max_age`, default 30m): once the stale entry is pruned the gate opens and the held inject runs. A manual `/compact` is a user-initiated slash command, not a system inject, so it is never held by this gate.

## Shutdown

Keepalive runners are stopped first during graceful shutdown, before the HTTP server is closed. This prevents new timer-triggered branches from starting while in-flight agent turns complete.

## Package

The implementation lives in `periodic/keepalive.go`:

- `Runner` — manages timer state and tick loop
- `BranchFunc` — callback type; receives branch type, parent session key, prompt, and returns success bool
- `RunnerConfig` — dependency injection struct

`buildBranchFunc()` in `cmd/foci-gw/agent_sessions.go` creates the bridge between the periodic package and main's agent/session infrastructure.

Tests in `keepalive/keepalive_test.go` cover the tick loop and gating behaviour.

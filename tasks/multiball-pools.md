# Task: Multi-bot Multiball Pools (Per-Agent + Shared)

## Summary

Change multiball config from a single bot per agent to multiple bots, with both per-agent and shared pools. Per-agent pool is preferred; shared pool is the fallback.

## Current State

- `[agent.X]` has `multiball_bot = "name"` (singular string → one bot)
- `[telegram]` has legacy `secondary_bots = [tokens...]` (raw tokens, not bot names)
- The `Pool` type already supports multiple bots — it's the config and wiring that's singular
- `BotManager` has per-agent pools (`map[string]*Pool`) but no shared pool concept

## Required Changes

### 1. Config (`config/config.go`)

**Agent config:**
```toml
[agent.clutch]
multiball_bots = ["clutchling", "clutchling2"]  # per-agent pool
```

- Rename `multiball_bot` (string) → `multiball_bots` ([]string)
- Keep `multiball_bot` as a deprecated alias: if present and `multiball_bots` is empty, treat it as a single-element list. Log a deprecation warning.

**Shared config:**
```toml
[telegram]
multiball_bots = ["spare1", "spare2"]  # shared pool, fallback for any agent
```

- Add `multiball_bots` field to `[telegram]` section ([]string, references keys in `[telegram.bots]`)
- Remove legacy `secondary_bots` field (raw tokens) — it's unused in production

### 2. BotManager (`telegram/manager.go`)

- Add a shared pool: `shared *Pool` field alongside the per-agent `pools` map
- Add `AddSharedMultiball(bot *Bot)` method
- Add `SharedPool() *Pool` method

### 3. Pool Acquisition Logic

When `/multiball` is invoked for an agent:
1. Try `pools[agentID].Acquire()` (per-agent pool)
2. If that returns false (all busy or no per-agent pool), try `shared.Acquire()`
3. If both fail, return "no bots available"

This logic should live in `BotManager` as a new method, e.g.:
```go
func (m *BotManager) AcquireMultiball(agentID string) (*Bot, bool)
```

The caller (command handler / fork logic in main.go) should use this instead of directly accessing the pool.

### 4. Pool Release

When a bot is released (TTL reclaim, `/done`, `/reset`), return it to whichever pool it came from. The bot already knows its pool via `bot.SetSecondary(pool)` — `pool.Release(bot)` handles this. No change needed here, but verify this works correctly for shared pool bots.

### 5. Wiring (`main.go`)

In the agent setup loop (~line 1456):
- Iterate `acfg.MultiballBots` (plural), resolve each bot name, create Bot, call `AddMultiball(agentID, bot)`
- Handle deprecated `multiball_bot` → single-element list with warning
- After the agent loop, iterate `cfg.Telegram.MultiballBots`, resolve each, call `AddSharedMultiball(bot)`
- Remove the legacy `secondary_bots` code path entirely

Configure session TTL on the shared pool the same way as per-agent pools.

### 6. Update SPEC.md and docs/CONFIG.md

- Document the new config fields
- Document the acquisition priority (per-agent first, shared fallback)
- Remove references to legacy `secondary_bots`
- Remove `multiball_bot` (singular) from examples, show `multiball_bots` (plural)

## Test Cases

- Agent with per-agent bots only → uses per-agent pool
- Agent with no per-agent bots, shared bots exist → uses shared pool
- Agent with per-agent bots all busy, shared bots available → falls back to shared
- Both pools exhausted → "no bots available"
- Bot released → returns to correct pool (per-agent or shared)
- TTL reclaim works on both pools
- Deprecated `multiball_bot` (singular) still works with warning
- Config validation: bot names in `multiball_bots` must exist in `[telegram.bots]`

## Files to Touch

- `config/config.go` + `config/config_test.go`
- `telegram/manager.go` + `telegram/manager_test.go`
- `telegram/pool.go` + `telegram/pool_test.go` (if needed)
- `main.go` (wiring)
- `command/builtins.go` (if acquisition call changes)
- `SPEC.md`, `docs/CONFIG.md`

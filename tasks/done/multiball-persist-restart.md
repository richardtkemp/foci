# Task: Multiball Sessions Surviving Process Restart (#59)

## Problem

When clod restarts (deploy, crash, manual restart), multiball/forked sessions are lost. The session data is persisted on disk (JSONL files), but the in-memory mapping between secondary bots and their session keys is not restored. After restart, secondary bots come up with no session key — they're idle in the pool — and any active forked conversations are orphaned.

## Current Behaviour

1. User runs `/multiball` → bot acquired from pool → `bot.SetSessionKey(branchKey)` → fork works
2. Process restarts → all bots start fresh → `sessionKey = ""` → bot goes back to idle pool
3. User messages the secondary bot → it has no session key → message routes nowhere useful
4. The JSONL session file still exists on disk but nothing references it

## Desired Behaviour

1. On startup, after bots are created and pools configured, restore the session key mapping
2. Secondary bots that had active forked sessions before restart should resume with their session key
3. User messages the secondary bot after restart → conversation continues where it left off
4. Session TTL reclaim should still work (stale sessions get cleaned up as normal)

## Design Considerations

The mapping to persist is: `bot_name → session_key` (e.g. `"clutchling" → "agent:clutch:multiball:mb-1709123456"`).

Options for where to store it:

**Option A: Derive from session files on disk.** Scan session JSONL files matching the multiball pattern (`agent:*:multiball:*`), check which ones are still active (have recent activity), and match them to bots. Problem: no reliable way to map a session back to a specific bot token/name without storing that association.

**Option B: Persist the mapping explicitly.** Store the bot-name→session-key mapping in a small state file (JSON) or in the existing data store. On startup, read it back and call `bot.SetSessionKey()` for each match. This is the cleaner approach.

I'd suggest Option B. The state file could live at `data/multiball-state.json` or use the existing key-value store.

## Implementation Notes

- The mapping should be updated whenever `SetSessionKey` is called (fork or release)
- On startup, after `AddMultiball` wiring, load the state and restore keys
- If a bot name in the state no longer exists in config, skip it (log a warning)
- If a session key in the state has no JSONL file on disk, skip it (session was cleaned up)
- Shared pool bots need the same treatment as per-agent pool bots

## Files Likely Involved

- `telegram/bot.go` — `SetSessionKey` needs to trigger persistence
- `telegram/pool.go` or `telegram/manager.go` — state save/load logic
- `main.go` — restore mappings during startup after bot wiring
- Tests for save/load/restore cycle

## Update docs

- Update SPEC.md and docs/CONFIG.md if any new config is needed
- Write/update tests
- Commit with descriptive message and push

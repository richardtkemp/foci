# Default Prompts & Memory Formation Integration

## Overview

Move four prompts from external files into the repo as embedded defaults, and integrate memory formation jobs into foci's config/code instead of relying on external cron.

## 1. Embedded Default Prompts

### Files to add to `prompts/` in the repo

| File | Purpose |
|------|---------|
| `memory-formation.md` | Capture notable events to daily memory files (used by both interval and session-end jobs) |
| `memory-consolidation.md` | Periodic curation of MEMORY.md from recent daily files |
| `compaction-summary.md` | Conversation compression on compaction |
| `keepalive.md` | Cache keepalive ping — instructs agent to reply with empty string (replaces hardcoded fallback) |
| `background.md` | Background work prompt (replaces hardcoded fallback string) |

All four are embedded via `//go:embed` in `prompts/prompts.go`, same as the branch orientation prompts.

### Config behaviour (all four)

| Config state | Behaviour |
|-------------|-----------|
| Key absent from TOML | Use embedded default |
| Key = `"default"` | Explicitly use embedded default (useful to override a global custom path back to default at agent level) |
| Key = `"/path/to/file.md"` | Use that file; if file missing, log error, use embedded default |
| Key = `""` (empty string) | Prompt not injected (explicitly disabled) |

### First-run file seeding

On startup, if `~/shared/prompts/` doesn't contain a copy of the default prompt files, write them out. This gives users a starting point to customise. Never overwrite existing files — only write if the file doesn't exist.

## 2. Memory Formation Jobs

Replace external cron-based memory formation with built-in scheduled jobs, configured alongside keepalive/background.

### New config section: `[agents.memory_formation]`

```toml
[[agents]]
id = "clutch"

[agents.memory_formation]
# Periodic memory formation — captures notable events to daily files
interval_enabled = true                  # default: true
interval = "1h"                          # default: "1h", fixed schedule (not relative to activity)
interval_prompt = ""                     # path override; absent = embedded default; "" = disabled

# Memory consolidation — curates MEMORY.md from recent daily files
consolidation_enabled = true              # default: true
consolidation_interval = "20h"            # default: "20h", minimum time since last run
consolidation_prompt = ""                 # path override; absent = embedded default; "" = disabled

# Session-end memory formation — runs on /reset and session reclaim
session_end_enabled = true               # default: true
session_end_prompt = ""                  # path override; absent = embedded default; "" = disabled
```

This replaces:
- The cron entry that fires memory formation prompts
- The `sessions.session_reset_prompt` config key (removed, not aliased)

### Behaviour

**Periodic memory formation (interval):**
- When `interval_enabled = true`, foci runs the prompt on a fixed interval (e.g. every hour on the clock)
- Skips if no user activity since the last run (no messages = nothing to capture)
- The prompt is injected as a user message into a temporary headless branch session
- The branch has full tool access (to read session history and write memory files)
- The prompt itself instructs the agent to do nothing if nothing notable happened

**Memory consolidation:**
- When `consolidation_enabled = true`, triggers when BOTH conditions are met:
  1. At least `consolidation_interval` (default 20h) has elapsed since the last run
  2. There has been user activity within the last 1 hour on the session it will branch from
- Runs as a headless branch with full tool access
- Reviews recent daily memory files and curates MEMORY.md

**Session-end memory formation:**
- Fires on `/reset` and session reclaim (timeout)
- Runs as a headless branch (async — doesn't block the new session starting, addressing TODO #174)
- The old session's conversation is available to the branch

### Config cascade

All fields follow the standard cascade: **agent → global → hardcoded default**.

```toml
# Global (applies to all agents unless overridden)
[memory_formation]
interval_enabled = true
interval = "1h"
# interval_prompt — absent = embedded default
session_end_enabled = true
# session_end_prompt — absent = embedded default

# Per-agent override
[[agents]]
id = "scout"
[agents.memory_formation]
interval = "2h"                          # scout gets a longer interval
session_end_enabled = false              # no session-end memory for scout
```

| Field | Hardcoded default |
|-------|------------------|
| `interval_enabled` | `true` |
| `interval` | `"1h"` |
| `interval_prompt` | embedded `memory-formation.md` (same file as session_end_prompt) |
| `consolidation_enabled` | `true` |
| `consolidation_interval` | `"20h"` (minimum time since last run) |
| `consolidation_prompt` | embedded `memory-consolidation.md` |
| `session_end_enabled` | `true` |
| `session_end_prompt` | embedded `memory-formation.md` (same file as interval_prompt) |

Agent config uses pointer types for all fields so "unset" is distinguishable from "set to zero/false/empty".

## 3. Upgrade Keepalive & Background Prompts

Keepalive and background currently use hardcoded fallback strings in `keepalive.go`. Upgrade to match the full pattern:

1. **Create `prompts/keepalive.md` and `prompts/background.md`** in the repo, embedded via `//go:embed`
2. **Three-state config** for `prompt` field (same as memory formation):
   - Absent → embedded default
   - `"default"` → explicitly use embedded default
   - `"/path/to/file.md"` → use that file; if missing, log error, use embedded default
   - `""` → disabled (use a minimal fallback, since keepalive/background need *something* to send)
3. **Seed to `~/shared/prompts/`** on first run alongside the memory formation prompts
4. **Remove hardcoded fallback strings** from `keepalive.go` — use the embedded prompts instead

The existing config keys (`keepalive.prompt`, `background.prompt`) stay the same — just the resolution logic changes.

## 4. Branch Session Cleanup

Multiball branches currently have no memory formation when they end — whatever wasn't explicitly written during the session is lost.

**Change:** When a multiball branch is cleaned up (idle timeout, `/reset` on main, or explicit kill), inject the session-end memory formation prompt directly into the expiring session before clearing it. No need to branch from a branch — just send the prompt as a final turn in that session.

Headless branches (spawn, cron) should NOT get the reset hook — they're short-lived task runners with `NoResetHook` set.

## 5. Migration

- `sessions.session_reset_prompt` is removed (replaced by `agents.memory_formation.session_end_prompt`)
- `sessions.compaction_summary_prompt` stays where it is (compaction is not memory formation, it's a session mechanism)
- External cron entries for memory formation can be removed after deploy

## 6. Documentation: docs/DEFAULTS.md

New doc describing what foci sets up by default out of the box. Should cover:

- **Embedded prompts** — what ships in the binary, where they're seeded on first run
- **Memory formation** — periodic interval capture (default off, configurable), session-end capture (default on), multiball branch cleanup capture
- **Compaction** — default compaction summary prompt
- **Config cascade** — how agent → global → hardcoded defaults work for all prompt/memory settings
- **Customisation** — how to override, disable, or restore defaults (including the `"default"` special value)
- **File seeding** — `~/shared/prompts/` populated on first run, never overwritten

Written for someone setting up foci for the first time — what do they get without configuring anything, and how do they change it.

## 7. Summary of changes

| Component | Change |
|-----------|--------|
| `prompts/` | Add 4 .md files, embed in prompts.go |
| `config/` | Add `MemoryFormation` struct, parse, cascade, display |
| `main.go` | Wire memory formation scheduling |
| Agent loop | New goroutine for periodic memory formation (fixed interval, skip if no activity) |
| `/reset` handler | Run session-end memory in async branch (fixes #174 delay) |
| Multiball cleanup | Fire session-end memory before clearing branch sessions |
| Startup | Seed `~/shared/prompts/` with defaults if missing |
| `keepalive/` | Remove hardcoded fallback strings, use embedded prompts |
| Docs | SPEC.md, CONFIG.md, docs/DEFAULTS.md (new) |

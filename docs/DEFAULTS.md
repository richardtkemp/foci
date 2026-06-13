# Embedded Defaults

Foci ships embedded prompt files in the binary. These provide sensible defaults that work out of the box. Users can override any prompt via config or by editing the seeded files.

## Embedded Prompt Files

| File | Accessor | Used by |
|------|----------|---------|
| `shared/prompts/branch-orientation-headless.md` | `BranchOrientationHeadless()` | Headless branch sessions (cron, spawn, keepalive) |
| `shared/prompts/branch-orientation-facet.md` | `BranchOrientationFacet()` | User-attached facet branch sessions |
| `shared/prompts/compaction-summary.md` | `CompactionSummary()` | Compaction summary generation |
| `shared/prompts/compaction-handoff.md` | `CompactionHandoff()` | Post-compaction handoff message |
| `shared/prompts/keepalive.md` | `Keepalive()` | Cache keepalive pings |
| `shared/prompts/background.md` | `Background()` | Background work trigger |
| `shared/prompts/reflection.md` | `Reflection()` | Interval + session-end reflection pass (memory + skill formation) |
| `shared/prompts/memory-consolidation.md` | `MemoryConsolidation()` | MEMORY.md curation |

## First-Run Seeding

On startup, foci seeds editable copies of the 4 new prompt files to `~/shared/prompts/`:

- `keepalive.md`
- `background.md`
- `reflection.md`
- `memory-consolidation.md`

Existing files are never overwritten. This gives users a starting point for customisation without needing to know the embedded content.

## 3-State Prompt Resolution

All prompt config fields use `prompts.ResolvePrompt(path, label, embeddedDefault)`:

| Config value | Behaviour |
|--------------|-----------|
| `""` (empty/absent) | Use embedded default |
| `"default"` | Use embedded default (explicit) |
| `"none"` | Disabled — returns empty string |
| `/path/to/file` | Read file; on error, log warning and fall back to embedded default |

Tilde expansion (`~/...`) is handled automatically.

## Memory Formation Defaults

| Setting | Default | Description |
|---------|---------|-------------|
| `interval_enabled` | `true` (nil) | Periodic memory capture enabled |
| `interval` | `"1h"` | Time between captures |
| `session_end_enabled` | `true` (nil) | Capture on /reset and reclaim |

Memory consolidation and the daily session reset live in `[maintenance]`:

| Setting | Default | Description |
|---------|---------|-------------|
| `consolidation_enabled` | `true` (nil) | MEMORY.md curation enabled |
| `consolidation_time` | `"20h"` | `"HH:MM"` daily or a duration (e.g. `"20h"`) |
| `reset_time` | `""` | Daily soft `/reset`: `"HH:MM"`, a duration, or `""` to disable |
| `reset_idle_guard` | `"55m"` | Skip scheduled reset if user active within this window |

All `*_enabled` fields use `*bool` — `nil` means `true`. Set to explicit `false` to disable.

## Config Cascade

Reflection config follows the same cascade as keepalive/background:

1. **Global defaults** — `[reflection]` section, filled with hardcoded fallbacks
2. **Per-agent zero-check** — if agent's `[agents.reflection]` is entirely empty, copy global
3. **Per-agent partial override** — if some fields set, fill gaps from global
4. **Runtime resolution** — prompt paths resolved via `ResolvePrompt` at fire-time

## How to Override

```toml
# Use a custom prompt file:
[reflection]
interval_prompt = "~/shared/prompts/my-custom-reflection.md"

# Disable interval reflection but keep session-end:
[reflection]
interval_enabled = false

# Disable all reflection for a specific agent:
[[agents]]
id = "minimal-agent"
[agents.reflection]
interval_enabled = false
consolidation_enabled = false
session_end_enabled = false
```

## How to Restore Defaults

Delete the seeded file at `~/shared/prompts/<name>.md`. On next startup, foci will re-seed it from the embedded copy. Alternatively, set the config field to `"default"` to explicitly use the embedded version regardless of any file on disk.

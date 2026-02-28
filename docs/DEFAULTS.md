# Embedded Defaults

Foci ships embedded prompt files in the binary. These provide sensible defaults that work out of the box. Users can override any prompt via config or by editing the seeded files.

## Embedded Prompt Files

| File | Accessor | Used by |
|------|----------|---------|
| `prompts/branch-orientation-headless.md` | `BranchOrientationHeadless()` | Headless branch sessions (cron, spawn, keepalive) |
| `prompts/branch-orientation-multiball.md` | `BranchOrientationMultiball()` | User-attached multiball branch sessions |
| `prompts/compaction-summary.md` | `CompactionSummary()` | Compaction summary generation |
| `prompts/compaction-handoff.md` | `CompactionHandoff()` | Post-compaction handoff message |
| `prompts/keepalive.md` | `Keepalive()` | Cache keepalive pings |
| `prompts/background.md` | `Background()` | Background work trigger |
| `prompts/memory-formation.md` | `MemoryFormation()` | Interval + session-end memory capture |
| `prompts/memory-consolidation.md` | `MemoryConsolidation()` | MEMORY.md curation |

## First-Run Seeding

On startup, foci seeds editable copies of the 4 new prompt files to `~/shared/prompts/`:

- `keepalive.md`
- `background.md`
- `memory-formation.md`
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
| `consolidation_enabled` | `true` (nil) | MEMORY.md curation enabled |
| `consolidation_interval` | `"20h"` | Minimum time between consolidations |
| `session_end_enabled` | `true` (nil) | Capture on /reset and reclaim |

All `*_enabled` fields use `*bool` — `nil` means `true`. Set to explicit `false` to disable.

## Config Cascade

Memory formation config follows the same cascade as keepalive/background:

1. **Global defaults** — `[memory_formation]` section, filled with hardcoded fallbacks
2. **Per-agent zero-check** — if agent's `[agents.memory_formation]` is entirely empty, copy global
3. **Per-agent partial override** — if some fields set, fill gaps from global
4. **Runtime resolution** — prompt paths resolved via `ResolvePrompt` at fire-time

## How to Override

```toml
# Use a custom prompt file:
[memory_formation]
interval_prompt = "~/shared/prompts/my-custom-memory.md"

# Disable interval memory formation but keep session-end:
[memory_formation]
interval_enabled = false

# Disable all memory formation for a specific agent:
[[agents]]
id = "minimal-agent"
[agents.memory_formation]
interval_enabled = false
consolidation_enabled = false
session_end_enabled = false
```

## How to Restore Defaults

Delete the seeded file at `~/shared/prompts/<name>.md`. On next startup, foci will re-seed it from the embedded copy. Alternatively, set the config field to `"default"` to explicitly use the embedded version regardless of any file on disk.

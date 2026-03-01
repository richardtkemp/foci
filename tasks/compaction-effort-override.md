# TODO #216: Compaction-Specific Effort Override

## Problem
Compaction API calls use the agent's default effort level. For Clutch, that's "low" — but compaction quality matters more than chat turns. Low effort compaction may lose important context.

## Solution
Add a `compaction_effort` config field. When set, compaction API calls use this effort instead of the session/agent default.

## Config
```toml
[agents.compaction]
effort = "high"  # override effort for compaction calls only
```

## Changes

### config/config.go
Add `Effort string` field to `CompactionConfig` struct.

### agent/agent.go (or wherever compaction API call is made)
Find where the compaction summary API call is built. Before setting effort on the request, check if `a.CompactionConfig.Effort` (or equivalent) is set — if so, use that instead of `SessionEffort()`.

### config display
Add compaction.effort to config display output.

### Tests
- Test that compaction call uses override effort when configured
- Test that compaction call falls back to session effort when not configured

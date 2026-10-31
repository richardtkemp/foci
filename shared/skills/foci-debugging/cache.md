<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Prompt-cache bust diagnosis

The cached prefix is the system prompt (environment block + character files) + history, per session. Foci caches character files in memory: edits take effect at the next compaction or restart, not mid-session.

A "bust" is a call with `cache_read_tokens = 0` where you expect a hit. Find the last call with a cache read before the first zero, then extract and **diff their system-prompt blocks**.

Common causes: a character/system-prompt edit picked up by `bootstrap.Reload()`, a model switch, a service restart, compaction on another session that shares the prefix.

```bash
# Find the bust boundary
sqlite3 -readonly ~/data/api.db "SELECT c.billed_at, c.cost_usd, t.class, t.count FROM call_costs c JOIN call_tokens t ON t.call_id = c.id WHERE t.class LIKE 'cache%' ORDER BY c.billed_at DESC LIMIT 40"
# Then pull both calls' .request.system[] from the payload log (jq in api-cost.md) and diff.
```

For a guided, step-by-step analysis use the **`cache-diagnosis`** skill.

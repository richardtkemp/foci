<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Anthropic prompt cache

## Mechanics

- **Per-session, prefix-matched.** The cached prefix is the system prompt (environment block + character files) + conversation history. Its stability is what makes the cache work.
- **Switching models rebuilds the cache** — a model switch invalidates the prefix.
- Multiple prefixes are cached simultaneously — switching *sessions* doesn't evict other sessions' caches.
- Foci caches character files in memory — edits take effect at the next compaction or restart, not mid-session.

## Cache-bust diagnosis

A "bust" is a call with `cache_read = 0` where you'd expect a hit. Find the last call with `cache_read > 0` before the first `cache_read = 0`, then extract and **diff their system-prompt blocks** to see what changed.

Common causes:
- a character/system-prompt file edit picked up by `bootstrap.Reload()`,
- a model switch,
- a service restart,
- compaction on another session that shares the prefix.

```bash
# Find the bust boundary
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_read, cache_write FROM api_calls ORDER BY ts DESC LIMIT 20"

# Then pull the two calls' system blocks from the payload log and diff (see api-cost.md
# for the jq to extract .request.system[] block sizes; extract each to a file, then diff).
```

For a guided, step-by-step bust analysis over the payload logs, use the dedicated **`cache-diagnosis`** skill — it's the deeper tool; this file is the quick reference.

<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Prompt-cache bust diagnosis

The cached prefix is the system prompt (environment block + character files) + history, per session. Foci caches character files in memory: edits take effect at the next compaction or restart, not mid-session.

A "bust" is a call with `cache_read_tokens = 0` where you expect a hit. Find the last call with a cache read before the first zero, then extract and **diff their system-prompt blocks**.

Common causes: a character/system-prompt edit picked up by `bootstrap.Reload()`, a model switch, a service restart, compaction on another session that shares the prefix.

**The `⚠️ <session>: cache bust, cache_read dropped X → Y` warning and the payload log exist only on the direct-API transport (`backend = "api"`).** Claude Code and opencode agents never fire the warning and never write `api-payload.jsonl`. For them, find the bust in `api.db` and get the system prompt from tracing (below).

```bash
# Find the bust boundary (every backend)
sqlite3 -readonly ~/data/api.db "SELECT c.billed_at, c.cost_usd, t.class, t.count FROM call_costs c JOIN call_tokens t ON t.call_id = c.id WHERE t.class LIKE 'cache%' ORDER BY c.billed_at DESC LIMIT 40"
```

Then get the two system prompts and diff them:

- **Direct API:** both calls' `.request.system[]` from the payload log (jq in `api-cost.md`).
- **Claude Code / opencode:** foci hands the prompt over once, at launch. With `[tracing]` on, the first turn after a launch or a prompt change exports a `system_prompt` event to Langfuse with the full text and its hash; later turns carry only the hash. A new hash between the two calls means the prompt changed. Read Langfuse with the `langfuse-v4-api` skill. Also check `foci.log` for `compaction reload gate: ... reason=prompt-changed` (DEBUG) and for restarts.

For a guided, step-by-step analysis use the **`cache-diagnosis`** skill.

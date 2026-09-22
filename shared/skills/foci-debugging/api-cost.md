<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# API calls, payloads & cost

## Auth

Provider API keys live in `secrets.toml` beside `foci.toml`. `foci auth` prompts, saves, and signals the running gateway to hot-swap (POST `/-/reload-credentials`). For Anthropic, CC credentials (`~/.claude/.credentials.json`) are a fallback — a pure-CC deployment needs no Anthropic key, and the startup `no Anthropic credentials` line is a caching probe, not an inference failure.

## API call log — `~/data/api.db`

Every LLM call, durable across restarts. (`~/logs/api.jsonl` mirrors the current process only — for history always use `api.db`.)

```bash
sqlite3 -readonly ~/data/api.db "SELECT ts, call_type, calculated_cost_usd FROM api_calls ORDER BY ts DESC LIMIT 10"
# call_type: conversation, compaction, summary, spawn, delegated_turn, subagent_turn
sqlite3 -readonly ~/data/api.db "SELECT SUM(calculated_cost_usd) FROM api_calls WHERE ts > '2026-03-04T06:00'"
```

**Before you state a cost figure, read `api-cost-accounting.md`** — `cost_usd` is cumulative, the
token columns have different scopes, and the `cost divergence` WARN is a sampler.

**Do NOT conclude a column is absent from a *grepped* `.schema`** — a keyword filter hides every non-matching line. Read the full `.schema api_calls`.

## Payload logs (JSONL)

Full request/response per call. Written whenever `payload_file` is non-empty, and it **defaults to `logs/api-payload.jsonl`** — so payload logging is on by default; set `payload_file = ""` in `[logging]` to disable. (`full_payload` exists in `[logging]` but does NOT gate writing.) Archives: `logs/archive/api-payload-*.jsonl.gz`. Large — filter with jq, never cat.

```bash
# Calls in a window with cache stats
tail -200 ~/logs/api-payload.jsonl | jq -r '
  select(.ts >= "TIME1" and .ts <= "TIME2") |
  "\(.ts) cache_read=\(.response.usage.cache_read_input_tokens // 0) cache_write=\(.response.usage.cache_creation_input_tokens // 0)"'

# System prompt block sizes for one call (extract two to files and diff to compare)
tail -200 ~/logs/api-payload.jsonl | jq -c '
  select(.ts >= "TIME") |
  [.request.system[] | {type, text_len: (.text // "" | length), cache: .cache_control}]' | head -1
```

## "Where did the cost go?"

```bash
sqlite3 -readonly ~/data/api.db "SELECT ts, agent_id, call_type, calculated_cost_usd, cache_read_tokens, cache_write_tokens FROM api_calls WHERE ts > datetime('now','-3 hours') ORDER BY calculated_cost_usd DESC LIMIT 10"
# cache busts: read nothing, wrote a lot
sqlite3 -readonly ~/data/api.db "SELECT ts, calculated_cost_usd, cache_write_tokens FROM api_calls WHERE cache_read_tokens = 0 AND cache_write_tokens > 10000 ORDER BY ts DESC LIMIT 10"
```

For *why* a cache bust happened (diffing the system prompt), see **cache.md**.


## Per-agent cost, and joining to `state.db:session_index`

**Since #1946, `api_calls.agent_id` holds the owning AGENT on every row**, so per-agent cost needs no join at all: `GROUP BY agent_id`. The Agent-tool `tool_use` id that this column used to hold moved to `api_calls.subagent_id` (populated on `subagent_turn` rows only). Before #1946 the column was NULL on all but 20 of 48,650 rows while carrying the same *name* as `session_index.agent_id` — so the obvious filter returned an empty result that read as "no data", and that trap cost two wrong answers to Dick.

Joining is still needed to break cost down by `session_type` (chat / reflection / keepalive / unknown). The join key is **verbatim equal** — no suffix, no transform:

```bash
# /usr/bin/sqlite3 (real binary): the readonly `sqlite3` wrapper BLOCKS ATTACH.
# -readonly covers the ATTACHed db too; -uri is unsupported in this build.
/usr/bin/sqlite3 -readonly -column -header ~/data/state.db "
ATTACH '$HOME/data/api.db' AS api;
SELECT si.session_type,
       COUNT(DISTINCT si.session_key)      AS n_sessions,
       ROUND(SUM(ac.calculated_cost_usd),2) AS total_usd
FROM session_index si
JOIN api.api_calls ac ON ac.session = si.session_key
WHERE ac.agent_id='clutch'
GROUP BY si.session_type ORDER BY total_usd DESC;"
```

**Gotchas that will mislead you:**
- **The `|` in `SELECT session_key, session_type` output is sqlite's default column separator, NOT part of the key.** Don't build a `substr(...,instr(...,'|'))` strip — it matches nothing and silently yields zero join hits. Use `-column` mode to see the real values.
- **Key-form encodes the cost model.** `chat` sessions are **root-form** (`agent/c<chatID>`) and accumulate the whole conversation's cost on one key (expensive). `reflection`/`keepalive`/most `unknown` are **branch-form** (`agent/c<chatID>/b<epoch>`) — typically one cheap spawned call each. A chatID hosts *mixed* types across its branches, so you cannot partition a root key's cost by type.
- **Join coverage is partial.** `api_calls.session` migrated from a legacy `agent:<id>:<kind>:<name>` grammar (e.g. `agent:clutch:cron:background-<epoch>`) to the current `agent/c/b` grammar. Legacy rows predate `session_index` and won't join — expect a large *untyped* remainder (`WHERE ac.session LIKE 'agent:%'`). Report the unmatched total as a coverage caveat; don't present the join as complete. (`agent_id` itself is fine on those rows — #1946's backfill parses both grammars.)

<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# API calls, payloads & cost

## Auth

Provider API keys live in `secrets.toml` beside `foci.toml`. `foci auth` prompts, saves, and signals the running gateway to hot-swap (POST `/-/reload-credentials`). For Anthropic, CC credentials (`~/.claude/.credentials.json`) are a fallback — a pure-CC deployment needs no Anthropic key, and the startup `no Anthropic credentials` line is a caching probe, not an inference failure.

## The cost ledger — `~/data/api.db`

Every LLM call, durable across restarts (#2111). (`~/logs/api.jsonl` mirrors the current process only — for history always use `api.db`.) **Read cost from the VIEWS, never from a column:** there is no cost column — counts live in `call_tokens` (one row per token class) and the views price them at the rates in effect when each call was billed.

- `call_costs` — one row per call: `billed_at` (UTC), `backend` (api / ccstream / opencode / codex), `kind` (call, compaction, summary, spawn, legacy, …), `session`, `agent_id`, `turn_id`, `actor` ('' = the session's own thread, else the subagent), `cost_usd` (NULL = unpriced, never a silent 0).
- `turn_costs` — per turn (`turns` + its calls): cost, parent/subagent split, `context_fill`, `still_running`.
- `daily_costs` (UTC days), `session_costs`; `call_class_costs` / `turn_class_costs` split by class.

```bash
sqlite3 -readonly ~/data/api.db "SELECT billed_at, backend, kind, model, cost_usd FROM call_costs ORDER BY billed_at DESC LIMIT 10"
sqlite3 -readonly ~/data/api.db "SELECT * FROM daily_costs ORDER BY day DESC LIMIT 7"
sqlite3 -readonly ~/data/api.db "SELECT TOTAL(cost_usd) FROM call_costs WHERE billed_at >= '2026-09-29T00:00'"
```

**`kind='legacy'` calls are pre-ledger rows** (everything before the cutover, and — until each switches — the delegated backends' turn-level rows): one call per turn or per subagent share, priced from their recorded figure when `cost_basis='recorded'`, with the pre-ledger `call_type` in `json_extract(detail,'$.v1_call_type')`. **Before you state a cost figure, read `api-cost-accounting.md`.**

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
sqlite3 -readonly ~/data/api.db "SELECT billed_at, agent_id, backend, kind, cost_usd FROM call_costs WHERE billed_at > strftime('%Y-%m-%dT%H:%M:%S', 'now', '-3 hours') ORDER BY cost_usd DESC LIMIT 10"
# the expensive turns, with their subagent share
sqlite3 -readonly ~/data/api.db "SELECT turn_id, cost_usd, subagent_cost_usd, calls FROM turn_costs ORDER BY started_at DESC LIMIT 10"
# cache busts: read nothing, wrote a lot (per-call counts; legacy calls hold turn totals)
sqlite3 -readonly ~/data/api.db "SELECT c.billed_at, c.cost_usd, w.count FROM call_costs c JOIN call_tokens w ON w.call_id = c.id AND w.class LIKE 'cache_write%' LEFT JOIN call_tokens r ON r.call_id = c.id AND r.class = 'cache_read' WHERE r.call_id IS NULL AND w.count > 10000 ORDER BY c.billed_at DESC LIMIT 10"
```

For *why* a cache bust happened (diffing the system prompt), see **cache.md**.


## Per-agent cost, and joining to `state.db:session_index`

**`agent_id` holds the owning AGENT on every call** (#1946), so per-agent cost needs no join at all: `SELECT agent_id, TOTAL(cost_usd) FROM call_costs GROUP BY agent_id`. The Agent-tool `tool_use` id that this column once held is the call's `actor` (pre-ledger: `subagent_id`). Before #1946 the column was NULL on all but 20 of 48,650 rows while carrying the same *name* as `session_index.agent_id` — so the obvious filter returned an empty result that read as "no data", and that trap cost two wrong answers to Dick.

Joining is still needed to break cost down by `session_type` (chat / reflection / keepalive / unknown). The join key is **verbatim equal** — no suffix, no transform:

```bash
# /usr/bin/sqlite3 (real binary): the readonly `sqlite3` wrapper BLOCKS ATTACH.
# -readonly covers the ATTACHed db too; -uri is unsupported in this build.
/usr/bin/sqlite3 -readonly -column -header ~/data/state.db "
ATTACH '$HOME/data/api.db' AS api;
SELECT si.session_type,
       COUNT(DISTINCT si.session_key)      AS n_sessions,
       ROUND(TOTAL(ac.cost_usd),2)         AS total_usd
FROM session_index si
JOIN api.call_costs ac ON ac.session = si.session_key
WHERE ac.agent_id='clutch'
GROUP BY si.session_type ORDER BY total_usd DESC;"
```

**Gotchas that will mislead you:**
- **The `|` in `SELECT session_key, session_type` output is sqlite's default column separator, NOT part of the key.** Don't build a `substr(...,instr(...,'|'))` strip — it matches nothing and silently yields zero join hits. Use `-column` mode to see the real values.
- **Key-form encodes the cost model.** `chat` sessions are **root-form** (`agent/c<chatID>`) and accumulate the whole conversation's cost on one key (expensive). `reflection`/`keepalive`/most `unknown` are **branch-form** (`agent/c<chatID>/b<epoch>`) — typically one cheap spawned call each. A chatID hosts *mixed* types across its branches, so you cannot partition a root key's cost by type.
- **Join coverage is partial.** The pre-ledger `session` column migrated from a legacy `agent:<id>:<kind>:<name>` grammar (e.g. `agent:clutch:cron:background-<epoch>`) to the current `agent/c/b` grammar. Legacy rows predate `session_index` and won't join — expect a large *untyped* remainder (`WHERE ac.session LIKE 'agent:%'`). Report the unmatched total as a coverage caveat; don't present the join as complete. (`agent_id` itself is fine on those rows — #1946's backfill parses both grammars.)

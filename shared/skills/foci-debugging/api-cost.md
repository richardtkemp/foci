<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# API calls, payloads & cost

## Auth

- Foci uses API keys for LLM providers (Anthropic, Gemini, OpenAI, OpenRouter), stored in `secrets.toml` alongside `foci.toml`.
- `foci auth` prompts for provider + API key, saves, and signals the running gateway to hot-swap credentials (POST `/-/reload-credentials`).
- For Anthropic, Claude Code credentials (`~/.claude/.credentials.json`) are used as a fallback when no API key is configured. (So a pure-CC deployment needs no Anthropic API key — the startup `no Anthropic credentials` line is a caching probe, not an inference failure.)

## API Call Log (SQLite) — `~/data/api.db`

Every LLM API call is logged to `~/data/api.db` table `api_calls`, durable across restarts. (`~/logs/api.jsonl` is the current-process-only mirror — for history across restarts always use `api.db`.)

```bash
# Recent calls
sqlite3 ~/data/api.db "SELECT ts, call_type, cost_usd FROM api_calls ORDER BY ts DESC LIMIT 10"

# Filter by type: conversation, compaction, summary, spawn
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_read, cache_write FROM api_calls WHERE call_type='conversation' ORDER BY ts DESC LIMIT 10"

# Cost in a time window -- SUM calculated_cost_usd, NEVER cost_usd (see below)
sqlite3 ~/data/api.db "SELECT SUM(calculated_cost_usd) FROM api_calls WHERE ts > '2026-03-04T06:00'"
```

**Column scopes differ (#1806):** `input/cache_read/cache_write_tokens` = the turn's FINAL-cycle context fill (a snapshot; pricing them recovers ~20% of cost). `output_tokens` + `turn_input/turn_cache_read/turn_cache_write_tokens` = per-TURN sums, what `calculated_cost_usd` priced. One row = one turn; no cycle ordinal. Table: WIRING.md "Cost columns".

**`cost_usd` is CUMULATIVE over the CC process — never `SUM` it; sum `calculated_cost_usd`.**
Two consequences. (1) Summing inflates ~quadratically in turns-per-process (to ~13x), yet is 1.0x
on quiet days — which is what lets it survive review. (2) It does not reconcile against the token columns, which hold one
round's snapshot: cost / a token column is a ROUND COUNT dressed as a rate — 3 rounds at Opus-5's
$0.50/M cache-read reads as "$1.50/M", indistinguishable from a stale-pricing fallback. Check
per-round usage in the CC transcript before calling a rate wrong. `calculated_cost_usd` is foci's
own priced figure and is authoritative (WIRING.md "Cost columns"); rows before #1674 have it NULL.

**Counting mispriced turns: query the table, not the log.** The `cost divergence` WARN is a
sampler — four gates, including **one warning per model per 10 min** (plus a 3% tolerance and a
$0.01 floor), so log lines undercount by an unknown factor. Get the backend's per-turn figure by
differencing the cumulative column per session (`LAG(cost_usd) OVER (PARTITION BY session ORDER BY
id)`). **A reset is NOT always a drop:** a restarted CC process can open ABOVE the old one's last
figure, so nothing falls and the difference silently spans the boundary — one such row inverted a
27-turn mean. Treat any row adjacent to a foci restart as unmeasured. **Control every run:**
single-turn branch sessions (`session LIKE '%/b%'`, no predecessor) must equal
`calculated_cost_usd` exactly.

## Payload Logs (JSONL)

Full request/response payloads per API call. Written whenever `payload_file` is non-empty — it defaults to `logs/api-payload.jsonl`, so payload logging is **on by default**; set `payload_file = ""` in `[logging]` to disable. (A separate `full_payload` bool exists in `[logging]` but does NOT gate writing.) Large file — filter with jq, don't cat.

**Path:** `logs/api-payload.jsonl` (default, configurable via `payload_file`)
**Archives:** `logs/archive/api-payload-*.jsonl.gz`

```bash
# Calls in a time window with cache stats
tail -200 ~/logs/api-payload.jsonl | jq -r '
  select(.ts >= "TIME1" and .ts <= "TIME2") |
  "\(.ts) cache_read=\(.response.usage.cache_read_input_tokens // 0) cache_write=\(.response.usage.cache_creation_input_tokens // 0)"'

# System prompt block sizes for a specific call
tail -200 ~/logs/api-payload.jsonl | jq -c '
  select(.ts >= "TIME") |
  [.request.system[] | {type, text_len: (.text // "" | length), cache: .cache_control}]' | head -1

# Compare system prompts between two calls (extract to files, then diff)
```

## "Where did the cost go?"

```bash
# Biggest calls / cache busts. Columns are *_tokens; cost is calculated_cost_usd.
sqlite3 -readonly ~/data/api.db "SELECT ts, call_type, calculated_cost_usd, cache_read_tokens, cache_write_tokens FROM api_calls WHERE ts > datetime('now','-3 hours') ORDER BY calculated_cost_usd DESC LIMIT 10"
sqlite3 -readonly ~/data/api.db "SELECT ts, calculated_cost_usd, cache_write_tokens FROM api_calls WHERE cache_read_tokens = 0 AND cache_write_tokens > 10000 ORDER BY ts DESC LIMIT 10"
```

For *why* a cache bust happened (diffing the system prompt), see **cache.md**.

## A `cost divergence` WARN: decompose it, don't theorise

The WARN carries every field needed to attribute the gap. Unpriced-TTL residue
is the usual culprit — Unknown prices at the 1h rate:

    Unknown = cache_write - ttl_1h - ttl_5m         # all three are in the WARN
    gap =~ Unknown x (rate_1h - rate_5m)            # opus-5: 10.00 - 6.25 $/M

Match to a few microdollars and the cause is settled. Cross-check against the
subagent's own transcript, which is the authority:

    ~/.claude/projects/<slug>/<parent-session-uuid>/subagents/agent-<id>.jsonl
    jq -r 'select(.message.stop_reason != null)
           | .message.usage.cache_creation_input_tokens' FILE

Its completed-message cache-write total equals Unknown exactly. Two matching
numbers from unrelated artifacts is a diagnosis; one is a coincidence.

Zero `call_type='subagent_turn'` rows means the correction path was never
REACHED — missing `stranded=`/`cost correction` lines then say nothing about
whether that code works.

## Joining cost to session metadata (`state.db:session_index`)

To break cost down by `session_type` (chat / reflection / keepalive / unknown), join `api_calls.session` to `session_index.session_key`. The join key is **verbatim equal** — no suffix, no transform:

```bash
# /usr/bin/sqlite3 (real binary): the readonly `sqlite3` wrapper BLOCKS ATTACH.
# -readonly covers the ATTACHed db too; -uri is unsupported in this build.
/usr/bin/sqlite3 -readonly -column -header ~/data/state.db "
ATTACH '$HOME/data/api.db' AS api;
SELECT si.session_type,
       COUNT(DISTINCT si.session_key) AS n_sessions,
       ROUND(SUM(ac.cost_usd),2)      AS total_usd,
       ROUND(SUM(ac.cost_usd)*1.0/COUNT(DISTINCT si.session_key),4) AS mean_usd
FROM session_index si
JOIN api.api_calls ac ON ac.session = si.session_key
WHERE si.agent_id='clutch'
GROUP BY si.session_type ORDER BY total_usd DESC;"
```

**Gotchas that will mislead you:**
- **The `|` in `SELECT session_key, session_type` output is sqlite's default column separator, NOT part of the key.** Don't build a `substr(...,instr(...,'|'))` strip — it matches nothing and silently yields zero join hits. Use `-column` mode to see the real values.
- **Key-form encodes the cost model.** `chat` sessions are **root-form** (`agent/c<chatID>`) and accumulate the whole conversation's cost on one key (expensive). `reflection`/`keepalive`/most `unknown` are **branch-form** (`agent/c<chatID>/b<epoch>`) — typically one cheap spawned call each. A chatID hosts *mixed* types across its branches, so you cannot partition a root key's cost by type.
- **Coverage is partial.** `api_calls.session` migrated from a legacy `agent:<id>:<kind>:<name>` grammar (e.g. `agent:clutch:cron:background-<epoch>`) to the current `agent/c/b` grammar. Legacy rows predate `session_index` and won't join — expect a large *untyped* remainder (check with `WHERE ac.session LIKE 'agent:%'`). Report the unmatched total as a coverage caveat, don't present the join as complete.

**Do NOT conclude a column is absent from a *grepped* `.schema`.** A keyword filter silently hides every non-matching line. `api_calls` really does have `ts` (indexed), `call_type`, `duration_ms`, `stop_reason` — read the full `.schema api_calls` before asserting otherwise.

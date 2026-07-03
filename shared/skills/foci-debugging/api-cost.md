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

# Cost in a time window
sqlite3 ~/data/api.db "SELECT SUM(cost_usd) FROM api_calls WHERE ts > '2026-03-04T06:00'"
```

**Token-field gotcha:** `cache_read` is the ONLY cumulative-per-call field — `input`/`output`/`cache_write` are per-call deltas (summable). Folding cumulative `cache_read` into a running total double-counts.

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
# Total cost in last N hours
sqlite3 ~/data/api.db "SELECT SUM(cost_usd), COUNT(*) FROM api_calls WHERE ts > datetime('now', '-3 hours')"

# Biggest individual calls
sqlite3 ~/data/api.db "SELECT ts, call_type, cost_usd, cache_read, cache_write FROM api_calls WHERE ts > datetime('now', '-3 hours') ORDER BY cost_usd DESC LIMIT 10"

# Cache busts (cache_read = 0 with large cache_write)
sqlite3 ~/data/api.db "SELECT ts, cost_usd, cache_write FROM api_calls WHERE cache_read = 0 AND cache_write > 10000 ORDER BY ts DESC LIMIT 10"
```

For *why* a cache bust happened (diffing the system prompt), see **cache.md**.

# langfuse-etl

Mirrors `api.db` (`api_calls`, one row per turn, all backends) into a self-hosted Langfuse as traces, so the
household's LLM spend, volume and latency are browsable per agent / model / session. Content (each turn's
prompt and reply) is joined from the per-agent `conversation.db` when `LANGFUSE_ETL_CONTENT=1` and a
redaction hash file is present (see `build-redactions.sh`); the payload logs are never read.

**Status: historical backfill + reconcile only.** Live turns are exported from inside the gateway by
`internal/telemetry` (`[tracing]` in foci.toml — see `docs/WIRING.md` "Tracing"), which sends the full
trace shape (tool calls, subagents, prompt/reply/thinking, system prompt, cross-agent links) that a
row-level mirror cannot. The two agree on the contract that matters: one `generation` observation per
api.db row with `cost = calculated_cost_usd`, so `reconcile` checks both eras with one query. **Do not run
`tail` while the Go exporter is enabled** — the ids differ (ETL: sha256 of the row id; Go: sha256 of the
turn id), so the same row would be counted twice. Cutover: stop the `tail` cron *before* deploying the
tracing build, then `backfill --from-id <watermark+1> --to-id <last row written by the old binary>` to
cover the gap between the last tail and the deploy.

Runs with [uv](https://docs.astral.sh/uv/) (inline script deps, no venv to manage):

```bash
set -a; . ~/.config/langfuse-etl.env; set +a     # LANGFUSE_HOST / _PUBLIC_KEY / _SECRET_KEY
scripts/langfuse-etl/etl.py backfill [--from-id N] [--to-id M] [--rate 40]
scripts/langfuse-etl/etl.py tail                        # cron: rows above the watermark, each sent exactly once
scripts/langfuse-etl/etl.py reconcile [--days 14]       # per-UTC-day api.db vs Langfuse Metrics API v2; exit 1 if any day differs > $0.05
```

Mapping: row → trace (name = `call_type`, user = `agent_id`, session = session key, tags = provider/call_type/token
scope) with one root GENERATION (model, usage from `turn_*` when present else the snapshot columns, cost =
`calculated_cost_usd`, start = `ts`, end = `ts + duration_ms`). Ids are sha256 of the row id; Langfuse v4 is append-only, so unchanged re-sends are absorbed but changed rows would be double-counted — rows are therefore sent exactly once and later #1918 corrections are not mirrored (visible in `reconcile`).

Transport is plain OTel/OTLP-HTTP with the official SDK's span attribute names (`langfuse.observation.*`,
`session.id`, `user.id`); the SDK itself can't set a historical start time, which the backfill needs.

Cost provenance lives in `docs/WIRING.md` ("Two cost columns"): rows before #1674 were backfilled once on
2026-09-18 from the best per-era source (exact token pricing for March; calibrated context-only pricing Apr–20 Jul;
CC's cumulative cost differenced 21 Jul–5 Aug). Treat pre-August figures as estimates.

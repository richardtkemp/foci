# langfuse-etl

Mirrors `api.db` (`api_calls`, one row per turn, all backends) into a self-hosted Langfuse as traces, so the
household's LLM spend, volume and latency are browsable per agent / model / session. Metadata only — nothing
from the payload logs is read, so no prompt or completion text leaves the host.

Runs with [uv](https://docs.astral.sh/uv/) (inline script deps, no venv to manage):

```bash
set -a; . ~/.config/langfuse-etl.env; set +a     # LANGFUSE_HOST / _PUBLIC_KEY / _SECRET_KEY
scripts/langfuse-etl/etl.py backfill [--from-id N] [--to-id M] [--rate 40]
scripts/langfuse-etl/etl.py tail [--overlap 300]        # cron: rows above the watermark, idempotent
scripts/langfuse-etl/etl.py reconcile [--days 14]       # per-UTC-day api.db vs Langfuse Metrics API v2; exit 1 if any day differs > $0.05
```

Mapping: row → trace (name = `call_type`, user = `agent_id`, session = session key, tags = provider/call_type/token
scope) with one root GENERATION (model, usage from `turn_*` when present else the snapshot columns, cost =
`calculated_cost_usd`, start = `ts`, end = `ts + duration_ms`). Ids are sha256 of the row id, so re-runs upsert.

Transport is plain OTel/OTLP-HTTP with the official SDK's span attribute names (`langfuse.observation.*`,
`session.id`, `user.id`); the SDK itself can't set a historical start time, which the backfill needs.

Cost provenance lives in `docs/WIRING.md` ("Two cost columns"): rows before #1674 were backfilled once on
2026-09-18 from the best per-era source (exact token pricing for March; calibrated context-only pricing Apr–20 Jul;
CC's cumulative cost differenced 21 Jul–5 Aug). Treat pre-August figures as estimates.

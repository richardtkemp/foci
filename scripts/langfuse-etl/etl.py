#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#   "opentelemetry-sdk>=1.27",
#   "opentelemetry-exporter-otlp-proto-http>=1.27",
#   "httpx>=0.27",
# ]
# ///
"""api.db -> Langfuse ETL.

One api_calls row = one Langfuse trace holding a single root GENERATION observation. Nothing from the
payload logs is read: the only fields that leave this host are the ones in api_calls (tokens, cost, model,
session key, agent id, timings). No prompt or completion content, by construction.

Modes
  --backfill            every row (or --from-id N onwards), paced with --rate rows/s
  --tail                rows above the watermark file, strictly. Langfuse v4 is append-only: re-sending an
                        observation whose content changed stores a second version and aggregations count both
                        (measured 2026-09-18: 40 rows re-sent after #1918 corrections -> 67 observations, +65% cost).
                        Byte-identical re-sends are absorbed. So rows are sent exactly once and later corrections
                        are NOT mirrored; `reconcile` shows that drift.
  --reconcile [--days N] per-UTC-day SUM(calculated_cost_usd) from api.db vs Langfuse Metrics API v2 sum(totalCost)

Transport is plain OpenTelemetry over OTLP/HTTP (protobuf) to Langfuse's /api/public/otel endpoint, using the
same span attributes the official Python SDK emits (langfuse.observation.*, session.id, user.id, ...). The SDK
itself is not used because it offers no way to set a historical start time on an observation.

Trace id = sha256("api.db:<id>")[:32], span id = sha256("api.db:<id>:gen")[:16] -> a re-run of unchanged rows is
absorbed by Langfuse; a re-run of CHANGED rows duplicates them (see --tail). Backfill only over ranges not yet sent.

Env: LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY; optional API_DB (~/data/api.db),
     LANGFUSE_ETL_WATERMARK (~/data/langfuse-etl.watermark), LANGFUSE_ENVIRONMENT (production)
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import sqlite3
import sys
import time
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

import httpx
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.trace.id_generator import IdGenerator
from opentelemetry.trace import SpanKind

HOME = Path.home()
API_DB = os.environ.get("API_DB", str(HOME / "data" / "api.db"))
WATERMARK = Path(os.environ.get("LANGFUSE_ETL_WATERMARK", str(HOME / "data" / "langfuse-etl.watermark")))
ENVIRONMENT = os.environ.get("LANGFUSE_ENVIRONMENT", "production")


def env(name: str) -> str:
    v = os.environ.get(name)
    if not v:
        sys.exit(f"missing env {name}")
    return v


class SeededIdGenerator(IdGenerator):
    """OTel generates ids at span start; we pre-set the next pair so ids are a pure function of the row id."""

    def __init__(self) -> None:
        self.trace_id = 0
        self.span_id = 0

    def generate_trace_id(self) -> int:
        return self.trace_id

    def generate_span_id(self) -> int:
        return self.span_id


def ids_for(row_id: int) -> tuple[int, int]:
    t = hashlib.sha256(f"api.db:{row_id}".encode()).hexdigest()[:32]
    s = hashlib.sha256(f"api.db:{row_id}:gen".encode()).hexdigest()[:16]
    return int(t, 16), int(s, 16)


def parse_ts(ts: str) -> datetime:
    d = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    if d.tzinfo is None:
        d = d.replace(tzinfo=timezone.utc)
    return d.astimezone(timezone.utc)


def open_db() -> sqlite3.Connection:
    db = sqlite3.connect(f"file:{API_DB}?mode=ro", uri=True, timeout=30)
    db.row_factory = sqlite3.Row
    return db


COLS = """id, ts, session, model, provider, call_type, agent_id, turn_id, stop_reason, duration_ms,
          input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
          turn_input_tokens, turn_output_tokens, turn_cache_read_tokens, turn_cache_write_tokens,
          cost_usd, calculated_cost_usd"""


def make_tracer(host: str, pk: str, sk: str) -> tuple[trace.Tracer, TracerProvider, SeededIdGenerator]:
    auth = "Basic " + base64.b64encode(f"{pk}:{sk}".encode()).decode()
    exporter = OTLPSpanExporter(
        endpoint=f"{host.rstrip('/')}/api/public/otel/v1/traces",
        headers={"Authorization": auth, "x-langfuse-public-key": pk, "x-langfuse-sdk-name": "foci-api-db-etl"},
        timeout=30,
    )
    gen = SeededIdGenerator()
    provider = TracerProvider(resource=Resource.create({"service.name": "foci", "service.namespace": "api.db-etl"}), id_generator=gen)
    provider.add_span_processor(BatchSpanProcessor(exporter, max_queue_size=8192, max_export_batch_size=256, schedule_delay_millis=1000))
    return provider.get_tracer("foci.api_db_etl"), provider, gen


def emit(tracer: trace.Tracer, gen: SeededIdGenerator, r: sqlite3.Row) -> None:
    start = parse_ts(r["ts"])
    end = start + timedelta(milliseconds=r["duration_ms"] or 0)
    start_ns, end_ns = int(start.timestamp() * 1e9), int(end.timestamp() * 1e9)
    gen.trace_id, gen.span_id = ids_for(r["id"])

    if r["turn_input_tokens"] is not None:
        scope = "turn"
        usage = {"input": r["turn_input_tokens"] or 0, "output": r["turn_output_tokens"] if r["turn_output_tokens"] is not None else (r["output_tokens"] or 0),
                 "cache_read_input_tokens": r["turn_cache_read_tokens"] or 0, "cache_creation_input_tokens": r["turn_cache_write_tokens"] or 0}
    else:
        scope = "snapshot"  # pre-#1854 rows: final-cycle context fill, output summed — see WIRING.md "Two scopes of token columns"
        usage = {"input": r["input_tokens"] or 0, "output": r["output_tokens"] or 0,
                 "cache_read_input_tokens": r["cache_read_tokens"] or 0, "cache_creation_input_tokens": r["cache_write_tokens"] or 0}
    usage["total"] = sum(usage.values())

    agent = r["agent_id"] or (r["session"].split("/", 1)[0] if r["session"] and "/" in r["session"] else None)
    call_type = r["call_type"] or "turn"
    attrs = {
        "langfuse.observation.type": "generation",
        "langfuse.observation.model.name": r["model"] or "unknown",
        "langfuse.observation.usage_details": json.dumps(usage),
        "langfuse.observation.cost_details": json.dumps({"total": r["calculated_cost_usd"] if r["calculated_cost_usd"] is not None else 0.0}),
        "langfuse.observation.level": "DEFAULT",
        "langfuse.trace.name": call_type,
        "langfuse.environment": ENVIRONMENT,
        "langfuse.trace.tags": [x for x in (r["provider"], call_type, f"tokens:{scope}") if x],
        "langfuse.observation.metadata.api_db_id": int(r["id"]),
        "langfuse.observation.metadata.token_scope": scope,
        "langfuse.observation.metadata.source": "api.db",
    }
    if agent:
        attrs["user.id"] = agent
    if r["session"]:
        attrs["session.id"] = r["session"]
    for k in ("turn_id", "stop_reason", "provider", "call_type"):
        if r[k]:
            attrs[f"langfuse.observation.metadata.{k}"] = r[k]
    if r["cost_usd"] is not None:
        attrs["langfuse.observation.metadata.backend_cost_usd"] = float(r["cost_usd"])
    if r["duration_ms"] is not None:
        attrs["langfuse.observation.metadata.duration_ms"] = int(r["duration_ms"])

    span = tracer.start_span(f"{call_type} {r['model'] or ''}".strip(), kind=SpanKind.CLIENT, attributes=attrs, start_time=start_ns)
    span.end(end_time=end_ns)


def run_rows(rows, rate: float, verbose: bool) -> int:
    host, pk, sk = env("LANGFUSE_HOST"), env("LANGFUSE_PUBLIC_KEY"), env("LANGFUSE_SECRET_KEY")
    tracer, provider, gen = make_tracer(host, pk, sk)
    n, t0, last_id = 0, time.monotonic(), None
    for r in rows:
        emit(tracer, gen, r)
        n += 1
        last_id = r["id"]
        if rate > 0:
            lag = n / rate - (time.monotonic() - t0)
            if lag > 0:
                time.sleep(lag)
        if verbose and n % 1000 == 0:
            print(f"{n} rows, at id {last_id} ({r['ts']})", flush=True)
    provider.force_flush(60_000)
    provider.shutdown()
    return last_id if last_id is not None else -1


def cmd_backfill(a) -> None:
    db = open_db()
    to_id = a.to_id if a.to_id is not None else 2**62
    rows = db.execute(f"SELECT {COLS} FROM api_calls WHERE id BETWEEN ? AND ? ORDER BY id", (a.from_id, to_id))
    total = db.execute("SELECT COUNT(*) FROM api_calls WHERE id BETWEEN ? AND ?", (a.from_id, to_id)).fetchone()[0]
    print(f"backfill: {total} rows, ids {a.from_id}..{to_id if a.to_id is not None else 'end'} at {a.rate} rows/s", flush=True)
    last = run_rows(rows, a.rate, True)
    if last >= 0 and not a.no_watermark:
        WATERMARK.write_text(str(last))
    print(f"done; last id {last}", flush=True)


def cmd_tail(a) -> None:
    wm = int(WATERMARK.read_text().strip()) if WATERMARK.exists() else 0
    db = open_db()
    rows = db.execute(f"SELECT {COLS} FROM api_calls WHERE id > ? ORDER BY id", (max(0, wm - a.overlap),)).fetchall()
    if not rows:
        return
    last = run_rows(rows, 0, False)
    if last > wm:
        WATERMARK.write_text(str(last))
    if a.verbose:
        print(f"tail: {len(rows)} rows, watermark {wm} -> {max(wm, last)}")


def cmd_reconcile(a) -> None:
    host, pk, sk = env("LANGFUSE_HOST"), env("LANGFUSE_PUBLIC_KEY"), env("LANGFUSE_SECRET_KEY")
    to = datetime.now(timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0) + timedelta(days=1)
    frm = to - timedelta(days=a.days)
    db = open_db()
    local = defaultdict(float)
    for ts, c in db.execute("SELECT ts, calculated_cost_usd FROM api_calls WHERE calculated_cost_usd IS NOT NULL"):
        d = parse_ts(ts)
        if frm <= d < to:
            local[d.strftime("%Y-%m-%d")] += c
    remote = {}
    step = timedelta(days=60)  # the Metrics API returns at most 100 rows per query; keep each window under that
    w0 = frm
    while w0 < to:
        w1 = min(w0 + step, to)
        q = {"view": "observations", "metrics": [{"measure": "totalCost", "aggregation": "sum"}], "dimensions": [],
             "timeDimension": {"granularity": "day"}, "fromTimestamp": w0.isoformat(), "toTimestamp": w1.isoformat(),
             "orderBy": [{"field": "time_dimension", "direction": "asc"}]}
        r = httpx.get(f"{host.rstrip('/')}/api/public/v2/metrics", params={"query": json.dumps(q)}, auth=(pk, sk), timeout=60)
        r.raise_for_status()
        for row in r.json().get("data", []):
            remote[str(row.get("time_dimension"))[:10]] = float(row.get("sum_totalCost") or 0)
        w0 = w1
    days = sorted(set(local) | set(remote))
    print(f"{'day':10} {'api.db':>10} {'langfuse':>10} {'diff':>9}")
    worst = 0.0
    for d in days:
        l, rm = local.get(d, 0.0), remote.get(d, 0.0)
        worst = max(worst, abs(l - rm))
        print(f"{d:10} {l:10.2f} {rm:10.2f} {rm - l:+9.2f}")
    print(f"total  api.db {sum(local.values()):.2f}  langfuse {sum(remote.values()):.2f}  worst day |diff| {worst:.2f}")
    if worst > a.tolerance:
        sys.exit(1)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    b = sub.add_parser("backfill"); b.add_argument("--from-id", type=int, default=1); b.add_argument("--to-id", type=int, default=None); b.add_argument("--rate", type=float, default=40.0); b.add_argument("--no-watermark", action="store_true")
    t = sub.add_parser("tail"); t.add_argument("--overlap", type=int, default=0, help="re-send this many rows below the watermark (0: never; see docstring)"); t.add_argument("--verbose", action="store_true")
    r = sub.add_parser("reconcile"); r.add_argument("--days", type=int, default=14); r.add_argument("--tolerance", type=float, default=0.05)
    a = ap.parse_args()
    {"backfill": cmd_backfill, "tail": cmd_tail, "reconcile": cmd_reconcile}[a.cmd](a)


if __name__ == "__main__":
    main()

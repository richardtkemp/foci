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

Content (LANGFUSE_ETL_CONTENT=1): each turn's prompt and reply are attached as observation input/output, verbatim
(nothing stripped; only Langfuse's own 2 MB field cap applies). Source: <agent>/.data/conversation.db — input is the
last `recv` in the same session within 15 min before the turn, output the first `sent` between the turn's start and
its end + 2 min. Turns with no human message (cron, keepalive, branches) get no content and content_source=none.
Secrets are redacted before anything is sent: every token-like substring is SHA-256 hashed and compared against
the hash list produced by build-redactions.sh (secret values never reach this process), plus generic key-shaped
patterns. Content is refused if the hash list is missing.

Env: LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY, LANGFUSE_SECRET_KEY; optional API_DB (~/data/api.db),
     LANGFUSE_ETL_WATERMARK (~/data/langfuse-etl.watermark), LANGFUSE_ENVIRONMENT (production),
     LANGFUSE_ETL_CONTENT (0/1), LANGFUSE_ETL_REDACT_HASHES (~/.config/langfuse-etl.redact-hashes),
     FOCI_HOME (/home/foci) for <agent>/.data/conversation.db
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
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
CONTENT = os.environ.get("LANGFUSE_ETL_CONTENT", "0") == "1"
REDACT_HASHES = Path(os.environ.get("LANGFUSE_ETL_REDACT_HASHES", str(HOME / ".config" / "langfuse-etl.redact-hashes")))
FOCI_HOME = Path(os.environ.get("FOCI_HOME", "/home/foci"))
FIELD_CAP = 2_000_000  # Langfuse LANGFUSE_OBSERVATION_FIELD_SIZE_LIMIT_BYTES default is 2 MiB
STATS = {"rows": 0, "with_input": 0, "with_output": 0, "redactions": 0}


def env(name: str) -> str:
    v = os.environ.get(name)
    if not v:
        sys.exit(f"missing env {name}")
    return v


class RecordingExporter(OTLPSpanExporter):
    """The batch processor swallows export failures; remember them so a tail run can refuse to advance the watermark."""

    failed = 0

    def export(self, spans):
        from opentelemetry.sdk.trace.export import SpanExportResult
        res = super().export(spans)
        if res != SpanExportResult.SUCCESS:
            RecordingExporter.failed += len(spans)
        return res


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


ROUTE_SEGMENTS = {"anthropic", "claude", "openrouter", "codex", "gemini", "openai"}


def normalize_model(model: str | None) -> tuple[str, list[str]]:
    """Mirror modelinfo.Normalize: bare leaf id, lowercased, trailing -YYYYMMDD stripped; route segments returned as tags.
    'anthropic/claude-opus-4-6' and 'claude/claude-opus-4-8' both become their leaf; 'claude-opus-4-6[1m]' keeps its suffix."""
    m = (model or "").strip()
    if not m:
        return "unknown", []
    parts = m.split("/")
    leaf = parts[-1].lower()
    leaf = re.sub(r"-\d{8}$", "", leaf)
    routes = [f"route:{p.lower().lstrip('~')}" for p in parts[:-1] if p]
    return leaf, routes


def agent_of(session: str | None) -> str | None:
    """Both key grammars: pre-stable-identity 'agent:<name>:<kind>:<id>' and current '<name>/c<chat>[/b<ts>]'."""
    if not session:
        return None
    if session.startswith("agent:"):
        seg = session.split(":")
        return seg[1] if len(seg) > 1 and seg[1] else None
    return session.split("/", 1)[0] or None


TRACE_NAME = {"conversation": "turn", "delegated_turn": "turn"}  # same thing, direct-API era vs delegated era (c3613d54)
BACKEND_OF = {"conversation": "backend:api", "delegated_turn": "backend:delegated", "subagent_turn": "backend:delegated"}


# ---------------------------------------------------------------- redaction
GENERIC_SECRET_PATTERNS = [
    re.compile(r"sk-[A-Za-z0-9_\-]{16,}"),                       # OpenAI/Anthropic/Langfuse-style keys
    re.compile(r"pk-lf-[0-9a-f\-]{20,}"),
    re.compile(r"gh[pousr]_[A-Za-z0-9]{20,}"),                    # GitHub tokens
    re.compile(r"xox[abpr]-[A-Za-z0-9\-]{10,}"),                  # Slack
    re.compile(r"AKIA[0-9A-Z]{16}"),                               # AWS access key id
    re.compile(r"AIza[0-9A-Za-z_\-]{30,}"),                       # Google API key
    re.compile(r"\d{8,10}:[A-Za-z0-9_\-]{30,}"),                  # Telegram bot token
    re.compile(r"eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}"),  # JWT
    re.compile(r"(?i)bearer\s+[A-Za-z0-9._\-]{16,}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S),
]
TOKEN_PATTERNS = [re.compile(r"[A-Za-z0-9_\-]{8,}"), re.compile(r"[A-Za-z0-9_\-.:+/=]{8,}")]
_hashes: set[str] | None = None


def load_hashes() -> set[str]:
    global _hashes
    if _hashes is None:
        if not REDACT_HASHES.exists():
            sys.exit(f"content enabled but {REDACT_HASHES} is missing — run build-redactions.sh (sudo) first")
        _hashes = {l.strip() for l in REDACT_HASHES.read_text().splitlines() if l.strip()}
    return _hashes


def redact(text: str) -> tuple[str, int]:
    """Replace known-secret tokens (by hash) and key-shaped strings. Returns (text, replacements)."""
    if not text:
        return text, 0
    hashes = load_hashes()
    n = 0

    def sub_hash(m):
        nonlocal n
        if hashlib.sha256(m.group(0).encode()).hexdigest() in hashes:
            n += 1
            return "[REDACTED]"
        return m.group(0)

    for pat in TOKEN_PATTERNS:
        text = pat.sub(sub_hash, text)
    for pat in GENERIC_SECRET_PATTERNS:
        text, k = pat.subn("[REDACTED]", text)
        n += k
    return text, n


# ---------------------------------------------------------------- content join (conversation.db)
class Convo:
    """Per-agent conversation.db, indexed by session: sorted (ts, direction, text)."""

    def __init__(self) -> None:
        self.by_agent: dict[str, dict[str, list]] = {}

    def _load(self, agent: str) -> dict[str, list]:
        if agent in self.by_agent:
            return self.by_agent[agent]
        idx: dict[str, list] = {}
        path = FOCI_HOME / agent / ".data" / "conversation.db"
        if path.exists():
            db = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=30)
            db.text_factory = lambda b: b.decode("utf-8", "replace")  # a few rows hold invalid UTF-8
            for ts, direction, sess, text in db.execute("SELECT ts, direction, session, text FROM messages WHERE session IS NOT NULL AND session != ''"):
                idx.setdefault(sess, []).append((parse_ts(ts), direction, text))
            db.close()
            for v in idx.values():
                v.sort(key=lambda x: x[0])
        self.by_agent[agent] = idx
        return idx

    def lookup(self, agent: str | None, session: str | None, start: datetime, lo: datetime, hi: datetime) -> tuple[str | None, str | None]:
        if not agent or not session:
            return None, None
        msgs = self._load(agent).get(session)
        if not msgs:
            return None, None
        inp = out = None
        grace = timedelta(seconds=2)
        for t, direction, text in msgs:
            if t > hi:
                break
            if direction == "recv" and lo <= t <= start + grace:
                inp = text  # latest recv at/just after the turn start
            elif direction == "sent" and start <= t <= hi and out is None:
                out = text
        return inp, out


CONVO = Convo()
_turns: dict[str, list[datetime]] | None = None


def turn_times(db: sqlite3.Connection) -> dict[str, list[datetime]]:
    """session -> sorted turn start times, so content is bounded by the previous/next turn in that session."""
    global _turns
    if _turns is None:
        _turns = {}
        for sess, ts in db.execute("SELECT session, ts FROM api_calls WHERE session IS NOT NULL"):
            _turns.setdefault(sess, []).append(parse_ts(ts))
        for v in _turns.values():
            v.sort()
    return _turns


def content_bounds(db: sqlite3.Connection, session: str | None, start: datetime, end: datetime) -> tuple[datetime, datetime]:
    import bisect
    # api.db stamps the turn start to the second and slightly BEFORE the inbound message is logged, so the
    # triggering recv can sit a few hundred ms after `start`; give both edges a 2 s grace.
    grace = timedelta(seconds=2)
    lo, hi = start - timedelta(minutes=15), end + timedelta(minutes=2)
    ts = turn_times(db).get(session or "", [])
    i = bisect.bisect_left(ts, start)
    if i > 0:
        lo = max(lo, ts[i - 1] + grace)   # not the previous turn's own message
    if i + 1 < len(ts):
        hi = min(hi, ts[i + 1] + grace)   # not past the next turn's start
    return lo, hi


def open_db() -> sqlite3.Connection:
    db = sqlite3.connect(f"file:{API_DB}?mode=ro", uri=True, timeout=30)
    db.row_factory = sqlite3.Row
    return db


COLS = """id, ts, session, model, provider, call_type, agent_id, turn_id, stop_reason, duration_ms,
          input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
          turn_input_tokens, turn_output_tokens, turn_cache_read_tokens, turn_cache_write_tokens,
          cost_usd, calculated_cost_usd"""


def healthy(host: str) -> bool:
    try:
        r = httpx.get(f"{host.rstrip('/')}/api/public/health", timeout=15)
        return r.status_code == 200 and r.json().get("status") == "OK"
    except Exception as e:  # noqa: BLE001
        print(f"langfuse health check failed: {e}", file=sys.stderr)
        return False


def make_tracer(host: str, pk: str, sk: str) -> tuple[trace.Tracer, TracerProvider, SeededIdGenerator]:
    auth = "Basic " + base64.b64encode(f"{pk}:{sk}".encode()).decode()
    exporter = RecordingExporter(
        endpoint=f"{host.rstrip('/')}/api/public/otel/v1/traces",
        headers={"Authorization": auth, "x-langfuse-public-key": pk, "x-langfuse-sdk-name": "foci-api-db-etl"},
        timeout=30,
    )
    gen = SeededIdGenerator()
    provider = TracerProvider(resource=Resource.create({"service.name": "foci", "service.namespace": "api.db-etl"}), id_generator=gen)
    provider.add_span_processor(BatchSpanProcessor(exporter, max_queue_size=8192, max_export_batch_size=256, schedule_delay_millis=1000))
    return provider.get_tracer("foci.api_db_etl"), provider, gen


def emit(tracer: trace.Tracer, gen: SeededIdGenerator, r: sqlite3.Row, db: sqlite3.Connection) -> None:
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

    call_type = r["call_type"] or "turn"
    model, route_tags = normalize_model(r["model"])
    # Subagent rows (#1880) carry the Agent tool_use id as agent_id; book them under the parent agent (the session's owner).
    is_subagent = call_type == "subagent_turn" or (r["agent_id"] or "").startswith("toolu_")
    agent = agent_of(r["session"]) if is_subagent or not r["agent_id"] else r["agent_id"]
    tags = [t for t in (BACKEND_OF.get(call_type), f"call_type:{call_type}", f"tokens:{scope}", *route_tags) if t]
    if is_subagent:
        tags.append("subagent")
    attrs = {
        "langfuse.observation.type": "generation",
        "langfuse.observation.model.name": model,
        "langfuse.observation.usage_details": json.dumps(usage),
        "langfuse.observation.cost_details": json.dumps({"total": r["calculated_cost_usd"] if r["calculated_cost_usd"] is not None else 0.0}),
        "langfuse.observation.level": "DEFAULT",
        "langfuse.trace.name": TRACE_NAME.get(call_type, call_type),
        "langfuse.environment": ENVIRONMENT,
        "langfuse.trace.tags": tags,
        "langfuse.observation.metadata.api_db_id": int(r["id"]),
        "langfuse.observation.metadata.token_scope": scope,
        "langfuse.observation.metadata.source": "api.db",
        "langfuse.observation.metadata.model_raw": r["model"] or "",
    }
    if agent:
        attrs["user.id"] = agent
    if r["session"]:
        attrs["session.id"] = r["session"]
    if is_subagent and (r["agent_id"] or "").startswith("toolu_"):
        attrs["langfuse.observation.metadata.subagent_tool_use_id"] = r["agent_id"]
    if CONTENT and not is_subagent:  # a subagent's prompt is the Agent tool call, not the human's message
        lo, hi = content_bounds(db, r["session"], start, end)
        inp, out = CONVO.lookup(agent, r["session"], start, lo, hi)
        redactions = 0
        if inp is not None:
            attrs["langfuse.observation.metadata.input_chars"] = len(inp)
            inp, k = redact(inp); redactions += k
            attrs["langfuse.observation.input"] = inp[:FIELD_CAP]
        if out is not None:
            attrs["langfuse.observation.metadata.output_chars"] = len(out)
            out, k = redact(out); redactions += k
            attrs["langfuse.observation.output"] = out[:FIELD_CAP]
        attrs["langfuse.observation.metadata.content_source"] = "conversation.db" if (inp is not None or out is not None) else "none"
        if redactions:
            attrs["langfuse.observation.metadata.redactions"] = redactions
        STATS["with_input"] += inp is not None; STATS["with_output"] += out is not None; STATS["redactions"] += redactions
    STATS["rows"] += 1
    for k in ("turn_id", "stop_reason", "provider", "call_type"):
        if r[k]:
            attrs[f"langfuse.observation.metadata.{k}"] = r[k]
    if r["cost_usd"] is not None:
        attrs["langfuse.observation.metadata.backend_cost_usd"] = float(r["cost_usd"])
    if r["duration_ms"] is not None:
        attrs["langfuse.observation.metadata.duration_ms"] = int(r["duration_ms"])

    span = tracer.start_span(f"{TRACE_NAME.get(call_type, call_type)} {model}", kind=SpanKind.CLIENT, attributes=attrs, start_time=start_ns)
    span.end(end_time=end_ns)


def run_rows(rows, rate: float, verbose: bool, db: sqlite3.Connection) -> int:
    host, pk, sk = env("LANGFUSE_HOST"), env("LANGFUSE_PUBLIC_KEY"), env("LANGFUSE_SECRET_KEY")
    tracer, provider, gen = make_tracer(host, pk, sk)
    n, t0, last_id = 0, time.monotonic(), None
    for r in rows:
        emit(tracer, gen, r, db)
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
    if RecordingExporter.failed:
        print(f"export failed for {RecordingExporter.failed} spans; watermark not advanced", file=sys.stderr)
        return -1
    return last_id if last_id is not None else -1


def cmd_backfill(a) -> None:
    db = open_db()
    to_id = a.to_id if a.to_id is not None else 2**62
    rows = db.execute(f"SELECT {COLS} FROM api_calls WHERE id BETWEEN ? AND ? ORDER BY id", (a.from_id, to_id))
    total = db.execute("SELECT COUNT(*) FROM api_calls WHERE id BETWEEN ? AND ?", (a.from_id, to_id)).fetchone()[0]
    print(f"backfill: {total} rows, ids {a.from_id}..{to_id if a.to_id is not None else 'end'} at {a.rate} rows/s", flush=True)
    last = run_rows(rows, a.rate, True, db)
    if last < 0:
        sys.exit(1)
    if not a.no_watermark:
        WATERMARK.write_text(str(last))
    if CONTENT and STATS["rows"]:
        print(f"content: input on {STATS['with_input']}/{STATS['rows']} rows, output on {STATS['with_output']}, {STATS['redactions']} redactions", flush=True)
    print(f"done; last id {last}", flush=True)


def cmd_tail(a) -> None:
    wm = int(WATERMARK.read_text().strip()) if WATERMARK.exists() else 0
    if not healthy(env("LANGFUSE_HOST")):
        sys.exit(2)  # nothing sent, watermark untouched; cron retries in 5 min
    db = open_db()
    cutoff = (datetime.now(timezone.utc) - timedelta(seconds=a.min_age)).isoformat()
    rows = [r for r in db.execute(f"SELECT {COLS} FROM api_calls WHERE id > ? ORDER BY id", (max(0, wm - a.overlap),)).fetchall()
            if parse_ts(r["ts"]).isoformat() <= cutoff]
    if not rows:
        return
    last = run_rows(rows, 0, False, db)
    if last < 0:
        sys.exit(1)
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


def cmd_show(a) -> None:
    db = open_db()
    for r in db.execute(f"SELECT {COLS} FROM api_calls WHERE id IN ({','.join('?'*len(a.ids))}) ORDER BY id", a.ids):
        model, routes = normalize_model(r["model"]); ct = r["call_type"] or "turn"
        sub = ct == "subagent_turn" or (r["agent_id"] or "").startswith("toolu_")
        agent = agent_of(r['session']) if sub or not r['agent_id'] else r['agent_id']
        print(f"{r['id']} {r['ts'][:19]} name={TRACE_NAME.get(ct, ct)!r} user={agent!r} model={model!r} routes={routes} session={r['session']!r} sub={sub}")
        if CONTENT:
            st = parse_ts(r["ts"]); lo, hi = content_bounds(db, r["session"], st, st + timedelta(milliseconds=r["duration_ms"] or 0))
            inp, out = (None, None) if sub else CONVO.lookup(agent, r["session"], st, lo, hi)
            ri, ki = redact(inp) if inp else (None, 0); ro, ko = redact(out) if out else (None, 0)
            print(f"   input ({len(inp) if inp else 0} chars, {ki} redacted): {(ri or '')[:120]!r}")
            print(f"   output({len(out) if out else 0} chars, {ko} redacted): {(ro or '')[:120]!r}")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    b = sub.add_parser("backfill"); b.add_argument("--from-id", type=int, default=1); b.add_argument("--to-id", type=int, default=None); b.add_argument("--rate", type=float, default=40.0); b.add_argument("--no-watermark", action="store_true")
    t = sub.add_parser("tail"); t.add_argument("--overlap", type=int, default=0, help="re-send this many rows below the watermark (0: never; see docstring)"); t.add_argument("--verbose", action="store_true"); t.add_argument("--min-age", type=int, default=120, help="seconds a row must be old before it is sent (lets the reply land in conversation.db)")
    sh = sub.add_parser("show"); sh.add_argument("ids", type=int, nargs="+")
    r = sub.add_parser("reconcile"); r.add_argument("--days", type=int, default=14); r.add_argument("--tolerance", type=float, default=0.05)
    a = ap.parse_args()
    {"backfill": cmd_backfill, "tail": cmd_tail, "reconcile": cmd_reconcile, "show": cmd_show}[a.cmd](a)


if __name__ == "__main__":
    main()

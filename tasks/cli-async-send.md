# Task: Async mode for clod send

## Problem
`clod send` is always synchronous — it waits for the full agent response before returning. For cron jobs this means the cron process hangs for minutes. Most callers don't need the response — they just want to inject a message and let the agent handle it, with any reply going to Telegram.

## Requirements

### CLI flag
- `--async` / `--no-wait` flag on `clod send` (and `clod branch` for consistency)
- Default: **async** (fire and forget, return immediately after the gateway accepts the message)
- `--sync` / `--wait` for when you DO want the response (e.g. `clod eval`)
- Env var: `CLOD_ASYNC` (non-empty = true), `CLOD_SYNC` for the inverse

### Gateway support
- The `/send` endpoint needs to support an `async` field in the request body
- When `async: true`: accept the message, queue it for processing, return `202 Accepted` immediately with `{"status": "queued"}`
- The agent processes the message in a background goroutine, any response goes to Telegram as normal
- When `async: false` (or omitted for backward compat): current sync behaviour

### Default behaviour
- `clod send` → async (default)
- `clod send --sync` → wait for response
- `clod branch` → async (default) 
- `clod eval` → always sync (needs the response)

## Update docs
- docs/CLI.md — new flags
- docs/CONFIG.md if any config needed
- SPEC.md if relevant

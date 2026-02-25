# Task: Built-in log rotation with archival

## Problem
Logs grow unbounded. api-payload.jsonl hit 3.1GB in 4 days. Need periodic rotation that archives old entries and keeps recent ones in the active log.

## Design

### Behaviour
Every `rotation_period` (default 24h), for each log file (api.jsonl, api-payload.jsonl, clod.log):
1. Stream through the file line by line (do NOT load entire file into memory — these can be multi-GB)
2. For each line, extract the timestamp (JSONL files have `ts` field; clod.log has timestamp prefix)
3. Lines older than `retention_period` (default 2d) → write to a gzip archive file
4. Lines within retention → write to a temp file
5. When complete: close the active log file handle in the logger, atomically rename temp → active, reopen
6. Archive files go to `archive_dir` (default `$log_dir/archive/`)

### Archive naming
`archive/api-payload-2026-02-23.jsonl.gz` — one archive file per rotation run, named by the date of the oldest entry in that batch (or the rotation date).

### Memory efficiency
- Stream line-by-line using `bufio.Scanner` (set buffer size to handle large lines, ~1MB max per line)
- Write to archive gzip writer and keep-file writer simultaneously
- Peak memory: one line buffer + gzip buffers. Never the whole file.
- If a log file has no lines older than retention, skip it entirely (check first line's timestamp first as a fast path)

### Logger reopen
The logger holds file handles open. Need a `Rotate()` or `Reopen()` method on the logger that:
1. Acquires the write lock
2. Closes current file handles
3. Opens fresh handles to the same paths
4. Releases the lock

The rotation goroutine calls this after swapping files.

### Config
```toml
[logging]
log_rotation = true           # enable/disable log rotation (default true)
rotation_period = "24h"       # how often to check and rotate
retention_period = "48h"      # keep this much in active log, archive the rest
archive_dir = ""              # default: $log_dir/archive/
```

### Edge cases
- First run with existing large log: will archive everything older than 2d on first rotation. This is correct.
- Empty log files: skip
- Log files that don't exist: skip
- Corrupt lines (no parseable timestamp): keep in active log (don't archive what you can't date)
- clod.log is not JSONL — parse timestamp differently (it uses standard log format with timestamps at line start)
- Rotation in progress when new log writes arrive: the logger lock prevents interleaving

### Startup
- Start a background goroutine on init that sleeps for `rotation_period`, then rotates, then sleeps again
- On first startup, check immediately if rotation is needed (last rotation time stored in state or checked by examining archive dir)

## Update docs
- docs/CONFIG.md — new [logging] fields
- SPEC.md — log rotation section

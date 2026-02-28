# Task: Rename "heartbeat" to "keepalive"

The heartbeat feature is confusingly named — it's a cache keepalive, not a heartbeat. Rename it throughout the codebase.

## Scope

Rename the heartbeat concept to "keepalive" everywhere:

- `HeartbeatConfig` → `KeepaliveConfig`
- `config.Heartbeat` → `config.Keepalive`
- `[heartbeat]` config section → `[keepalive]`
- `[agents.heartbeat]` → `[agents.keepalive]`
- Log messages: `log.Infof("heartbeat", ...)` → `log.Infof("keepalive", ...)`
- Branch type string: `"heartbeat"` → `"keepalive"`
- Variable/field names: `hbCfg`, `heartbeatRunning`, `heartbeatInterval`, `lastHeartbeat`, etc.
- The `heartbeat/` package directory → `keepalive/`
- Comments and doc references
- `docs/HEARTBEAT.md` → `docs/KEEPALIVE.md` (update content too)
- `docs/CONFIG.md` references
- `SPEC.md` references

## What NOT to rename

- The `heartbeat.go` Runner struct manages BOTH keepalive and background work. The Runner itself and the background work system stay as-is — only the "heartbeat" half gets renamed.
- Don't touch agent workspace files (prompts, memory) — those are outside the repo.

## Tests

- Rename test functions/variables to match
- All tests must pass: `go test ./... -timeout 120s`

## Docs

- Update SPEC.md, docs/CONFIG.md, docs/WIRING.md, docs/HEARTBEAT.md (rename file)
- Commit with descriptive message and push

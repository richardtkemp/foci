<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backends & storage internals

## Claude Code (CC / ccstream)

- **Cold-launch effort:** pass `claude --effort <level>` (`internal/delegator/backend.go` → ccstream). NOT via `--settings` — `--settings` carries the **hook command string** only; an `effortLevel` there is silently ignored. Mid-session effort changes go through the `apply_flag_settings` control (`internal/delegator/control.go`).
- **Idle timeout:** `DefaultIdleTimeout = 3h` (`internal/agent/delegated_manager.go`) — idle delegated backends are closed after this and resumed on the next turn. It's a per-agent backend config key (`idle_timeout`); unset → the const. This is SEPARATE from `streamIdleTimeout` (~24h), which is the silence-tolerance *within* a single turn stream, not session close. Don't conflate them.
- `--settings` also carries the PostToolUse hook that installs `foci-cc-hook`; `--resume <id>` continues a CC session across a foci restart.

## opencode

- **Model** lives in `~/.config/opencode/opencode.json` (global, `provider/model`); foci **ignores** `backend_config.model` for opencode.
- **The live `opencode serve` OpenAPI is the only authoritative contract** — vendored SDK types and cloned opencode source diverge by version. To confirm any route/event/payload, spin up a throwaway `opencode serve` and read ITS OpenAPI (see the `opencode-live-openapi` skill). Key endpoints (verify before relying):
  - compaction = `POST /session/:id/summarize {providerID, modelID, auto:false}` (NOT `/command compact`)
  - permissions = `permission.asked` event + `POST /permission/{id}/reply {reply: once|always|reject}`
  - context window = `GET /config/providers` → model `limit.context`
- **Session scoping:** `/session` is project/dir-scoped to the server's cwd — a server started from arnix lists only arnix's sessions. Migrate via `opencode export <id>` / `opencode import <file>` (import binds to the import-time cwd — run it from the session's real directory).

## SQLite

Set pragmas in the **DSN**, not via a post-open `Exec` — connection pooling means an `Exec`'d pragma sticks to one connection while others keep the default (e.g. `busy_timeout=0` → `SQLITE_BUSY`). Use:

```
file:PATH?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)
```

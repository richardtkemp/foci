<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backends & storage internals

## Claude Code (CC / ccstream)

- **Cold-launch effort:** pass `claude --effort <level>` (`internal/delegator/backend.go` → ccstream). NOT via `--settings`: an `effortLevel` there is silently ignored. Mid-session effort changes use the `apply_flag_settings` control (`internal/delegator/control.go`).
- `--settings` carries only the hook command string (the PostToolUse hook that installs `foci-cc-hook`). `--resume <id>` continues a CC session across a foci restart.
- **Two timeouts:** the idle timeout closes an idle delegated backend (the next turn resumes it): `[[agents]].backend_config.idle_timeout`, else `[sessions] idle_timeout`, else 3h (`delegated_manager.go`). The live-session caps (`max_live_sessions`, #2280) evict the longest-idle backend early. `streamIdleTimeout` (~24h) is the silence tolerance inside one turn stream.

## opencode

- **Model** comes from `~/.config/opencode/opencode.json` (global, `provider/model`). Foci **ignores** `backend_config.model` for opencode.
- **The live `opencode serve` OpenAPI is the only authoritative contract.** Vendored SDK types and cloned source diverge by version. To confirm a route/event/payload, start a throwaway `opencode serve` and read ITS OpenAPI (`opencode-live-openapi` skill). Key endpoints (verify before you rely on them):
  - compaction = `POST /session/:id/summarize {providerID, modelID, auto:false}` (NOT `/command compact`)
  - permissions = `permission.asked` event + `POST /permission/{id}/reply {reply: once|always|reject}`
  - context window = `GET /config/providers` → model `limit.context`
- **Session scoping:** `/session` is scoped to the server's cwd. A server started from arnix lists only arnix's sessions. Migrate with `opencode export <id>` / `opencode import <file>`; import binds to the import-time cwd, so run it from the session's real directory.

## SQLite

Set pragmas in the **DSN**, not with a post-open `Exec`: with connection pooling an `Exec`'d pragma applies to one connection only (e.g. others keep `busy_timeout=0` → `SQLITE_BUSY`).

```
file:PATH?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)
```

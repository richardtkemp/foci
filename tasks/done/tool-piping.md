# Task: Tool Piping — Shell Functions via Unix Socket

Read the full spec at `/home/foci/clutch/temp/pipeline-spec.md`.

## Summary

Expose selected foci tools as shell functions inside `exec` calls, allowing agents to compose tools with unix pipes in a single exec invocation. Uses a per-exec unix socket for secure, concurrent tool calls from shell.

## Implementation Order

### Phase 1: Core infrastructure
1. **`foci-call` binary** — small Go program in `cmd/foci-call/`. Reads `FOCI_SOCK` env var, connects to unix socket, sends JSON request (first arg), prints result to stdout or error to stderr, exits 0/1. Build in `setup.sh` alongside `focigw` and `foci`.
2. **Unix socket listener in exec tool handler** — when exec spawns a subprocess, create a unix socket at `/tmp/foci-exec-<pid>.sock` (0600 perms). Accept connections in a goroutine. Each connection: read one line of JSON, dispatch to tool handler, write one line of JSON response, close. Tear down socket when exec exits.
3. **Inject `FOCI_SOCK` env var** into exec subprocess environment.
4. **`set -o pipefail`** — prepend to all exec commands.

### Phase 2: Tool export framework
1. **`ExecExport` bool field** on tool definitions (the `Tool` struct in `tools/`).
2. **Shell function generator** — for each tool with `ExecExport: true`, generate a bash function `foci_<toolname>` that builds a JSON request and calls `foci-call`. Write to a temp file, source it at exec startup (prepend `source /tmp/foci-exec-<pid>-funcs.sh` to exec command).
3. **Socket request router** — parse `{"tool": "...", "params": {...}}` from socket, find the matching tool handler, invoke it with the agent's context, return `{"ok": true/false, "result"/"error": "..."}`.

### Phase 3: Export specific tools
Mark these tools with `ExecExport: true` and ensure their handlers work when invoked from the socket bridge:
- `http_request` — secret resolution must use the calling agent's allowed secrets
- `web_fetch`
- `web_search`
- `memory_search`
- `todo`
- `send_telegram`
- `spawn`

## Key Design Decisions

- **One socket per exec** — path includes PID, perms 0600. Agent identity is implicit (foci knows which agent spawned which exec).
- **One connection per tool call** — connect, request, response, disconnect. Naturally supports concurrent calls from background subshells.
- **No socat dependency** — `foci-call` binary handles all socket communication.
- **Shell functions, not Python** — bash only.
- **Positional primary arg, flags for rest** — e.g. `foci_http_request "https://..." --method POST`
- **`send_telegram` reads stdin** if no positional arg given.
- **Errors** — `foci-call` prints to stderr and exits 1 on failure. Combined with pipefail, errors propagate.

## Testing

- Unit test the socket listener (mock tool handler, verify request/response cycle)
- Unit test `foci-call` (connect to test socket, send request, verify output)
- Unit test shell function generation (verify correct bash for each exported tool)
- Integration test: exec with `foci_http_request` calling a mock HTTP endpoint
- Test concurrent calls from background subshells
- Test error propagation with pipefail
- All existing tests must pass: `go test ./... -timeout 120s`

## Docs

- Update SPEC.md with tool piping section
- Update docs/WIRING.md with socket bridge architecture
- Update docs/CONFIG.md if any config is needed
- Add docs/TOOL-PIPING.md with usage examples
- Commit with descriptive message and push

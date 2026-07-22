# Tools

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery. Each tool has a name, description, JSON Schema parameters, and an `Execute` function.

## Basic Tools

| Tool | Description |
|------|-------------|
| `read` | Read file contents with line numbers (truncates at 2000 lines). PDFs are returned as native document content blocks (base64-encoded, ≤32MB). |
| `write` | Create or overwrite files. |
| `edit` | Find-and-replace in files. `old_string` must be unique. Syntax validation for `.json`, `.toml`, `.go`, `.yaml`/`.yml`, `.xml`, `.py`, `.sh`/`.bash` — rejects edits that would break a valid file. |
| `shell` | Run shell commands via `sh -c` with process group kill on timeout. Output redacted for secrets. Supports `background: true` for daemons and auto-background for long-running commands. Regular `{{secret:}}` templates are blocked (use `http_request`); Bitwarden `{{secret:bw.*}}` templates are allowed (approval-gated). |
| `web_search` | Search the web. Default: Anthropic server-side tool (`search_provider = "anthropic"`). Fallback: Brave Search API (`search_provider = "brave"`). |
| `web_fetch` | Fetch web page content. Two providers: `"builtin"` (default) — client-side HTTP GET with readability extraction, goes through tool result guard and auto-summarise. `"anthropic"` — server-side fetch, bypasses guard/summarise. Set via `fetch_provider` in config. |
| `memory_search` | FTS5 full-text search over memory files + conversation history (porter stemming, memory weighted 2x, sort by relevance or recency). |
| `todo` | Per-agent task list — add, list, complete, remove, search. SQLite backend with priority ordering (high/medium/low) and status tracking (open/started/done/dropped). FTS5 full-text search with porter stemming (e.g. "run" matches "running"). List excludes done/dropped by default; use `status: "all"` to include them. Tag support for filtering. Items tagged `background` are automatically picked up and worked on in background branch sessions when the user is idle and background work is allowed — see [HEARTBEAT.md](HEARTBEAT.md). |
| `remind` | Defer a thought for later (delay, tomorrow, specific date/time). Stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session. |
| `scratchpad` | Working notes that survive compaction — write, read, clear, list via `action` parameter. |
| `send_to_chat` | Send proactive Telegram messages and media. `send_as` controls file type: document (default), voice, video, photo, audio, animation. With `send_as="voice"` and text (no file), synthesizes speech via TTS. |
| `send_to_session` | Inject a message into another session for cross-session communication. `reply_to` param controls where the response goes: `"caller"` (default) or `"session"`. |
| `bitwarden_search` | Search Bitwarden vault items by name/URI/folder/username (metadata only, no passwords). Only available when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | Unlock a vault item by ID — requires admin approval via aisudo/Telegram. Caches value for `secret_ttl`. Never returns the actual password. |
| `app_android` | Invoke a tool on the user's connected Android device (via Tasker). `action="list"` returns the device's allowlisted tasks; `action="perform"` runs one. Returns an error string if no device is connected. Gated on `[[platforms]] id="app"` being configured. The on-device allowlist ships empty by default — the user edits `TaskerAllowlist` in the foci-android source to expose tasks. v1: a "pending" result (Tasker didn't finish in the device's 10s sync window) is final — late completion is dropped, async injection lands later. |
| `mcp` | Call a tool on a connected MCP server. Re-reads `mcp.toml` on each call — servers can be added/removed without restarting. Only registered when `mcp.toml` exists or `configDir` is set. See [CONFIG.md](CONFIG.md#mcptoml) for configuration. |
| `task_list` | Manage task items. Distinct from `todo`: `task_list` is its own subsystem for tracking task work in progress (see also [HEARTBEAT.md](HEARTBEAT.md)). |
| `set_session_alias` (foci_set_session_alias) | Set a short descriptive name for the current conversation. The alias is surfaced in session listings and UI surfaces. Disabled for the Codex backend, which manages session naming server-side. |
| `browser` | Full CDP browser automation via [go-rod](https://github.com/go-rod/rod). Only registered when `[browser] enabled = true`. Provides navigation, DOM interaction, screenshots, and script evaluation against a real Chromium instance. |

## `ask` — Human-in-the-loop questioning

`foci_ask` is a first-class tool exported as `foci_ask` for interactive human-in-the-loop questioning with selectable button options. It is the primary mechanism for getting a decision from the user mid-turn without forcing the model to guess.

- **Async by design.** Posting an ask ends the current turn; the answer arrives later as a new user message routed back into the same session. The model does not block waiting.
- **Persists across restarts (24h TTL).** Asks are stored durably, so a restart mid-question does not lose the pending prompt.
- **Batched asks survive restart.** Multiple outstanding asks are tracked together; restarting does not drop any pending answer.
- **Optional grader subprocess.** When `grader` is configured, the answer is fed to an external program that decides accept/reject. Keys: `grader` (binary), `grader_args`, `grader_timeout_seconds`, `grader_on_error` (controls behaviour when the grader itself fails or times out).
- **Button clicks always work.** Selecting a button option submits the answer directly to the originating ask.
- **Typed replies route to the ask when the session is idle** — so free-text answers reach the right outstanding question without disambiguation prompts.
- **Capture control.** `/pause`, `/resume`, and `/complete` commands start, stop, and finalize answer capture for an ask.

See [ASKGW.md](ASKGW.md) and [ASKGW-PROTOCOL.md](ASKGW-PROTOCOL.md) for the wire protocol and gateway details.

## Complex Tools

### `http_request` — Domain-locked HTTP

Secure HTTP requests with secret template support. Secrets in headers/body are validated against per-section `allowed_hosts` before sending. See [SECRETS.md](SECRETS.md) for `{{secret:NAME}}` template syntax, domain locking, and the security model.

Features:
- **Cross-domain redirect blocking** when secrets are present
- **Response redaction** — secret values in response bodies replaced with `[REDACTED]`
- **`save_to`** — save response body to a specific file path (returns status + headers + path, not body)
- **`save_from_json_path`** — extract a value from JSON response by dot path; decodes `data:` URIs to binary. Designed for image generation APIs.
- **`body_file`** — read request body from a local file instead of inline `body`. Solves large payload problems (e.g. base64 audio).
- **`files`** — multipart/form-data file uploads with `form_fields` for additional text fields
- **Binary auto-save** — `image/*`, `audio/*`, `video/*` responses auto-save to temp file
- **Auto-background** — long requests auto-background and deliver results asynchronously

### `tmux` — Session lifecycle management

Manage tmux sessions with built-in monitoring:

- **`start`** — create a tmux session and run a command. Auto-watches by default.
- **`send`** — send keystrokes to a pane. Auto-watches after send (autopilot mode).
- **`read`** — read pane output (last N lines).
- **`list`** — list active sessions.
- **`kill`** — kill a session.
- **`watch`** — monitor a pane for inactivity. Fires when content unchanged for `threshold_seconds` (default 30s). Content tracked via MD5 hash. One-shot alert, persists across restarts.
- **`unwatch`** — stop monitoring a session.

Autopilot mode (default on): auto-unwatches after inactivity notification, auto-watches on send — removes manual watch/unwatch overhead.

Owned sessions persist across app restarts via the state store.

### `summary` — Cheap-group extraction

Summarize or extract specific information from a file via a side-call to the `cheap` model group (which defaults to the `powerful` model when `cheap` is unset) without loading the file into conversation context. Useful for large files where only specific data is needed — the full content never enters the agent's context window.

### `spawn` — Sub-calls with context modes

Unified sub-call to a model with four context modes, all with tool access:

| Mode | System prompt | Tools | Behaviour |
|------|--------------|-------|-----------|
| `raw` | None | Most (no `send_to_chat`, `send_to_session`) | One-shot. No character context means no communication awareness. |
| `character` | Character files only | All | One-shot with identity. |
| `clone` (default) | Full clone | All | Branch session — a headless self-fork. Runs async, delivers result on completion. |
| `explore` | Code explorer | Read-only allowlist (see below) | One-shot. Safe exploration — no file mutation, no shell exec, no messaging. Uses the `cheap` model group (which defaults to the `powerful` model when `cheap` is unset). |

`clone` creates a branch `{parentKey}/b{TIMESTAMP}`, runs via `AsyncNotifier`, and returns an immediate ack. Recursive `clone` is blocked. Concurrent spawns limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion.

**`explore` allowlist.** The read-only tool set for `explore` is broader than just the search/read primitives: `git` and `todo` are always available, and the following are enabled conditionally (based on config/availability): `file`, `stat`, `wc`, `head`, `tail`, `tree`, `du`, `jq`, `yq`, `mdq`, `docker`, `systemctl`, `sqlite3`, `crontab`, `id`. None of these can mutate the workspace — they are inspection-only commands wrapped as tools.

## Slash Commands as Tools

All registered slash commands are automatically exposed to the agent as tools with the same name (without the `/` prefix). The agent can invoke any command programmatically — each accepts an optional `args` string parameter. See [COMMANDS.md](COMMANDS.md) for the full command reference.

## Tool Piping (Exec Bridge)

Tool piping exposes foci tools as shell functions inside `shell` commands. Instead of chaining tool calls through the model (one inference pass per step), you can compose tools with unix pipes in a single shell invocation. Intermediate data never enters context.

### Architecture

```
exec subprocess                       foci process
┌─────────────────────┐               ┌───────────────┐
│ foci_http_request ──┼──connect────▶ │ goroutine/conn │
│ foci_web_fetch    ──┼──connect────▶ │ goroutine/conn │
│ foci_spawn        ──┼──connect────▶ │ goroutine/conn │
└─────────────────────┘               └───────────────┘
    <tempdir>/exec-<pid>-<n>.sock
```

Each shell call creates a per-shell unix socket (0600 perms) at `<tempdir>/exec-<pid>-<n>.sock` (no `foci-` prefix — the system temp directory is used as-is). The `foci-call` binary connects, sends a JSON request, and prints the result. Shell wrapper functions provide ergonomic interfaces on top of `foci-call`.

### Available Functions

#### `foci_web_search <query>`
Search the web. All arguments become the query string.
```bash
foci_web_search "golang error handling best practices"
```

#### `foci_web_fetch <url> [--raw]`
Fetch a URL and return content as markdown (or raw HTML with `--raw`).
```bash
foci_web_fetch https://example.com/api-docs
foci_web_fetch https://example.com/page --raw
```

#### `foci_http_request <url> [--method M] [--header 'K: V'] [--body B] [--save-to P]`
Make an HTTP request with full control over method, headers, and body.
```bash
foci_http_request https://api.example.com/data
foci_http_request https://api.example.com/items --method POST --body '{"name":"test"}'
foci_http_request https://api.example.com/file --save-to /tmp/output.json
foci_http_request https://api.example.com/auth --header 'Authorization: Bearer token123'
```

#### `foci_memory_search <query>`
Search memory files and conversation history.
```bash
foci_memory_search "database migration"
```

#### `foci_send_to_chat <text> [--file -] [--filename NAME]`
Send a Telegram message. Reads message text from stdin when no arguments (pipe-friendly). To send piped output as a *document attachment* (rather than as message text), pass `--file -`: this reads the attachment body from stdin into a temp file, so no temp file is needed on disk. Pair with `--filename` to set the display name.
```bash
foci_send_to_chat "Build completed successfully"
echo "Pipeline results: all green" | foci_send_to_chat
git diff | foci_send_to_chat "diff for review" --file - --filename review.diff
```

#### `foci_todo <action> [args...]`
Manage the todo list. `list` shows active items only (excludes done/dropped); use `list-all` or `list --status all` to see everything.
```bash
foci_todo add "Review PR #42"
foci_todo list                    # active items only
foci_todo list --status all       # include done/dropped
foci_todo list-all                # shorthand for --status all
foci_todo complete 3
foci_todo search "review"
foci_todo remove 5
```

#### `foci_spawn <prompt> [--model M] [--context C]`
Spawn a sub-call to a model.
```bash
foci_spawn "Summarize this data" --model haiku --context none
```

### Composition Examples

Search the web and send the top results via Telegram:
```bash
foci_web_search "latest golang release" | head -5 | foci_send_to_chat
```

Fetch an API, filter with jq, and send a notification:
```bash
foci_http_request https://api.github.com/repos/golang/go/releases/latest \
  | jq -r '.tag_name + ": " + .name' \
  | foci_send_to_chat
```

Search memory for context, then ask a model to summarize:
```bash
context=$(foci_memory_search "deployment checklist")
foci_spawn "Summarize this: $context" --model haiku --context none
```

Fetch a page and save processed output:
```bash
foci_web_fetch https://example.com/docs | grep -i "api" > /tmp/api-notes.txt
```

### Dependencies

- **jq** — used by shell functions for safe JSON construction (avoids injection from special characters in URLs/text)
- **foci-call** — small Go binary installed to `/usr/local/bin` by `setup.sh`

### How It Works Internally

1. When `shell` runs a command (non-background mode), it creates an `ExecBridge`
2. The bridge opens a unix socket at `<tempdir>/exec-<pid>-<n>.sock`
3. A shell functions file is generated with `foci_<toolname>()` for each tool with `ExecExport: true`
4. The command is wrapped: `set -o pipefail; source <funcs.sh>; <original command>`
5. `FOCI_SOCK` environment variable is set so `foci-call` knows where to connect
6. Each function call connects to the socket, sends a JSON request, and returns the result
7. After the command exits, the bridge is closed and socket/funcs files are cleaned up

### Limitations

- **Background mode:** Tool piping is not available in `background: true` shell calls (daemon mode)
- **Large responses:** 1MB scanner buffer limit (tools already truncate output)
- **jq dependency:** Functions fail with "command not found" if jq is not installed
- **foci-call not in PATH:** Functions fail if the binary is not installed

# Tools

Tools are Go functions registered at compile time. No dynamic loading, no plugin discovery. Each tool has a name, description, JSON Schema parameters, and an `Execute` function.

## Basic Tools

| Tool | Description |
|------|-------------|
| `read` | Read file contents with line numbers (truncates at 2000 lines). |
| `write` | Create or overwrite files. |
| `edit` | Find-and-replace in files. `old_string` must be unique. Syntax validation for `.json`, `.toml`, `.go`, `.yaml`/`.yml`, `.xml`, `.py`, `.sh`/`.bash` — rejects edits that would break a valid file. |
| `exec` | Run shell commands via `sh -c` with process group kill on timeout. Output redacted for secrets. Supports `background: true` for daemons and auto-background for long-running commands. Regular `{{secret:}}` templates are blocked (use `http_request`); Bitwarden `{{secret:bw.*}}` templates are allowed (approval-gated). |
| `web_search` | Search the web via Brave Search API. |
| `web_fetch` | HTTP GET with readability extraction + markdown conversion. Raw HTML mode available. |
| `memory_search` | FTS5 full-text search over memory files + conversation history (porter stemming, memory weighted 2x, sort by relevance or recency). |
| `todo` | Per-agent task list — add, list, complete, remove, search. SQLite backend with priority ordering (high/medium/low). Tag support for background work filtering. |
| `remind` | Defer a thought for later (delay, tomorrow, specific date/time). Stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session. |
| `scratchpad` | Working notes that survive compaction — write, read, clear, list via `action` parameter. |
| `send_telegram` | Send proactive Telegram messages and media. `send_as` controls file type: document (default), voice, video, photo, audio, animation. With `send_as="voice"` and text (no file), synthesizes speech via TTS. |
| `send_to_session` | Inject a message into another session for cross-session communication. `reply_to` param controls where the response goes: `"caller"` (default) or `"session"`. |
| `bitwarden_search` | Search Bitwarden vault items by name/URI/folder/username (metadata only, no passwords). Only available when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | Unlock a vault item by ID — requires admin approval via aisudo/Telegram. Caches value for `secret_ttl`. Never returns the actual password. |

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
- **`send`** — send keystrokes to a pane. Auto-watches after send (braindead mode).
- **`read`** — read pane output (last N lines).
- **`list`** — list active sessions.
- **`kill`** — kill a session.
- **`watch`** — monitor a pane for inactivity. Fires when content unchanged for `threshold_seconds` (default 30s). Content tracked via MD5 hash. One-shot alert, persists across restarts.
- **`unwatch`** — stop monitoring a session.

Braindead mode (default on): auto-unwatches after inactivity notification, auto-watches on send — removes manual watch/unwatch overhead.

Owned sessions persist across app restarts via the state store.

### `summary` — Haiku-powered extraction

Summarize or extract specific information from a file via a Haiku side-call without loading the file into conversation context. Useful for large files where only specific data is needed — the full content never enters the agent's context window.

### `spawn` — Sub-calls with context modes

Unified sub-call to a model with three context modes, all with tool access:

| Mode | System prompt | Tools | Behaviour |
|------|--------------|-------|-----------|
| `none` | None | Most (no `send_telegram`, `send_to_session`) | One-shot. No character context means no communication awareness. |
| `character_only` | Character files only | All | One-shot with identity. |
| `clone_current` (default) | Full clone | All | Branch session — a headless self-fork. Runs async, delivers result on completion. |

`clone_current` creates a branch `agent:ID:spawn:spawn-TIMESTAMP`, runs via `AsyncNotifier`, and returns an immediate ack. Recursive `clone_current` is blocked. Concurrent spawns limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion.

## Slash Commands as Tools

All registered slash commands are automatically exposed to the agent as tools with the same name (without the `/` prefix). The agent can invoke any command programmatically — each accepts an optional `args` string parameter. See [COMMANDS.md](COMMANDS.md) for the full command reference.

## Tool Piping (Exec Bridge)

Tool piping exposes foci tools as shell functions inside `exec` commands. Instead of chaining tool calls through the model (one inference pass per step), you can compose tools with unix pipes in a single exec invocation. Intermediate data never enters context.

### Architecture

```
exec subprocess                       foci process
┌─────────────────────┐               ┌───────────────┐
│ foci_http_request ──┼──connect────▶ │ goroutine/conn │
│ foci_web_fetch    ──┼──connect────▶ │ goroutine/conn │
│ foci_spawn        ──┼──connect────▶ │ goroutine/conn │
└─────────────────────┘               └───────────────┘
    /tmp/foci-exec-<pid>-<n>.sock
```

Each exec call creates a per-exec unix socket (0600 perms). The `foci-call` binary connects, sends a JSON request, and prints the result. Shell wrapper functions provide ergonomic interfaces on top of `foci-call`.

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

#### `foci_send_telegram <text>`
Send a Telegram message. Reads from stdin when no arguments (pipe-friendly).
```bash
foci_send_telegram "Build completed successfully"
echo "Pipeline results: all green" | foci_send_telegram
```

#### `foci_todo <action> [args...]`
Manage the todo list.
```bash
foci_todo add "Review PR #42"
foci_todo list
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
foci_web_search "latest golang release" | head -5 | foci_send_telegram
```

Fetch an API, filter with jq, and send a notification:
```bash
foci_http_request https://api.github.com/repos/golang/go/releases/latest \
  | jq -r '.tag_name + ": " + .name' \
  | foci_send_telegram
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

1. When `exec` runs a command (non-background mode), it creates an `ExecBridge`
2. The bridge opens a unix socket at `/tmp/foci-exec-<pid>-<n>.sock`
3. A shell functions file is generated with `foci_<toolname>()` for each tool with `ExecExport: true`
4. The command is wrapped: `set -o pipefail; source <funcs.sh>; <original command>`
5. `FOCI_SOCK` environment variable is set so `foci-call` knows where to connect
6. Each function call connects to the socket, sends a JSON request, and returns the result
7. After the command exits, the bridge is closed and socket/funcs files are cleaned up

### Limitations

- **Background mode:** Tool piping is not available in `background: true` exec calls (daemon mode)
- **Large responses:** 1MB scanner buffer limit (tools already truncate output)
- **jq dependency:** Functions fail with "command not found" if jq is not installed
- **foci-call not in PATH:** Functions fail if the binary is not installed

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
| `remind` | Defer a thought for later (delay, tomorrow, specific date/time). Stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session; `list=true` shows pending wakes with their ids and state (`pending`, or `held`/`fired` once the timer has fired — "fired, held behind background work" while background work delays the wake, "fired, waiting to run" otherwise); `cancel=<id>` cancels one (a fired wake is NOT cancellable — the error says whether it is held behind background work or merely waiting to run). |
| `scratchpad` | Working notes that survive compaction — write, read, clear, list via `action` parameter. |
| `send_to_chat` | Send proactive Telegram messages and media. `send_as` controls file type: document (default), voice, video, photo, audio, animation. With `send_as="voice"` and text (no file), synthesizes speech via TTS. |
| `send_to_session` | Inject a message into another session for cross-session communication. `reply_to` param controls where the response goes: `"caller"` (default) or `"session"`. On the app, the receiving chat shows your message as a tap-to-reveal chit (and the asking chat shows the reply, for `reply_to=caller`) — chits appear only in chats actually open with that agent. |
| `bitwarden_search` | Search Bitwarden vault items by name/URI/folder/username (metadata only, no passwords). Only available when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | Unlock a vault item by ID — requires admin approval via aisudo/Telegram. Caches value for `secret_ttl`. Never returns the actual password. |
| `app_android` | Invoke a tool on the user's connected Android device (via Tasker). `action="list"` returns the device's allowlisted tasks; `action="perform"` runs one. Runs on the most recently connected device that can run it (advertises `tool:android`, or is an older Android build); the desktop app is never picked. Returns an error string if no such device is connected. Gated on `[[platforms]] id="app"` being configured. The on-device allowlist ships empty by default — the user edits `TaskerAllowlist` in the foci-android source to expose tasks. v1: a "pending" result (Tasker didn't finish in the device's 10s sync window) is final — late completion is dropped, async injection lands later. |
| `mcp` | Call a tool on a connected MCP server. Re-reads `mcp.toml` on each call — servers can be added/removed without restarting. Only registered when `mcp.toml` exists or `configDir` is set. See [CONFIG.md](CONFIG.md#mcptoml) for configuration. |
| `task_list` | Manage task items. Distinct from `todo`: `task_list` is its own subsystem for tracking task work in progress (see also [HEARTBEAT.md](HEARTBEAT.md)). |
| `set_session_alias` (foci_set_session_alias) | Set a short descriptive name for the current conversation. The alias is surfaced in session listings and UI surfaces. Disabled for the Codex backend, which manages session naming server-side. |
| `browser` (foci_browser) | Full CDP browser automation via [go-rod](https://github.com/go-rod/rod). Only registered when `[browser] enabled = true`. Provides navigation, DOM interaction, screenshots, and script evaluation against a real Chromium instance. Available on the API loop and, as `foci_browser`, on every delegated backend. Each session gets its own browser (never shared across the main chat, forks, branches or other chats); one idle for 30 minutes is stopped. Only one session at a time can open the persistent `user_data_dir` profile. |
| `close_session` (foci_close_session) | Close the app chat the agent is running in, exactly as if the user closed it: the chat leaves the open tabs on every device, its history stays, and the user's next message reopens it. Optional `mark_read` (bool, default false, #2592): also mark the chat read on every device through the end of this turn, final reply included. Gated on `[[platforms]] id="app"` being configured; on a non-app session it reports there is nothing to close. |
| `whoami` (foci_whoami) | Read-only report of the calling session's identity: agent id, session key, chat id, platform, backend, transport (`api` or `delegated`), model. Available on the API loop and every delegated backend; `--json` for machine use. Missing values are reported as `none`/`unknown`, never guessed — e.g. the chat id `send_to_session` addressing needs. |

## `ask` — Human-in-the-loop questioning

`foci_ask` is a first-class tool exported as `foci_ask` for interactive human-in-the-loop questioning with selectable button options. It is the primary mechanism for getting a decision from the user mid-turn without forcing the model to guess.

- **Async by design.** Posting an ask ends the current turn; the answer arrives later as a new user message routed back into the same session. The model does not block waiting.
- **Persists across restarts (24h TTL).** Asks are stored durably, so a restart mid-question does not lose the pending prompt.
- **Batched asks survive restart.** Multiple outstanding asks are tracked together; restarting does not drop any pending answer.
- **Optional grader subprocess.** When `grader` is configured, the answer is fed to an external program that decides accept/reject. Keys: `grader` (binary), `grader_args`, `grader_timeout_seconds`, `grader_on_error` (controls behaviour when the grader itself fails or times out).
- **Button clicks always work.** Selecting a button option submits the answer directly to the originating ask.
- **Typed replies route to the ask when the session is idle** — so free-text answers reach the right outstanding question without disambiguation prompts.
- **Quiz mode (#1316).** Mark any question with `quiz` (`correct`: 0-based option indexes, or `answer` for a free-recall question with no options; optional `explanation`) and the user's client hides the answer until they reveal it. On the app the whole quiz rides the interactive form; on chat platforms the options are numbered and a typed reply that is just a number selects that option, with one reveal message after each answer. The answer batch carries a per-question true/false `quiz_results` verdict for quiz option questions; free recall is the agent's to judge.
- **Multi-select (#2308).** `multiSelect: true` on a question lets the user tick several options (plus optional typed text) in the foci app. The answer line lists every ticked label and the JSON payload adds `selections` (question text → `{selected, other, single_select_fallback}`). Telegram/Discord show the options as ordinary single-pick buttons; that answer is flagged `single_select_fallback`. On a multi-select quiz question the verdict is correct only for exactly the correct set.
- **Capture control.** `/pause`, `/resume`, and `/complete` commands start, stop, and finalize answer capture for an ask.

See [ASKGW.md](ASKGW.md) and [ASKGW-PROTOCOL.md](ASKGW-PROTOCOL.md) for the wire protocol and gateway details.

## Complex Tools

### `http_request` — Domain-locked HTTP

Secure HTTP requests with secret template support. Secrets in headers/body are validated against per-section `allowed_hosts` before sending. See [SECRETS.md](SECRETS.md) for `{{secret:NAME}}` template syntax, domain locking, and the security model.

Features:
- **`basic_auth`** — HTTP Basic credentials as `user:password` (`--basic-auth` in the shell function). `{{secret:NAME}}` templates resolve first, then the pair is base64-encoded into `Authorization: Basic …`, e.g. `'{{secret:companies_house.api_key}}:'` for a key-as-username API. Same allowed_hosts/https checks as a header secret; mutually exclusive with an `Authorization` header. When it held a secret, the encoded token is redacted from the response too.
- **Cross-domain redirect blocking** when secrets are present
- **Response redaction** — secret values in response bodies replaced with `[REDACTED]`
- **`save_to`** — save response body to a specific file path (the result is `Saved N bytes to <path>`, not the body)
- **`include_headers`** — prefix the result with the `HTTP <status>` line and every response header (default off: body only, so output pipes cleanly)
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
| `character` (default) | The agent's full system prompt | All except `send_to_session`, `whoami`, `close_session`, `ask` (nothing that needs the user) | Returns its result directly; with `background=true` runs in the background on a fresh branch (none of the conversation) and is steered, checked on and delivered like a clone. |
| `clone` | Full clone | All | Branch session — a headless self-fork. Always runs in the background, delivers result on completion. Only when the task needs so much of the conversation that it cannot be written into a prompt. |
| `raw` | None | Most (no `send_to_chat`, `send_to_session`, `whoami`) | One-shot. The caller's prompt is the only prompt. |
| `explore` | Code explorer | Read-only allowlist (see below) | One-shot. Safe exploration — no file mutation, no shell exec, no messaging. Uses the `cheap` model group (which defaults to the `powerful` model when `cheap` is unset). |

On a **delegated agent** (Claude Code, codex, opencode) the one-shot modes run as a batch on the agent's own backend (`DelegatedManager.RunBatch`), never through model groups: `model` is passed to the backend as written (empty = the agent's own model). Each mode is offered only where the backend can run it safely:

| Mode | Delegated behaviour | Claude Code | codex | opencode |
|------|--------------------|:-:|:-:|:-:|
| `raw` | The caller's prompt is also the system prompt, replacing the backend's default (Claude Code rejects a blank one, and still prepends its one-line SDK identity); file tools confined to a fresh temp dir, no shell, no MCP, no foci tools; created files listed | ✓ | ✗ (sandbox cannot confine reads) | ✗ (one server per agent, rooted in its workspace) |
| `character` | The agent's full system prompt, tools, permission mode and allow rules, launched as its sessions are (and in its spawner's permission mode: in auto mode the classifier decides); an ask the rules do not settle goes up to the spawner's chat (#2684, see below); no `foci_ask`, `foci_close_session` or spawn tools. `--background` runs it on a fresh branch, launched the same way | ✓ | ✓ | ✓ |
| `clone` | Background fork (below) | ✓ | ✓ | ✓ |
| `explore` | Explorer prompt; reads and searches files and the web, writes nothing, runs nothing mutating (Claude Code: Read/Glob/Grep/WebSearch/WebFetch only; codex: read-only sandbox; opencode: enforced read-only ruleset) | ✓ | ✓ | ✓ |

**Spawn permissions (#2684, Dick 2026-10-30: "Spawns (as they are always from interactive sessions) should not skip permissions, they should report their permission requests up to the spawner").** No spawn is a skip-permissions session. A permission request (or question) from any spawn — every mode, direct or background, nested under depth leaves — that the auto-approve rules do not settle is shown in its spawner's chat, headed `🐣 From spawn <id>: <task>`, and does not block the spawner's turn; the answer goes back to the spawn's waiting call. A spawn starts in its spawner's permission mode: if that is auto mode, the classifier decides and nobody is asked. Unattended as before (rules, else deny, logged `unattended: session …`): a spawn started by an unattended turn (keepalive, reflection, a webhook, …), a cross-agent spawn, and a spawn whose spawner's app chat is closed (`trigger=spawner_chat_closed`). A spawn that ends while a request waits takes it down from the chat. Raw and explore spawns keep their restricted tool sets either way. CC's own deny rules (`permissions.deny` in its settings) still refuse a call before any ask exists.

`clone` creates a branch `{parentKey}/b{TIMESTAMP}`, runs via `AsyncNotifier`, and returns an immediate ack naming its spawn id; a background character spawn does the same on a fresh branch (#2643). Timeouts: 120 s default for a spawn that returns directly, 3600 s for one in the background, at most 86400 s (clamped). Delegated agents get `spawn` with the modes their backend supports (`foci_spawn <prompt> [--context MODE] [--background] [--model NAME] [--timeout S]`, #2523); `clone` needs a backend that can fork a session. `spawn_send` (steer a running background spawn, or resume a finished one on its branch) and `spawn_status` (list, or one spawn's last N activity entries) work on both paths. A spawn reports ONE `[SPAWN RESULT]` per run; messages its turn did not take run as a follow-up turn first. A background spawn cannot start another (clone or background character). `spawn_stop` stops a running spawn of any mode by id, or `all` (#2656); a spawn that returns directly is tracked while its call waits, so `spawn_status` lists it and `spawn_stop`/`/stop` end it, and it stops when its call ends without its result (the exec bridge cancels a call whose `foci-call` is gone). `agent=<id>` (`--agent`) runs the spawn AS another agent (#2662): its character, tools, workspace, permission rules, backend and model; naming an agent implies character mode (an explicit other mode is refused); any agent may spawn any agent; not from inside a spawn. Its `send_to_chat` goes to the caller's chat; its `send_to_session` and `remind` are refused. `send_to_spawner <message>` (every spawn, #2662 round 3): a spawn messages the session that spawned it while it runs (the caller's, for a cross-agent spawn). A background spawn's message arrives at once as a turn of its own (answer with `spawn_send`); a direct spawn's spawner is blocked in its call and cannot read it, so it is kept and heads the result, unless the spawner's session has no turn running, when it goes at once. A Claude Code raw or explore spawn has no shell and so cannot send one. Every spawn first checks the plan allowance of the backend it runs on and is refused when the 5-hour or weekly window has less than `[tools] spawn_reserve_percent` (default 10) left, unless `use_reserve=true` (`--use-reserve`); no usage source, a failed read or a read still running lets it run (the latter two say so). Usage is read on a schedule, once per account every 4-14 min, so the check normally costs nothing. Concurrent spawns limited by `max_concurrent_spawns` (default 3). `spawn` itself is excluded from one-shot tool sets to prevent recursion. The one-shot modes (`raw`, `character`) also exclude `whoami`: their tool calls run on the parent's context, so it would report the parent's session key, chat and model.

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

#### `foci_http_request <url> [--method M] [--header 'K: V'] [--body B] [--save-to P] [--include-headers]`
Make an HTTP request with full control over method, headers, and body.
```bash
foci_http_request https://api.example.com/data
foci_http_request https://api.example.com/items --method POST --body '{"name":"test"}'
foci_http_request https://api.example.com/file --save-to /tmp/output.json
foci_http_request https://api.example.com/auth --header 'Authorization: Bearer token123'
foci_http_request https://api.example.com/health --include-headers   # status line + response headers before the body
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

#### `foci_spawn <prompt> [--model M] [--context C] [--background]`
Spawn a sub-call to a model (default context `character`).
```bash
foci_spawn "Summarize this data" --model haiku --context raw
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
foci_spawn "Summarize this: $context" --model haiku --context raw
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

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
| `web_fetch` | Fetch web page content. Two providers: `"builtin"` (default) — client-side HTTP GET with readability extraction, goes through tool result guard and auto-summarise; reads at most `web_fetch_max_bytes` of the response and notes in the result when it cut one. `"anthropic"` — server-side fetch, bypasses guard/summarise. Set via `fetch_provider` in config. |
| `memory_search` | FTS5 full-text search over memory files + conversation history (porter stemming, memory weighted 2x, sort by relevance or recency). |
| `todo` | Per-agent task list — add, list, complete, remove, search. SQLite backend with priority ordering (high/medium/low) and status tracking (open/started/done/dropped). FTS5 full-text search with porter stemming (e.g. "run" matches "running"). List excludes done/dropped by default; use `status: "all"` to include them. Tag support for filtering. Items tagged `background` are automatically picked up and worked on in background branch sessions when the user is idle and background work is allowed — see [HEARTBEAT.md](HEARTBEAT.md). |
| `remind` | Defer a thought for later (delay, tomorrow, specific date/time). Stored in SQLite, surfaced as injected context when due. `wake=true` actively wakes the session; `list=true` shows pending wakes with their ids and state (`pending`, or `held`/`fired` once the timer has fired — "fired, held behind background work" while background work delays the wake, "fired, waiting to run" otherwise); `cancel=<id>` cancels one (a fired wake is NOT cancellable — the error says whether it is held behind background work or merely waiting to run). |
| `scratchpad` | Working notes that survive compaction — write, read, clear, list via `action` parameter. |
| `send_to_chat` | Send proactive Telegram messages and media. `send_as` controls file type: document (default), voice, video, photo, audio, animation. With `send_as="voice"` and text (no file), synthesizes speech via TTS. |
| `send_to_session` | Inject a message into another session for cross-session communication. `reply_to` param controls where the response goes: `"caller"` (default) or `"session"`. On the app, the receiving chat shows your message as a tap-to-reveal chit (and the asking chat shows the reply, for `reply_to=caller`) — chits appear only in chats actually open with that agent. A session whose app conversation is archived is refused with an error naming it, and no turn runs (#3024). |
| `bitwarden_search` | Search Bitwarden vault items by name/URI/folder/username (metadata only, no passwords). Only available when `[bitwarden] enabled = true`. |
| `bitwarden_unlock` | Unlock a vault item by ID — requires admin approval via aisudo/Telegram. Caches value for `secret_ttl`. Never returns the actual password. |
| `client` | (shell: `foci_client`) Act on the user's Android phone through the foci app (#2802; replaced `app_android`). Actions: `tasker_list`, `tasker_perform` (the Tasker bridge, allowlist-gated as before), `set_alarm` (HH:MM, label, `--days`; set silently, no date), `set_timer`, `call` (direct call), `contact_lookup`, `clipboard`, `open_url` (http/https/geo/obsidian only), `location`, `ring_phone`, `read_notifications` (the notifications in the shade at call time, no history; #2809), `health` (every Health Connect data type; `--from`, `--to`, `--limit`; no type lists the types and their grants; #2809). The server checks each action's arguments before sending; the phone refuses an action the user switched off in the app (Settings > Advanced > Phone actions; call, contact_lookup, location, read_notifications and health default off) or whose Android permission is not granted there — it never prompts at call time. Runs on the most recently connected device that advertises `tool:client`; the desktop app is never picked. A phone with an app from before #2802 still runs the two Tasker actions (as `android` list/perform); other actions get an update-the-app error. Distinct error strings for: no app device connected, no capable phone, app too old, no answer within 60s. Gated on `[[platforms]] id="app"` being configured. |
| `mcp` | Call a tool on a connected MCP server. Re-reads `mcp.toml` on each call — servers can be added/removed without restarting. Only registered when `mcp.toml` exists or `configDir` is set. See [CONFIG.md](CONFIG.md#mcptoml) for configuration. |
| `task_list` | Manage task items. Distinct from `todo`: `task_list` is its own subsystem for tracking task work in progress (see also [HEARTBEAT.md](HEARTBEAT.md)). |
| `set_session_alias` (foci_set_session_alias) | Set a short descriptive name for the current conversation. The alias is surfaced in session listings and UI surfaces. Disabled for the Codex backend, which manages session naming server-side. |
| `browser` (foci_browser) | Full CDP browser automation via [go-rod](https://github.com/go-rod/rod). Only registered when `[browser] enabled = true`. Provides navigation, DOM interaction, screenshots, and script evaluation against a real Chromium instance. Available on the API loop and, as `foci_browser`, on every delegated backend. Each session gets its own browser (never shared across the main chat, forks, branches or other chats); one idle for 30 minutes is stopped. Only one session at a time can open the persistent `user_data_dir` profile. |
| `compact_session` (foci_compact_session) | Compact the agent's own conversation, the same compaction as the user's `/compact` (#3040). One optional parameter, `wake_message` (#3041). The tool only records the request: the compaction runs in the post-turn phase of the turn that made it, after that turn's final reply, on every backend. It is not refused while subagents or background commands run; a Claude Code restart after it (system prompt changed, `reload_on_compact`) waits until that work finishes, so nothing is killed. The chat shows the usual compaction notices. Refused in a session with `no_compact` set (Dick 2026-11-10), unlike the user's `/compact`; a request made before `no_compact` was set is dropped at the turn's end. Nothing restarts the agent after the compaction unless `wake_message` is given: it is then injected into the same session once the compaction finishes and starts a turn the same way as a `remind` wake (via `wake`), with a line saying whether it compacted. It is injected whatever the outcome (compacted, nothing to compact, failed, or dropped by `no_compact`), and replaces the post-restart resume nudge. A later call without `wake_message` keeps an earlier call's message; a later one with it replaces it. The request and its wake live in memory: a restart or a session reset drops both. Not available in raw or character spawns (their tool calls run on the parent's context). |
| `secrets` (foci_secrets) | Ask to change `secrets.toml` (#3090): `list` (names and rules, never values; no dialog), `set <section.key> [--allowed-hosts h1,h2] [--allowed-agents a1,a2]`, `remove`, `hosts <section> add/remove/clear`, `body <section> add/remove/clear`, `agents <section> allow/deny/clear`. Each write opens a dialog in the foci app only (no app device = immediate failure); a set's value is typed there by the user and never reaches the agent. Returns at once; the outcome arrives as a later message. Gated on `[[platforms]] id="app"`; left out of spawns. See [SECRETS.md](SECRETS.md#agent-requests-foci_secrets-3090). |
| `close_session` (foci_close_session) | Close the app chat the agent is running in, exactly as if the user closed it: the chat leaves the open tabs on every device, its history stays, and the user's next message reopens it. Optional `mark_read` (bool, default false, #2592): also mark the chat read on every device through the end of this turn, final reply included. Gated on `[[platforms]] id="app"` being configured; on a non-app session it reports there is nothing to close. Refused in a depth leaf, open or reported (#2623): the leaf's chat belongs to the tree. |
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
- **Response redaction** — secret values in the inline body, header block, spill file and a text `save_to` file replaced with `[REDACTED]`; a binary save (save_to, decoded data URI, auto-save) that holds a secret value is refused, with no file written
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

Native ephemeral backends use disposable in-memory threads for inherited/clone workers; set `persistent=true` (`--persistent`) to retain their backend history for restart/resume. Fresh character, cross-agent, raw and explore spawns remain persistent. Ephemeral workers lose their conversation on backend restart; files and recorded results survive.


Unified sub-call to a model with four context modes, all with tool access.

**Every spawn has a name (#2721).** `name` is required on every spawn call (`--name` in the shell form), in every mode and with `agent=`; a missing or blank name is refused and starts nothing. Leading and trailing whitespace is dropped. A name with a run of spaces, a tab or a line break inside it is refused and starts nothing (#2810), because the name is shown on one line and looked up exactly; nothing else about it is restricted. It must be unique, exactly and case-sensitively, among the spawns `spawn_status` lists in the calling session at that moment: its own and those handed over to it from depth leaves, running or finished and still remembered (the last 20 finished per session, until a restart). A clash is refused with the holder's id; once that spawn is no longer listed, the name is free again. `all` is refused, because `spawn_stop all` means every spawn. A spawn's follow-up runs (`spawn_send` to a finished spawn, a resume after a restart) keep its name. The name labels the spawn everywhere: the app's stop dialog, its chip and view header and the activity caption, the `🐣 From spawn <id>: <name>` line on its prompts, the restart chat line and the Telegram/Discord tool-call line show the name instead of the prompt opening; `[SPAWN RESULT]`, `spawn_status` (which keeps its `Task:` line) and the background start reply add it. `spawn_send`, `spawn_stop` and `spawn_status` take a name wherever they take an id or branch key; an id or branch key wins over a name it equals, and a name that two reachable spawns carry (only possible across spawns handed over from different depth leaves) is refused with their ids. A spawn persisted before names existed shows `(unnamed spawn)` wherever its name would go; it cannot be found by a name and never blocks one.

| Mode | System prompt | Tools | Behaviour |
|------|--------------|-------|-----------|
| `character` (default) | The agent's full system prompt | All except `send_to_session`, `whoami`, `close_session`, `compact_session`, `ask` (nothing that needs the user or acts on the parent's session) | Returns its result directly; with `background=true` runs in the background on a fresh branch (none of the conversation) and is steered, checked on and delivered like a clone. |
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

**Spawn permissions (#2684, Dick 2026-10-30: "Spawns (as they are always from interactive sessions) should not skip permissions, they should report their permission requests up to the spawner").** No spawn is a skip-permissions session. A permission request (or question) from any spawn — every mode, direct or background, nested under depth leaves — that the auto-approve rules do not settle is shown in its spawner's chat, headed `🐣 From spawn <id>: <name>` (`🐣 From <agent>'s spawn <id>: <name>` for a cross-agent spawn, whose request goes to the caller's chat after the target's own rules, #2730), and does not block the spawner's turn; the answer goes back to the spawn's waiting call. A spawn starts in its spawner's permission mode: if that is auto mode, the classifier decides and nobody is asked (a cross-agent spawn keeps the target's own mode). Unattended as before (rules, else deny, logged `unattended: session …`): a spawn started by an unattended turn (keepalive, reflection, a webhook, …) and a spawn whose spawner's app chat is closed (`trigger=spawner_chat_closed`). A spawn that ends while a request waits takes it down from the chat. Raw and explore spawns keep their restricted tool sets either way. An explore spawn never asks: its commands are decided by foci's read-only rules alone, and the rest is denied (#2702). CC's own deny rules (`permissions.deny` in its settings) still refuse a call before any ask exists.

`clone` creates a branch `{parentKey}/b{TIMESTAMP}`, runs via `AsyncNotifier`, and returns an immediate ack naming its spawn id; a background character spawn does the same on a fresh branch (#2643). Spawns have no run-time limit; explicit stops, caller cancellation for direct spawns, and shutdown handling still apply. Model API requests retain their normal request/stream-idle limits. Delegated agents get `spawn` with the modes their backend supports (`foci_spawn <prompt> --name NAME [--context MODE] [--background] [--model NAME]`, #2523); `clone` needs a backend that can fork a session. `spawn_send` (steer a running background spawn, or resume a finished one on its branch) and `spawn_status` (list, or one spawn's last N activity entries) work on both paths. A spawn reports ONE `[SPAWN RESULT]` per run; messages its turn did not take run as a follow-up turn first. **Nesting (#3061): one rule, the spawn depth limit `[tools] max_spawn_depth` (default 2, live).** A chat (or any session no spawn started, a depth-mode leaf included) is depth 0, its spawn depth 1, that spawn's spawn depth 2; a session may start a spawn only if the new depth is at most the limit, in every mode, on every backend, and as another agent. A session that may not is not given the spawn tools (exec bridge, shell-tool listing, API tool list); a call anyway (a session launched before the limit was lowered) is refused naming its depth and the limit. A direct spawn (raw, explore, character without background) cannot start a background spawn or a clone: its session ends with its spawner's call, so nothing would take the result. Raw and explore spawns start no spawn at all (isolated and read-only). `spawn_stop` stops a running spawn of any mode by name or id, or `all` (#2656); a spawn that returns directly is tracked while its call waits, so `spawn_status` lists it and `spawn_stop`/`/stop` end it, and it stops when its call ends without its result (the exec bridge cancels a call whose `foci-call` is gone). `agent=<id>` (`--agent`) runs the spawn AS another agent (#2662): its character, tools, workspace, permission rules, backend and model; naming an agent implies character mode (an explicit other mode is refused); any agent may spawn any agent, within the spawn depth limit. Its `send_to_chat` goes to the caller's chat; its `send_to_session` and `remind` are refused. `send_to_spawner <message>` (every spawn, #2662 round 3): a spawn messages the session that spawned it while it runs (the caller's, for a cross-agent spawn). A background spawn's message arrives at once as a turn of its own (answer with `spawn_send`); a direct spawn's spawner is blocked in its call and cannot read it, so it is kept and heads the result, unless the spawner's session has no turn running, when it goes at once. A Claude Code raw spawn has no shell and so cannot send one. Every spawn first checks the plan allowance of the backend it runs on and is refused when the 5-hour or weekly window has less than `[tools] spawn_reserve_percent` (default 10) left, unless `use_reserve=true` (`--use-reserve`); no usage source, a failed read or a read still running lets it run (the latter two say so). Usage is read on a schedule, once per account every 4-14 min, so the check normally costs nothing. Concurrent spawns limited by `max_concurrent_spawns` (default 10, live-editable). An API `character` one-shot below the depth limit gets `spawn` (not `spawn_send`/`spawn_status`/`spawn_stop`: its calls run on its spawner's session key, so they would reach its spawner's other spawns); raw and explore one-shots never get it. A `raw` one-shot also excludes `whoami`: its tool calls run on the parent's context, so it would report the parent's session key, chat and model. An API `character` one-shot has the same tools as a delegated character spawn, `whoami` and `send_to_session` included (#2676): both report and send from an identity of its own (`<parent root>/b<ns>`, the spawn's model), its `send_to_session` reply goes to the target's own chat, and a cross-agent spawn's `send_to_session` stays refused.

**`explore` allowlist.** The read-only tool set for `explore` is broader than just the search/read primitives: `git` and `todo` are always available, and the following are enabled conditionally (based on config/availability): `file`, `stat`, `wc`, `head`, `tail`, `tree`, `du`, `jq`, `yq`, `mdq`, `docker`, `systemctl`, `sqlite3`, `crontab`, `id`. None of these can mutate the workspace — they are inspection-only commands wrapped as tools. `todo` runs only list, search and get (#2702).

**Delegated `explore`: a read-only shell (#2702, Dick 2026-11-01).** A delegated agent's explore spawn (Claude Code, opencode) has its backend's read and search tools and a shell whose every command foci decides from the built-in read-only auto-approve set (the `read-only` group: `ls`, `cat`, `grep`, `rg`, `find`, `jq`, `yq`, `mdq`, `sqlite3 -readonly -safe`, `journalctl`, read-only `git` — status, log, show, diff, blame, grep, list-only branch/tag, … (#2734) — …) plus its read-only foci functions (`foci_memory_search`, `foci_todo` list/search/get, `foci_web_search`, `foci_web_fetch`, `foci_send_to_spawner`, …). Anything else — another command, a redirect to a file, a write flag such as `sed -i` or `find -delete` — is denied at once; nobody is asked. Its system prompt lists that whitelist, generated from the same rules, and invites it to name a missing tool at the end of its answer. On codex the spawn keeps codex's own shell in its read-only sandbox, and never asks to leave it.

## Slash Commands Are Not Tools

Slash commands are for the user and the operator only: they are not exposed to the agent as tools. The tool registration table (`toolTable` in `cmd/foci-gw/tool_table.go`) is the only place tools are registered, and it builds no tool from the command registry. Where an agent needs what a command does, it gets a tool of its own: `compact_session` (the agent's `/compact`), `close_session`, `set_session_alias`. An operator can still run a command against a session with `foci command "/<command>" -a <agent> -s <session>`. See [COMMANDS.md](COMMANDS.md) for the command reference.

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
Manage the todo list. `list` shows active items only (excludes done/dropped); use `list-all` or `list --status all` to see everything. `add` reads piped stdin as the body when neither `--text` nor `--body` is given; bare words then become the title, as with `--body`. Piped stdin beside `--text`/`--body` is refused, not dropped.
```bash
foci_todo add "Review PR #42"
foci_todo add --title "Fix the thing" <<'EOF'
Body text, as many lines as needed.
EOF
foci_todo list                    # active items only
foci_todo list --status all       # include done/dropped
foci_todo list-all                # shorthand for --status all
foci_todo complete 3
foci_todo search "review"
foci_todo remove 5
```

#### `foci_spawn <prompt> --name N [--model M] [--context C] [--background]`
Spawn a sub-call to a model (default context `character`). `--name` is required and unique in the session; `foci_spawn_status`, `foci_spawn_send --id` and `foci_spawn_stop` take it in place of the id.
```bash
foci_spawn "Summarize this data" --name summary --model haiku --context raw
foci_spawn_status summary
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
foci_spawn "Summarize this: $context" --name checklist --model haiku --context raw
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

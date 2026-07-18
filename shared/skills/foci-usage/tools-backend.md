<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Tools for backend (shell-based) agents

A manual for agents running on the **Claude Code (CC) backend** — most foci agents. foci delegates inference and tool execution to a CC process and bridges the messaging platform to it. You have **two tool families at once**, and this file covers both.

## 1. CC-native tools

CC gives you its own first-class tools: `Read`, `Write`, `Edit`, `Bash`, `Grep`, `Glob`, `Agent` (sub-agents), `WebFetch`, and more. Use these for files, shell, search, and spawning sub-agents — they're the right tool for anything general-purpose. foci does **not** replace them.

## 2. foci tools (as shell functions)

foci exposes its own tools to you as `foci_*` **shell functions** you call through Bash:

```
foci_todo list --status open
```

How this works: foci generates a shell-functions file and points `BASH_ENV` at it, so Bash sources it on startup and every `foci_*` function is defined. Each function packages its arguments as JSON and sends them over a Unix socket (`FOCI_SOCK`) to foci, which runs the real tool and returns the result. A startup parity check guarantees every flag in a tool's `--help` actually has a working handler — so the help text is authoritative.

**The foci tools always available to you as shell functions:**
`foci_ask`, `foci_send_to_chat`, `foci_send_to_session`, `foci_todo`, `foci_remind`, `foci_memory_search`, `foci_http_request`, `foci_web_fetch`, `foci_web_search`, `foci_summary`.

**`foci_spawn` is also available, conditionally:** the tool table exposes `spawn` to delegated agents whose backend can fork a session (`Agent.DelegatedManager.BackendCanBranch()` — true for `claude-code`/ccstream, `codex`, and `opencode`; **false for `claude-code-tmux`/cctmux**, which has no fork/branch support). If your backend is streaming CC (the common case), you have it — same four modes (`raw`/`character`/`clone`/`explore`) as the API-loop path; `clone` routes through `Agent.ForkSession`. On cctmux, `spawn` isn't registered at all, so prefer CC's native `Agent` tool for sub-calls there.

There is **no `foci_tmux`** on this backend — that stays API-loop-only. For persistent terminals use `Bash` with `tmux` (see the `coding-agent` skill). foci's file/shell/browser tools are likewise absent — use CC's `Read`/`Write`/`Edit`/`Bash`/browser instead.

Every tool accepts `-h`/`--help`. **Read the `--help` before first use of any tool this session.**

### `foci_ask` — ask the user selectable questions
- **Async.** Posts the question(s) and returns immediately; answers arrive later as a new inbound message. **End your turn after calling it** — do not wait.
- JSON-only input (questions object). No 4-question cap (unlike the backend's built-in AskUserQuestion — prefer `foci_ask`).
- Request IDs must be **colon-free** (button payloads are encoded as `<id>:<index>`).
- Optional grader: `--grader <abs-path>` runs an executable over `{request_id, questions, answers}` (JSON on stdin) and delivers *its* output to you instead of the raw answers. `--grader-args <json-array>` appends argv after request_id; `--grader-timeout-seconds` (default 15); `--grader-on-error fallback|report`.

### `foci_send_to_chat` — send a rich message to your own chat
- Positional `text` (or stdin, or `--text`/`--description`). Markdown supported.
- `--file <path>` attaches a file; `--filename <name>` sets display name; `--send-as document|voice|video|photo|audio|animation`.
- It always sends to **your own** chat (no chat-targeting parameter — the destination derives from your session). To reach a different chat, use `foci_send_to_session`.
- **Don't use it to duplicate a plain reply** on a bot-attached session — your reply text is already delivered. Use it for attachments or piping command output (`… | foci_send_to_chat`).

### `foci_send_to_session` — message another session
- Positional `session_key`: full session key (`scout/c5970082313`, `scout/iresearch`), agent-qualified session name or chat alias (`scout/research`), or bare agent name (`scout` → its default session).
- `--message` (or stdin). `--reply-to caller|session` (default **caller** — the reply comes back to *you*, not the target's user chat; use `session` to surface it to their chat).

### `foci_spawn` — sub-calls to a model (backend must support forking; see above)
- Four context modes: `raw` (no system context, cheapest isolated call), `character` (full system + character files — a copy of you), `clone` (async branch of the current session via `Agent.ForkSession`; result arrives later as a message — the default), `explore` (sync, read-only, cheap model, restricted toolset for investigation).
- Selects a model group with `--powerful|--fast|--cheap`. Same semantics as the API-loop `spawn` tool (see tools-api.md) — the shell-function form just wraps the same JSON schema.

### `foci_todo` — persistent todo list
- Subcommand-style: `add|list|list-all|search|get|complete|drop|edit|remove` (`create` is an alias for `add`).
- `add --text T [--priority high|medium|low] [--tag TAGS]`. **`--tag` REPLACES the tags parsed from the text body**, it doesn't append.
- `list [--status open|started|done|dropped|active|all] [--tag T] [--priority P] [--sort F] [--reverse] [--limit N]`. **Defaults to `--limit 10`** — use `get <id>` or `--limit 100` to see the full backlog.
- `complete|drop <id>` accept `--reason`/`--note`/`--notes` (all map to the close reason); ID forms `<id>` positional, `--id N`, or `--ids 1,2,3`.
- **No `reopen` verb.** To reopen a closed item, write the DB directly: `UPDATE todos SET status='open', completed_at=NULL, close_reason='' WHERE agent_id=? AND id=?` on `~/<agent>/.data/todo.db`.
- Chain with Unix tools to keep output small: `foci_todo list --status open | wc -l`.

### `foci_remind` — defer a thought
- `--text T --when SPEC`. SPEC: duration (`2h`, `30m`), `tomorrow`, `next_keepalive`, `next_session`, a date (`YYYY-MM-DD`), or an ISO timestamp.
- `--wake` (default false): passive reminders inject as context at the time; `--wake` actively wakes the session with a message to yourself.

### `foci_memory_search` — full-text search of memory + conversation history
- Positional `query`, stemmed FTS. Memory files rank above chat history.
- `--sort relevance|newest|oldest`, `--date-from`/`--date-to YYYY-MM-DD`, `--lines N` (context window).
- Direct lookup: `--query "session#rowID"` (e.g. `agent/c123#42`) pulls surrounding messages.
- This reaches **conversation history that grep can't** — prefer it over grepping the memory dir.

### `foci_http_request` — HTTP with server-side secret resolution
- Positional `url`. `--method`, `--header 'K: V'` (repeatable) or `--headers <json>`, `--body`/`--body-file`, `--query <json>`.
- Secrets: `{{secret:NAME}}` in headers is resolved server-side against `allowed_hosts`; in body/form fields it requires `allowed_in_body` in secrets.toml.
- `--save-to <path>` writes the body to disk (returns status/headers only); `--save-from-json-path data.0.url` extracts a field first (and decodes `data:` URIs). `--background` runs async. `--include-headers` keeps status/headers in output.
- Filter responses with jq so only what you need hits context: `foci_http_request URL | jq '.[].name'`.

### `foci_web_fetch` — URL → clean Markdown
- Positional `url`. Readability extraction → Markdown; `--raw` returns HTML. SSRF-safe. Not for downloading files (use `foci_http_request`); large pages truncated.

### `foci_web_search` — Brave web search
- Positional `query`. Returns titles/URLs/descriptions. Requires a Brave API key configured.

### `foci_summary` — extract from a file via a cheap model
- `foci_summary "what does this define?" --file path` (or pipe: `… | foci_summary "categorise these"`). For targeted extraction, **not** dumping a whole file. Great for piping noisy data through a cheap model before it hits your context.

## 3. Deferred tools & ToolSearch

Some CC backend tools aren't loaded into the prompt up-front — they appear by *name only* in a `<system-reminder>` as "deferred" (MCP tools, calendar, etc.). You can't call a deferred tool until you fetch its schema with **ToolSearch** (`select:<name>` for exact, or keywords). Once its definition is returned, it's callable like any other tool. This keeps the tool list small until you actually need a tool.
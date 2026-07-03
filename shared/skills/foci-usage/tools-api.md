# Tools for API-loop agents

A manual for agents running on foci's **own API loop** (`backend = ""` or `"api"`) rather than the Claude Code backend. Here foci itself drives the request → tool-use → response loop directly against the model endpoint.

## How you call tools

foci hands the model a set of **formal tool definitions** (each a name + JSON schema). You invoke a tool by emitting a **JSON tool-call** with its name and arguments; foci dispatches it, runs the real tool, and returns a tool-result message you see on the next iteration. There is no shell layer and no `foci_*` shell functions — that form exists only on the Claude Code backend. The JSON schema attached to each tool is always authoritative for exact argument names and types.

## What you get that backend agents don't

Because there's no Claude Code process underneath, an API-loop agent is given foci's **own** general-purpose tools as first-class definitions — the things a CC agent would instead get from CC natively:

- **`read`** — read a file (path, with optional offset/limit).
- **`write`** — create or overwrite a file (path, content).
- **`edit`** — exact string replacement in a file (path, old, new).
- **`shell`** — run a shell command (command, optional timeout).
- **`tmux`** — persistent terminal sessions: `start|send|read|list|kill|watch|unwatch` (by name). Async inactivity notifications via `watch`.
- **`browser`** — headless browser actions (only if the browser feature is enabled in config).
- **`spawn`** — sub-calls to a model in four context modes: `raw` (no system context, cheapest isolated call), `character` (full system + character files — a copy of you), `clone` (async branch of the current session; result arrives later as a message — the default), `explore` (sync, read-only, cheap model, restricted toolset for investigation). Selects a model group with `powerful|fast|cheap`.
- **`scratchpad`** — working-notes store (only if configured).
- **`task_list`** — structured task items (only if configured).
- **`bitwarden_search` / `bitwarden_unlock`** — vault access (only if configured).
- **`mcp`** — call tools exposed by connected MCP servers.

(Each is enabled only when its backing store/feature is configured; otherwise it isn't offered.)

## Shared tools (also on the backend, as shell functions)

These behave the same as for backend agents — the only difference is you call them as JSON tool-calls rather than `foci_*` shell functions. Argument names below are the conceptual fields; the schema is authoritative.

### `ask` — ask the user selectable questions
- **Async.** Posts the question(s) and returns immediately; answers arrive later as a new inbound message. **End your turn after calling it.**
- Questions object as input. No 4-question cap.
- Request IDs must be **colon-free** (button payloads are encoded as `<id>:<index>`).
- Optional grader: an executable run over `{request_id, questions, answers}` (JSON on stdin) whose output is delivered to you instead of the raw answers; with optional extra argv, a timeout (default 15s), and a fallback/report on-error mode.

### `send_to_chat` — send a rich message to your own chat
- `text` (markdown supported); optional `file` attachment with display `filename` and `send-as` (document|voice|video|photo|audio|animation).
- Always sends to **your own** chat (no chat-targeting field — destination derives from your session). To reach a different chat, use `send_to_session`.
- Don't use it to duplicate a plain reply on a bot-attached session — your reply text is already delivered. Use it for attachments.

### `send_to_session` — message another session
- `session_key`: full session key (`scout/c5970082313`, `scout/iresearch`), agent-qualified session name or chat alias (`scout/research`), or bare agent name (`scout` → its default session).
- `message`; `reply-to` = `caller` (default — reply returns to you) or `session` (reply goes to the target's own chat).

### `todo` — persistent todo list
- Actions: `add|list|list-all|search|get|complete|drop|edit|remove`.
- `add`: text, optional `priority` (high|medium|low), optional `tag` set — **`tag` REPLACES tags parsed from the text body**, it doesn't append.
- `list`: filter by `status` (open|started|done|dropped|active|all), `tag`, `priority`; `sort`, `reverse`, `limit`.
- `complete|drop` take a close `reason`; ID forms single or list.

### `remind` — defer a thought
- `text` + `when`: duration (`2h`, `30m`), `tomorrow`, `next_keepalive`, `next_session`, a date, or an ISO timestamp.
- `wake` (default false): passive reminders inject as context at the time; `wake` actively wakes the session.

### `memory_search` — full-text search of memory + conversation history
- `query`, stemmed FTS; memory files rank above chat. `sort` (relevance|newest|oldest), `date-from`/`date-to`, `lines` (context window).
- Direct lookup form `session#rowID` pulls surrounding messages. Reaches conversation history that a file grep can't.

### `http_request` — HTTP with server-side secret resolution
- `url`, `method`, `header`(s) or `headers`, `body`/`body-file`, `query`.
- `{{secret:NAME}}` in headers resolves server-side against `allowed_hosts`; in body/form fields it requires `allowed_in_body`.
- Can save the body to a path, extract a JSON field first, run in the background, or include status/headers.

### `web_fetch` — URL → clean Markdown
- `url`; Readability extraction → Markdown (or raw HTML). SSRF-safe; large pages truncated. Not for downloading files (use `http_request`).

### `web_search` — Brave web search
- `query` → titles/URLs/descriptions. Requires a Brave API key.

### `summary` — extract from a file via a cheap model
- A question plus a `file` (or piped input). For targeted extraction, not whole-file dumping.

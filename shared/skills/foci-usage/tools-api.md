<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Tools for API-loop agents

For agents on foci's **own API loop** (`backend = ""` or `"api"`): foci drives the model directly and you call tools as JSON tool-calls. There are no `foci_*` shell functions (those exist only on the Claude Code backend). Each tool's JSON schema is authoritative for argument names and types.

## What you get that backend agents don't

foci's **own** general-purpose tools, which a CC agent gets from CC instead:

- **`read`**, **`write`**, **`edit`** (exact string replacement), **`shell`** — files and shell.
- **`tmux`** — persistent terminals by name: `start|send|read|list|kill|watch|unwatch`. `watch` sends async inactivity notifications.
- **`browser`** — headless browser actions (only if the browser feature is enabled in config).
- **`spawn`** — a sub-call to a model. Model group `powerful|fast|cheap`. Timeout: 120 s direct, 3600 s background, 86400 s max. Modes:
- `character` (default): your system prompt and tools, not this conversation, so the prompt must carry the whole task. Direct, or background with `background=true`.
- `clone`: a background fork of this session; the result arrives later as a message. Use it only when the task needs too much of the conversation to write down.
- `raw`: just your prompt, the cheapest isolated call. `explore`: direct, read-only, cheap model, restricted tools.
- **`spawn_send`** — steer a running background spawn, or resume a finished one with a new message. **`spawn_status`** — the list of your spawns, or one spawn's latest activity.
- **`send_to_spawner`** — inside a spawn, of any mode: message the session that spawned you while you run. From a background spawn it arrives now (your spawner answers with `spawn_send`); from a spawn your spawner waits on, it is kept and heads your result.
- **`spawn_stop`** — stop a running spawn of any mode by id (or `all`).
- **`spawn` with `agent=<id>`** runs as another agent: its character, tools and model; implies character mode. The result comes back to you, and its `send_to_chat` lands in your chat.
- Every spawn checks its backend's plan allowance first. It is refused with less than `[tools] spawn_reserve_percent` (default 10%) left, unless `use_reserve=true`.
- **`scratchpad`** — working-notes store (only if configured).
- **`task_list`** — structured task items (only if configured).
- **`bitwarden_search` / `bitwarden_unlock`** — vault access (only if configured).
- **`mcp`** — call tools exposed by connected MCP servers.

(Each is enabled only when its backing store/feature is configured; otherwise it isn't offered.)

## Shared tools (also on the backend, as shell functions)

Same behaviour as the backend's `foci_*` functions. Below are only the behaviours the schema does not tell you.

- **`ask`** — async: posts and returns at once; answers arrive as a new message, so **end your turn**. No question cap. Request IDs must be **colon-free**. `quiz` questions hide the answer until revealed — see the `quiz` skill. A grader executable gets `{request_id, questions, answers}` on stdin; its output replaces the raw answers.
- **`send_to_chat`** — always your **own** chat (destination comes from your session); use `send_to_session` for another. Your reply text is already delivered, so use it for attachments, not to repeat a reply.
- **`send_to_session`** — `session_key` takes a full key (`scout/c5970082313`), `agent/name` or alias, or a bare agent name (its default session). `reply-to`: `caller` (default, reply comes back to you) or `session` (reply goes to the target's chat).
- **`todo`** — `tag` **replaces** the whole tag set (and tags parsed from the text); it never appends.
- **`remind`** — passive by default (injected as context when due); `wake` actively wakes the session.
- **`memory_search`** — stemmed full-text; memory files rank above chat. `session#rowID` pulls the surrounding messages.
- **`http_request`** — `{{secret:NAME}}` resolves server-side: in headers against the secret's `allowed_hosts`, in body/form fields only with `allowed_in_body`. `basic_auth` takes `user:password` (templates resolved, then base64-encoded; the token is redacted from output), e.g. `'{{secret:NAME}}:'` for a key-as-username. Returns the body only unless `include_headers: true`.
- **`web_fetch`** — Readability → Markdown; large pages truncated. Not for downloading files (use `http_request`).
- **`web_search`** — Brave Search; titles, URLs and descriptions only.
- **`summary`** — targeted extraction from a file by a cheap model; never a whole-file dump.
- **`set_session_alias`** — chat sessions only (errors on a branch). Call once after the first exchange, under 5 words. It won't overwrite a manual rename (it replies that the user named the chat by hand) unless you pass `force`, which is only for when the user explicitly asks for the rename.
- **`app_android`** — offered when the app platform is configured, but offered ≠ connected: with no device it returns a plain error string. The on-device allowlist is **empty by default** (the user opts tasks in under the app's Advanced settings). Structured args go JSON-stringified in `par1`. A task still running after ~60s returns pending, and its later result is dropped.
- **`close_session`** — closes your own app chat as if the user closed it (tab leaves every device; history stays; the user's next message reopens it). Only when nothing is outstanding or the user asks. `mark_read: true` also marks the chat read once this turn ends, final reply included. Offered when the app platform is configured.
- **`whoami`** — read-only report of your own foci identity: agent id, session key, chat id, platform, backend, transport and model (on backends this is `foci_whoami`, same report). The go-to source for the chat id `send_to_session` addressing needs; missing values come back as `none`/`unknown`, never guessed.

<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

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
- **`spawn`** — sub-calls to a model in four context modes: `character` (the default: your full system prompt and tools, not this conversation — write the prompt to carry the whole task; returns directly, or runs in the background with `background=true`), `clone` (a background fork of the current session; result arrives later as a message — only when the task needs so much of the conversation that it cannot be written down), `raw` (just your prompt, cheapest isolated call), `explore` (direct, read-only, cheap model, restricted toolset for investigation). Selects a model group with `powerful|fast|cheap`. Timeout: 120 s default directly, 3600 s in the background, 86400 s max.
- **`spawn_send`** / **`spawn_status`** — steer a running background spawn — clone or background character — (or resume a finished one with a new message), and check on your spawns: the list, or one spawn's most recent activity.
- **`scratchpad`** — working-notes store (only if configured).
- **`task_list`** — structured task items (only if configured).
- **`bitwarden_search` / `bitwarden_unlock`** — vault access (only if configured).
- **`mcp`** — call tools exposed by connected MCP servers.

(Each is enabled only when its backing store/feature is configured; otherwise it isn't offered.)

## Shared tools (also on the backend, as shell functions)

Same behaviour as for backend agents, called as JSON tool-calls instead of `foci_*` shell functions. Each tool's schema lists its parameters and is authoritative; below are only the behaviours the schema doesn't tell you.

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

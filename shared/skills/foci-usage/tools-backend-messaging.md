<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backend tools — talking to users and sessions

Per-tool reference for the Claude Code (shell) backend. How `foci_*` shell functions work, and which ones you have, is in **tools-backend.md**. Every tool accepts `-h`/`--help`; read it before first use this session.

### `foci_ask` — ask the user selectable questions
- **Async.** Posts the question(s) and returns immediately; answers arrive later as a new inbound message. **End your turn after calling it** — do not wait.
- JSON-only input (questions object). No 4-question cap (unlike the backend's built-in AskUserQuestion — prefer `foci_ask`).
- Request IDs must be **colon-free** (button payloads are encoded as `<id>:<index>`).
- Optional grader: `--grader <abs-path>` runs an executable over `{request_id, questions, answers}` (JSON on stdin) and delivers *its* output to you instead of the raw answers. `--grader-args <json-array>` appends argv after request_id; `--grader-timeout-seconds` (default 15); `--grader-on-error fallback|report`.

### `foci_send_to_chat` — send a rich message to your own chat
- Positional `text` (or stdin, or `--text`/`--description`/`--caption`). Markdown supported.
- `--file <path>` attaches a file; `--filename <name>` sets display name; `--send-as document|voice|video|photo|audio|animation`.
- It always sends to **your own** chat (no chat-targeting parameter — the destination derives from your session). To reach a different chat, use `foci_send_to_session`.
- **Don't use it to duplicate a plain reply** on a bot-attached session — your reply text is already delivered. Use it for attachments or piping command output (`… | foci_send_to_chat`).

### `foci_send_to_session` — message another session
- Positional `session_key`: full session key (`scout/c5970082313`, `scout/iresearch`), agent-qualified session name or chat alias (`scout/research`), or bare agent name (`scout` → its default session).
- `--message` (or stdin). `--reply-to caller|session` (default **caller** — the reply comes back to *you*, not the target's user chat; use `session` to surface it to their chat).

### `foci_set_session_alias` — name this conversation
- `--alias TEXT` (required). Sets a short descriptive name for the current chat session, shown in the chat list. Call once after the first exchange to name what the conversation is about; keep it under 5 words.
- **Chat sessions only** — errors on a branch/independent session key.
- **Won't clobber a manual rename:** if the chat already has an alias that wasn't set by this tool, it replies "Skipped" instead of overwriting it.
- `--replace-manual` overrides that guard. Use it **only when the user explicitly asks you to rename the chat**. The new name then counts as the user's own, so a later plain call skips it too.
- **Only registered when your backend doesn't auto-name sessions.** Codex generates thread names itself (`TurnResult.ThreadName`) and never gets this tool; streaming CC (ccstream), cctmux, opencode, and API-loop agents all lack auto-naming and get it.

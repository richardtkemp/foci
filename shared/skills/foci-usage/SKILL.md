---
name: foci-usage
description: How to operate as a foci agent — the tools you call, how your prompts and turns are built, the databases behind your state, and the config that shapes you. Read the relevant subfile before doing related work.
---

# Foci Usage — Operating Manual for a Foci Agent

You are an agent running on **foci**, a platform that bridges messaging channels (Telegram, Discord, an HTTP API, voice) to an LLM backend and gives you a set of tools, a persistent memory, and a turn lifecycle. This skill is the operating manual: what foci *is* from your point of view, and where to look when you need detail.

## Where to look

| Subfile | Read it when you need… |
|---------|------------------------|
| **tools-api.md** | You're an **API-loop agent** (`backend = ""`/`"api"`): a self-contained manual for calling tools as formal JSON tool-calls, including foci's own file/shell/spawn/browser tools. |
| **tools-backend.md** | You're a **Claude Code (shell) agent** (most agents): a self-contained manual for the `foci_*` shell functions, plus CC-native tools and deferred tools/ToolSearch. |
| **prompts.md** | Where foci's prompt templates live and how to customise them; the `[meta]` header, nudges, injections; `[[NO_RESPONSE]]`; compaction. |
| **scheduled-tasks.md** | The periodic tasks foci runs for you (keepalive, reflection, consolidation, log rotation) and how to create your own durable scheduled turns via the crontab. |
| **databases.md** | The SQLite stores behind todos, reminders, scratchpad, memory, sessions, cost. |
| **config.md** | `foci.toml` structure, smart defaults derived from agent ID, secrets, models, cron. |

For **debugging** foci (reading logs, diagnosing cache busts, tracing turns), see the companion `foci-debugging` skill — this skill is about normal operation, that one is about investigation.

## First principles

- **Files are memory.** You have no continuity between sessions except what's written to your character files and daily memory files. See **prompts.md** and **databases.md**.
- **Text you emit is delivered.** On a bot-attached session, a plain text reply already reaches the user. Don't double-send with `foci_send_to_chat` — that tool is for attachments or piping command output, always to your *own* chat (it has no chat-targeting param). To reach a *different* chat, use `foci_send_to_session`.
- **Silence is a turn outcome.** Emit a bare `[[NO_RESPONSE]]` (and nothing else) to complete a turn without messaging anyone.
- **Secrets never leave their store.** Reference them as `{{secret:NAME}}` in `foci_http_request`; never paste, echo, or commit them. See **config.md**.

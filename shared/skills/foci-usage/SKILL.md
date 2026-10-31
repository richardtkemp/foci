---
name: foci-usage
description: How to operate as a foci agent — the tools you call, how your prompts and turns are built, the databases behind your state, and the config that shapes you. Read the relevant subfile before doing related work.
owner: foci
seeded: true
---

# Foci Usage — Operating Manual for a Foci Agent

You run on **foci**: it bridges messaging channels (Telegram, Discord, the app, an HTTP API, voice) to an LLM backend, and gives you tools, persistent memory and a turn lifecycle.

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/foci-usage/`), not the deployed `~/shared/skills/` copy.

## Where to look

| Subfile | Read it when you need… |
|---------|------------------------|
| **tools-api.md** | You're an **API-loop agent** (`backend = ""`/`"api"`): a self-contained manual for calling tools as formal JSON tool-calls, including foci's own file/shell/spawn/browser tools. |
| **tools-backend.md** | You're a **Claude Code (shell) agent** (most agents): start here: how the `foci_*` shell functions work and which you have. It routes to the per-tool files below. |
| **tools-backend-messaging.md** | (CC agent) `foci_ask`, `foci_send_to_chat`, `foci_send_to_session`, `foci_set_session_alias`, `foci_close_session`. |
| **tools-backend-state.md** | (CC agent) `foci_todo`, `foci_remind`, `foci_memory_search`. |
| **tools-backend-spawn.md** | (CC agent) `foci_spawn`, `foci_spawn_send`, `foci_spawn_status`, `foci_spawn_stop`, `foci_send_to_spawner`. |
| **tools-backend-external.md** | (CC agent) `foci_http_request`, `foci_web_fetch`, `foci_web_search`, `foci_summary`, `foci_browser`, `foci_app_android`. |
| **pretool-rules.md** | (CC agent) A tool call was refused by a `PreToolUse` hook, or you need to list, test or write foci's deny rules (`foci pretool list/test`). |
| **prompts.md** | Where foci's prompt templates live and how to customise them; the `[meta]` header, nudges, injections; `[[NO_RESPONSE]]`; compaction. |
| **scheduled-tasks.md** | The periodic tasks foci runs for you (keepalive, reflection, consolidation, log rotation) and how to create your own durable scheduled turns via the crontab. |
| **databases.md** | The SQLite stores behind todos, reminders, scratchpad, memory, sessions, cost. |
| **config.md** | `foci.toml` structure, smart defaults derived from agent ID, secrets, models, cron. |

For **debugging** foci (logs, cache busts, tracing turns), see the `foci-debugging` skill.

## First principles

- **Files are memory.** You have no continuity between sessions except what's written to your character files and daily memory files. See **prompts.md** and **databases.md**.
- **Text you emit is delivered.** On a bot-attached session, a plain text reply already reaches the user. Don't double-send with `foci_send_to_chat` — that tool is for attachments or piping command output, always to your *own* chat (it has no chat-targeting param). To reach a *different* chat, use `foci_send_to_session`.
- **Silence is a turn outcome.** Emit a bare `[[NO_RESPONSE]]` (and nothing else) to complete a turn without messaging anyone.
- **Secrets never leave their store.** Reference them as `{{secret:NAME}}` in `foci_http_request`; never paste, echo, or commit them. See **config.md**.

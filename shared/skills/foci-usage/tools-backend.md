<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Tools for backend (shell-based) agents

A manual for agents running on the **Claude Code (CC) backend** — most foci agents. foci delegates inference and tool execution to a CC process and bridges the messaging platform to it. You have **two tool families at once**, and this file covers both.

This file is the entry point: how the two families work and which tools you have. The per-tool reference lives in sibling files — read the one for what you are about to do:

| file | read it when you are about to… |
|---|---|
| **tools-backend-messaging.md** | talk to someone: ask the user (`foci_ask`), post to your chat (`foci_send_to_chat`), message another session (`foci_send_to_session`), or name the chat (`foci_set_session_alias`). |
| **tools-backend-state.md** | track work or recall: todos (`foci_todo`), reminders (`foci_remind`), searching memory and chat history (`foci_memory_search`). |
| **tools-backend-external.md** | reach outside: HTTP (`foci_http_request`), web pages and search (`foci_web_fetch`, `foci_web_search`), cheap-model extraction (`foci_summary`), a headless browser (`foci_browser`), the user's Android device (`foci_app_android`). |

## 1. CC-native tools

CC gives you its own first-class tools: `Read`, `Write`, `Edit`, `Bash`, `Grep`, `Glob`, `Agent` (sub-agents), `WebFetch`, and more. Use these for files, shell, search, and spawning sub-agents — they're the right tool for anything general-purpose. foci does **not** replace them.

## 2. foci tools (as shell functions)

foci exposes its own tools to you as `foci_*` **shell functions** you call through Bash:

```
foci_todo list --status open
```

How this works: foci generates a shell-functions file and points `BASH_ENV` at it, so Bash sources it on startup and every `foci_*` function is defined. Each function packages its arguments as JSON and sends them over a Unix socket (`FOCI_SOCK`) to foci, which runs the real tool and returns the result.

A startup parity check guarantees every flag in a tool's `--help` actually has a working handler — so the help text is authoritative.

**The foci tools always available to you as shell functions:**
`foci_ask`, `foci_send_to_chat`, `foci_send_to_session`, `foci_todo`, `foci_remind`, `foci_memory_search`, `foci_http_request`, `foci_web_fetch`, `foci_web_search`, `foci_summary`.

There is **no `foci_spawn`** on this backend — `spawn` is API-loop-only (its one-shot modes run over the direct API, never your backend). For sub-calls use CC's native `Agent` tool.

There is **no `foci_tmux`** on this backend — that stays API-loop-only. For persistent terminals use `Bash` with `tmux` (see the `coding-agent` skill). foci's file/shell tools are likewise absent — use CC's `Read`/`Write`/`Edit`/`Bash` instead.

**Three more conditional tools**, present when their backing config is on: `foci_browser` (when `[browser] enabled`, the default — see the `browser` skill), `foci_set_session_alias` (backends that don't auto-name sessions — see tools-backend-messaging.md) and `foci_app_android` (only when the `app` platform is configured).

Every tool accepts `-h`/`--help`. **Read the `--help` before first use of any tool this session.**

**`--json` for machine-readable output.** Every `foci_*` function except `foci_ask` (where `--json` is the questions *input*) takes `--json`, in any position, and prints one JSON document instead of its text — pipe it to `jq` rather than parsing prose. Text stays the default. `foci_web_search`, `foci_web_fetch`, `foci_memory_search`, `foci_http_request` and `foci_remind` print a tool-specific shape (their `--help` "Output:" section documents it); every other tool prints `{"result": "<the text>"}`; `foci_todo list/search/get` print JSONL, as `--format jsonl`. Errors are unchanged: stderr and a non-zero exit.

## 3. Refused calls: PreToolUse rules

foci can refuse a tool call before it runs. You see `PreToolUse:<Tool> hook error:` and a reason that says what to do instead. Follow the reason; do not retry the call or rephrase it to dodge the rule. To see your rules or check a call against them, run `foci pretool list` / `foci pretool test`. See **pretool-rules.md**.

## 4. Deferred tools & ToolSearch

Some CC backend tools aren't loaded into the prompt up-front — they appear by *name only* in a `<system-reminder>` as "deferred" (MCP tools, calendar, etc.).

You can't call a deferred tool until you fetch its schema with **ToolSearch** (`select:<name>` for exact, or keywords). Once its definition is returned, it's callable like any other tool. This keeps the tool list small until you actually need a tool.

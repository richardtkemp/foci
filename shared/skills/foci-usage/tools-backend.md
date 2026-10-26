<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Tools for backend (shell-based) agents

A manual for agents running on the **Claude Code (CC) backend** — most foci agents. foci delegates inference and tool execution to a CC process and bridges the messaging platform to it. You have **two tool families at once**, and this file covers both.

This file is the entry point: how the two families work and which tools you have. The per-tool reference lives in sibling files — read the one for what you are about to do:

| file | read it when you are about to… |
|---|---|
| **tools-backend-messaging.md** | talk to someone: ask the user (`foci_ask`), post to your chat (`foci_send_to_chat`), message another session (`foci_send_to_session`), name the chat (`foci_set_session_alias`), or close it (`foci_close_session`). |
| **tools-backend-state.md** | track work or recall: todos (`foci_todo`), reminders (`foci_remind`), searching memory and chat history (`foci_memory_search`). |
| **tools-backend-external.md** | reach outside: HTTP (`foci_http_request`), web pages and search (`foci_web_fetch`, `foci_web_search`), cheap-model extraction (`foci_summary`), a headless browser (`foci_browser`), the user's Android device (`foci_app_android`). |

## 1. CC-native tools

CC gives you its own first-class tools: `Read`, `Write`, `Edit`, `Bash`, `Grep`, `Glob`, `Agent` (sub-agents), `WebFetch`, and more. Use them for files, shell, search and sub-agents. foci does **not** replace them.

## 2. foci tools (as shell functions)

foci exposes its own tools to you as `foci_*` **shell functions** you call through Bash:

```
foci_todo list --status open
```

How this works: foci generates a shell-functions file and points `BASH_ENV` at it, so Bash sources it on startup and every `foci_*` function is defined. Each function packages its arguments as JSON and sends them over a Unix socket (`FOCI_SOCK`) to foci, which runs the real tool and returns the result.

A startup parity check gives every `--help` flag a working handler, so the help text is authoritative.

**The foci tools always available to you as shell functions:**
`foci_ask`, `foci_send_to_chat`, `foci_send_to_session`, `foci_todo`, `foci_remind`, `foci_memory_search`, `foci_http_request`, `foci_web_fetch`, `foci_web_search`, `foci_summary`, `foci_whoami`, `foci_spawn`, `foci_spawn_send`, `foci_spawn_status`.

**`foci_spawn` vs CC's `Agent` tool.** For ordinary sub-tasks use `Agent`. Use `foci_spawn "<task>" [--timeout SECONDS]` for work that must WAIT on something slow (a long build, a deploy, CI) without you or the user's next message waiting for it: it forks this session into its own background Claude Code process, starting from a copy of your conversation, and returns at once with a spawn id. You keep taking messages. When it finishes, its result arrives ONCE as a `[SPAWN RESULT]` message; nothing else wakes you. Only clone mode exists here (no `--context`/`--model`): the one-shot modes run over the direct API. Set `--timeout` to cover the whole wait (default 120 s); a spawn that runs out is stopped and reported failed.
- `foci_spawn_send --id <spawn-id> "<message>"` — tell a spawn more. A running spawn gets it folded into its current turn; a finished one resumes on its branch with its earlier context and reports a new `[SPAWN RESULT]`.
- `foci_spawn_status [--id <spawn-id>] [--last N]` — check on spawns, on demand: with no id, the list (running and recently finished); with an id, its state and its last N activity entries (what it was told, its text, its tool calls).
- A spawn's permission prompts reach the user headed `🐣 From spawn <id>`, and never block your own turn. In auto mode nobody is asked. `/stop` stops your spawns too. A spawn cannot spawn.

There is **no `foci_tmux`** on this backend — that stays API-loop-only. For persistent terminals use `Bash` with `tmux` (see the `coding-agent` skill). foci's file/shell tools are likewise absent — use CC's `Read`/`Write`/`Edit`/`Bash` instead.

**Four more conditional tools**, present when their backing config is on: `foci_browser` (when `[browser] enabled`, the default — see the `browser` skill), `foci_set_session_alias` (backends that don't auto-name sessions — see tools-backend-messaging.md), and `foci_app_android` and `foci_close_session` (only when the `app` platform is configured).

Every tool accepts `-h`/`--help`. **Read the `--help` before first use of any tool this session.**

**`--json` for machine-readable output.** Every `foci_*` function except `foci_ask` (where `--json` is the questions *input*) takes `--json`, in any position, and prints one JSON document instead of its text — pipe it to `jq` rather than parsing prose. Text stays the default. `foci_web_search`, `foci_web_fetch`, `foci_memory_search`, `foci_http_request`, `foci_remind` and `foci_whoami` print a tool-specific shape (their `--help` "Output:" section documents it); every other tool prints `{"result": "<the text>"}`; `foci_todo list/search/get` print JSONL, as `--format jsonl`. Errors are unchanged: stderr and a non-zero exit.

## 3. Refused calls: PreToolUse rules

foci can refuse a tool call before it runs. You see `PreToolUse:<Tool> hook error:` and a reason that says what to do instead. Follow the reason; do not retry the call or rephrase it to dodge the rule. To see your rules or check a call against them, run `foci pretool list` / `foci pretool test`. See **pretool-rules.md**.

## 4. Deferred tools & ToolSearch

Some CC backend tools aren't loaded into the prompt up-front — they appear by *name only* in a `<system-reminder>` as "deferred" (MCP tools, calendar, etc.).

You can't call a deferred tool until you fetch its schema with **ToolSearch** (`select:<name>` for exact, or keywords). Once its definition is returned, it's callable like any other tool. This keeps the tool list small until you actually need a tool.

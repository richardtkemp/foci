<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Tools for backend (shell-based) agents

For agents on the **Claude Code (CC) backend** (most foci agents). You have two tool families at once: CC's own tools and foci's `foci_*` shell functions. This file is the entry point. The per-tool reference is in sibling files; read the one for what you are about to do:

| file | read it when you are about to… |
|---|---|
| **tools-backend-messaging.md** | talk to someone: ask the user (`foci_ask`), post to your chat (`foci_send_to_chat`), message another session (`foci_send_to_session`), name the chat (`foci_set_session_alias`), or close it (`foci_close_session`). |
| **tools-backend-state.md** | track work or recall: todos (`foci_todo`), reminders (`foci_remind`), searching memory and chat history (`foci_memory_search`). |
| **tools-backend-spawn.md** | run work in a sub-call or as another agent (`foci_spawn`), and steer, check, stop or reply to spawns. |
| **tools-backend-external.md** | reach outside: HTTP (`foci_http_request`), web pages and search (`foci_web_fetch`, `foci_web_search`), cheap-model extraction (`foci_summary`), a headless browser (`foci_browser`), the user's Android device (`foci_app_android`). |

## 1. CC-native tools

foci does **not** replace CC's own tools (`Read`, `Edit`, `Bash`, `Agent`, ...). Use them for files, shell, search and sub-agents.

## 2. foci tools (as shell functions)

foci exposes its own tools as `foci_*` **shell functions** you call through Bash (`foci_todo list --status open`). foci writes a functions file and points `BASH_ENV` at it; each function sends its arguments as JSON over the Unix socket `FOCI_SOCK`. A startup parity check gives every `--help` flag a working handler, so the help text is authoritative.

**Always available:**
`foci_ask`, `foci_send_to_chat`, `foci_send_to_session`, `foci_todo`, `foci_remind`, `foci_memory_search`, `foci_http_request`, `foci_web_fetch`, `foci_web_search`, `foci_summary`, `foci_whoami`, and the spawn tools `foci_spawn`, `foci_spawn_send`, `foci_spawn_status`, `foci_spawn_stop`, `foci_send_to_spawner` (see **tools-backend-spawn.md**).

There is **no `foci_tmux`** on this backend — that stays API-loop-only. For persistent terminals use `Bash` with `tmux` (see the `coding-agent` skill). foci's file/shell tools are likewise absent — use CC's `Read`/`Write`/`Edit`/`Bash` instead.

**Four more conditional tools**, present when their backing config is on: `foci_browser` (when `[browser] enabled`, the default — see the `browser` skill), `foci_set_session_alias` (backends that don't auto-name sessions — see tools-backend-messaging.md), and `foci_app_android` and `foci_close_session` (only when the `app` platform is configured).

Every tool accepts `-h`/`--help`. **Read the `--help` before first use of any tool this session.**

**`--json` for machine-readable output.** Every `foci_*` function except `foci_ask` (where `--json` is the questions *input*) takes `--json`, in any position, and prints one JSON document instead of its text — pipe it to `jq` rather than parsing prose. Text stays the default. `foci_web_search`, `foci_web_fetch`, `foci_memory_search`, `foci_http_request`, `foci_remind` and `foci_whoami` print a tool-specific shape (their `--help` "Output:" section documents it); every other tool prints `{"result": "<the text>"}`; `foci_todo list/search/get` print JSONL, as `--format jsonl`. Errors are unchanged: stderr and a non-zero exit.

## 3. Refused calls: PreToolUse rules

foci can refuse a tool call before it runs. You see `PreToolUse:<Tool> hook error:` and a reason that says what to do instead. Follow the reason; do not retry the call or rephrase it to dodge the rule. To see your rules or check a call against them, run `foci pretool list` / `foci pretool test`. See **pretool-rules.md**.

## 4. Deferred tools

Tools listed by name only in a `<system-reminder>` (MCP, calendar, ...) are deferred. Fetch the schema with **ToolSearch** (`select:<name>`) before you call one.

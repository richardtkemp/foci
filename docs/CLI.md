# Foci CLI Reference

The `foci` CLI is a lightweight client that talks to the `foci-gw` HTTP gateway. It is built separately:

```
go build ./cmd/foci
```

All commands communicate over HTTP to the gateway. The CLI auto-discovers the gateway's Unix socket at `<install root>/data/foci-gw.sock` (`$FOCI_HOME`, else `$HOME`; failing that, beside its own binary: `<root>/bin/foci` → `<root>/data/foci-gw.sock`, the macOS case, #2577) (same-user auth via kernel peer credentials — no API key needed). Falls back to TCP at `FOCI_ADDR` (default `127.0.0.1:18791`) with `FOCI_API_KEY` for remote/cross-user access.

---

## Global Flags

These flags are accepted by all commands. On the commands that take free text (`todo done`, `command`, `send`, `branch`, `eval`) they must come before the text: see [Flag placement](#flag-placement).

| Flag | Short | Env var | Description |
|------|-------|---------|-------------|
| `--help` | `-h` | | Show usage for any command (e.g. `foci send -h`). |
| `--socket <path>` | | `FOCI_GW_SOCK` | Gateway Unix socket path. Auto-detected from `<install root>/data/foci-gw.sock`, then from beside the binary (see above). No API key needed. An explicit `--socket` path that does not exist or is not a socket is an error naming the path; the CLI never falls back to TCP from it. (`FOCI_GW_SOCK` and the default path do fall back to TCP when no socket is there.) |
| `--addr <host:port>` | | `FOCI_ADDR` | Gateway TCP address. Default: `127.0.0.1:18791`. Used when no Unix socket is available and no `--socket` was given. |
| `--agent <id>` | `-a` | `FOCI_AGENT` | Target a specific agent. Default: first configured agent — `todo done` requires one (todo ids are per agent). |
| `--session <sel>` | `-s` | `FOCI_SESSION` | Target session selector: full session key, session name, or chat alias. Empty = the agent's default session. Send/branch/score read the env var; `command` accepts `-s` but does **not** read `FOCI_SESSION` (see `command`). |
| `--model <model>` | `-m` | `FOCI_MODEL` | Model override: group name (`powerful`/`fast`/`cheap`), model name, or `developer/model_id`. See [MODELS.md](MODELS.md). |
| `--if-warm <dur>` (`--if-active`) | | `FOCI_IF_WARM` (`FOCI_IF_ACTIVE`) | **Session-level**: skip if the target session has not run a turn within duration (e.g. `8h`, `30m`). A turn currently in flight always counts as active. |
| `--if-cold <dur>` (`--if-inactive`) | | `FOCI_IF_COLD` (`FOCI_IF_INACTIVE`) | **Session-level**: skip if the target session has run a turn within duration (e.g. `30m`, `1h`). Opposite of `--if-warm`; in-flight always counts. Standard keepalive shape. |
| `--if-user-active <dur>` | | `FOCI_IF_USER_ACTIVE` | **User-attention**: skip if the user has not touched this agent within duration. In-flight counts as user attention. |
| `--if-user-inactive <dur>` | | `FOCI_IF_USER_INACTIVE` | **User-attention**: skip if the user has touched this agent within duration. In-flight counts as user attention. |
| `--message-text <text>` | `-mt` | `FOCI_MESSAGE_TEXT` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | `FOCI_MESSAGE_FILE` | Read message from file path. |
| `--sync` / `--wait` | | `FOCI_SYNC` | Wait for the agent's reply, blocking the CLI (send/branch only, non-empty = true). This is NOT one of the `--wait-*` deferral gates below. |
| `--async` / `--no-wait` | | `FOCI_ASYNC` | Fire-and-forget (send/branch default, non-empty = true). |
| `--no-compact` | | `FOCI_NO_COMPACT` | Skip compaction (branch only, non-empty = true). |
| `--no-reset-hook` | | `FOCI_NO_RESET_HOOK` | Skip reset hook (branch only, non-empty = true). |
| `--oneshot` | | `FOCI_ONESHOT` | `--no-compact` + `--no-reset-hook` + `--silent` as a flag; the env var sets only the first two (branch only, non-empty = true). |
| `--wait-warm <dur>` (`--wait-active`) | | `FOCI_WAIT_WARM` | **Send/branch/command**: defer the request (never block the CLI) until the target session has run a turn within duration, then run it. Session-level. |
| `--wait-cold <dur>` (`--wait-inactive`) | | `FOCI_WAIT_COLD` | **Send/branch/command**: defer until the target session has NOT run a turn within duration, then run it. Opposite of `--wait-warm`. |
| `--wait-user-active <dur>` | | `FOCI_WAIT_USER_ACTIVE` | **Send/branch/command**: defer until the user has touched this agent within duration, then run it. User-attention. |
| `--wait-user-inactive <dur>` | | `FOCI_WAIT_USER_INACTIVE` | **Send/branch/command**: defer until the user has NOT touched this agent within duration, then run it. |
| `--wait-timeout <dur>` | | `FOCI_WAIT_TIMEOUT` | **Send/branch/command**: max time a deferred request waits before running anyway. Default `2h`. Alias `--deadline`. |
| `--no-gate` | | `FOCI_NO_GATE` | **Send/branch/command**: run now, ignoring any wait condition (and send's `--wait-cold 1m` default). Non-empty = true. |
| `--human` | | `FOCI_HUMAN` | **Send/branch/command**: declare a human (not a cron or script) sent this request. It counts as user attention (`--if-user-*` / `--wait-user-*` activity) once dispatched; a skipped or deferred request never does. Non-empty = true. |
| `--steer` | | `FOCI_STEER` | **Send**: redirect the agent now — fold into a running user turn; a running system turn (cron, keepalive, webhook, a modeless send) is waited out on an API-mode agent but CAN be folded into on a delegated (Claude Code/opencode/codex) agent; an idle session starts at once. Mutually exclusive with `--queue` (env value `steer`). See [Delivery mode](#delivery-mode-steer--queue). |
| `--queue` | | `FOCI_STEER` | **Send**: run as its own fresh turn, behind a running one if any (an idle session starts it at once; never plan feedback, never an ask answer). Mutually exclusive with `--steer` (env value `queue`). |

**Resolution order:** explicit flag > env var > default. Every flag has a corresponding `FOCI_` env var and vice versa. One exception: the delivery-mode pair `--steer`/`--queue` — giving a flag plus the *other* mode's `FOCI_STEER` value is a usage error (two live sources disagreeing), rather than the flag silently winning.

## Flag placement

`todo done`, `command`, `send`, `branch` and `eval` take free text: a close reason, a slash command, a message, a branch text or a shell command. On these commands, flags go **before** the text (Dick, 2026-10-17):

- The connection flags (`--socket`, `--addr`, `--api-key`) go before the command word.
- The command's own flags (`-a`, `-s`, the gate and wait flags, `--human`, and so on) go after the command word and before the text. `todo done` has one own flag, `-a`, before the id.
- The text starts at the first word that is not one of those flags. Every word from there on is text, kept as typed, whatever it looks like. `foci send hello -a research` sends `hello -a research` to the default agent. `foci command -a helen /reset --if-warm 8h` dispatches `/reset --if-warm 8h` with no gate. A `-h` inside the text is text; only `-h` before the text prints the usage.

Three shapes are errors, and nothing is sent:

- a connection flag after the command word (`foci send --addr h:p hello` → `--addr must come before the send command word`);
- a value flag that ends the line with no value (`foci send --if-warm` → `flag --if-warm needs a value`);
- a second `-a`/`--agent` pair before the text (`only one -a/--agent may precede the text`).

Every other command reads its flags anywhere on the line (`foci pair example.com -a helen` selects helen). On `ping` and `pair`, a value flag at the end of the line stays a word (`foci ping -s` dispatches `/-s /ping`).

## Environment Variables

Setting env vars is useful for crontab entries where the same agent/session is targeted repeatedly:

```crontab
FOCI_AGENT=clutch
FOCI_IF_WARM=4h
*/30 * * * * foci send -mf /home/foci/shared/prompts/reflection.md
0 7 * * * foci branch --oneshot -mf /home/foci/shared/prompts/morning-routine.md
```

---

## Commands

### `send` — Send a message to the agent

Sends a text message to the agent's default session (or a named session / chat alias / exact session key). By default, send is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to the chat. Use `--sync`/`--wait` to block until the response is available.

`--broadcast` delivers the agent's response to **every surface** for the agent — each platform's default destination (telegram default chat, the app's default conversation, else its newest — auto-created if the agent has none) — instead of only the target session's chat. Useful for "heads up" crons that must reach you wherever you are. Equivalent to `policy=broadcast` in the request (or embedded in the selector: `-s 'research?policy=broadcast'`).

Every response carries a **routing receipt** — which session the message resolved to and which resolution rung matched (`exact`, `named`, `alias`, `created`, `default`). The CLI prints it to stderr as one timestamped line (`2026-10-11T09:00:00Z session: clutch/iresearch (named) status=` — `status=` carries `deferred` for a queued request, empty otherwise), so cron logs show where a send actually landed, dated, instead of trusting silent fallbacks. An async send accepted while the target session is busy with background work (a running subagent or background command) adds `held=background_work` to that stderr line — the message is queued all the same and runs when that work finishes; stdout stays the bare `queued` token either way.

**Wait gates (deferral, not blocking):** an unmet `--wait-*` gate never blocks the CLI — the gateway stores the request (it survives a restart), answers with a "deferred" receipt immediately (even with `--sync`), and delivers it once the condition holds; `--wait-timeout`/`--deadline` (default **2h**) delivers anyway. A send with no if/wait flag defaults to `--wait-cold 1m` (waits for 1m of session idleness first); `--no-gate` opts out of the default and any wait condition.

**Delivery mode (`--steer` / `--queue`):** chooses what the message does when the agent is already mid-turn. `--steer` folds into the running turn at its next tool boundary. On an API-mode agent that is only a turn a user started (a chat message or a choice-carrying send): a system turn in flight (cron, keepalive, webhook, a plain `foci send` with no mode) is never folded, and the message waits for it to finish and then runs as its own fresh turn. On a delegated agent (Claude Code, opencode, codex) the backend's own in-flight flag also counts, so a `--steer` CAN fold into a running cron/keepalive/webhook turn. An idle session delivers at once. `--queue` always runs as its own fresh turn, behind a running one if any. Without `--human`, neither mode ever answers for the human: it is never consumed as plan feedback, an ask answer or a CC re-login code. With `--human`, `--steer` is treated like a typed chat reply and CAN be captured as the answer to a pending ask, as plan feedback or as the re-login code; `--queue` is never consumed by any of them. This holds whether the message is delivered at once, folded or buffered mid-turn and replayed as a follow-up turn. An explicit mode skips only send's implicit `--wait-cold 1m` default — explicit `--if-*`/`--wait-*` gates still apply verbatim (a deferred mode-carrying send keeps its mode), and `--no-gate` still skips everything. With `--sync`, a mode-carrying send against a session with a turn in flight returns the 202 "queued" receipt immediately — the reply belongs to the turns, so the CLI does not block for it — while an idle session blocks and returns the reply as usual (the mode is inert with nothing in flight). With `--broadcast` the mode is ignored. Both flags take their value from `FOCI_STEER` (`steer`/`queue`; other values are ignored); giving both — two flags, or a flag plus the other switch's env value — is a usage error.

Flags go before the message: see [Flag placement](#flag-placement).

**Usage:**
```
foci send [-a agent] [-s session] [-m model] [--steer | --queue] [--if-warm <duration>] [--if-cold <duration>] [--if-user-active <duration>] [--if-user-inactive <duration>] [--sync] [-mt text | -mf file] [message text]
```

**Flags:**

| Flag | Short | Description |
|------|-------|-------------|
| `--agent <id>` | `-a` | Target agent. |
| `--session <sel>` | `-s` | Target session selector, resolved through one ladder on the server: full session key (`clutch/c123`) → existing named session (`research` → `clutch/iresearch`) → chat alias → create-named. Empty = the agent's default session. |
| `--model <model>` | `-m` | Model override for this request. Group name (`powerful`/`fast`/`cheap`), model name (`opus`), or `developer/model_id`. |
| `--if-warm <dur>` (`--if-active`) | | **Session-level gate**: skip if the target session has not run a turn within duration. Go duration format (e.g. `8h`, `30m`). A turn currently in flight always counts as active. |
| `--if-cold <dur>` (`--if-inactive`) | | **Session-level gate**: skip if the target session has run a turn within duration. Opposite of `--if-warm` — keepalive shape; in-flight always counts. |
| `--if-user-active <dur>` | | **User-attention gate**: skip if the user has not touched this agent within duration. CLI/cron/agent-to-agent traffic does not count — unless sent with `--human`. |
| `--if-user-inactive <dur>` | | **User-attention gate**: skip if the user has touched this agent within duration. |
| `--wait-warm <dur>` (`--wait-active`) | | **Deferral gate**: defer until the target session has run a turn within duration (in-flight counts), then send. |
| `--wait-cold <dur>` (`--wait-inactive`) | | **Deferral gate**: defer until the target session has been idle the whole duration, then send. |
| `--wait-user-active <dur>` | | **Deferral gate**: defer until the user has touched this agent within duration, then send. |
| `--wait-user-inactive <dur>` | | **Deferral gate**: defer until the user has NOT touched this agent within duration, then send. |
| `--wait-timeout <dur>` (`--deadline`) | | Max deferral before sending anyway. Default **2h**. |
| `--no-gate` | | Send immediately: no wait default, no wait condition. |
| `--steer` | | Redirect the agent now: fold into a running user turn (a running system turn too on delegated agents; on API agents it is waited out); idle starts at once (env: `FOCI_STEER=steer`). |
| `--queue` | | Run as its own fresh turn, behind a running one if any (an idle session starts it at once; env: `FOCI_STEER=queue`). |
| `--human` | | Declare a human sent this: counts as user attention once dispatched; skipped/deferred requests never do. |
| `--sync` / `--wait` | | Wait for the agent's reply instead of returning immediately. Not a `--wait-*` gate. |
| `--async` / `--no-wait` | | Fire-and-forget mode (default). Returns immediately, response goes to Telegram. |
| `--message-text <text>` | `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` | `-mf` | Read message from file. Sends the file contents as the message. |

Trailing args without a flag are treated as implicit `--message-text`. Cannot use both `-mt` and `-mf`.

**Examples:**
```bash
# Send a message (async, returns immediately with "queued")
foci send "check the weather forecast"

# Send and wait for the response
foci send --sync "check the weather forecast"

# Equivalent using explicit flag
foci send -mt "check the weather forecast"

# Send file contents as the message
foci send -mf /home/foci/shared/prompts/reflection.md

# Send to a specific agent
foci send -a research "summarize today's news"

# Send to a named session
foci send -a clutch -s research "continue the analysis"

# Only send if the session ran a turn in the last 8 hours (for cron jobs)
foci send --if-warm 8h "daily health check"

# Only send if the user has touched this agent in the last 4 hours
foci send --if-user-active 4h "follow-up question"

# Send with a model override (use fast group model)
foci send --model fast "quick question"

# Send with a specific model name
foci send -m opus "think carefully about this"

# Send file contents with session-level activity gating
foci send -a clutch --if-warm 4h -mf tasks/review.md

# A human-typed send: counts as user attention once dispatched
foci send --human "checking in on this thread"

# Redirect the agent NOW: folds into a running user turn (an idle session
# starts at once; on an API-mode agent a system turn in flight — cron,
# keepalive, a modeless send — is waited out, then the message runs as its
# own turn; on a delegated agent it can fold into that system turn)
foci send --steer "stop digging, use the other approach"

# Queue behind the running turn as its own fresh turn (never an ask answer)
foci send --queue "once you finish that, also check the logs"

# A mode skips only send's implicit --wait-cold 1m default: the message is
# delivered to the session now (not deferred); on a busy session it then
# queues behind the running turn. An explicit --wait-* gate would still
# defer it, mode kept.
foci send --queue "follow-up while you work"
```

**Exit codes:** 0 on success, 1 on error (network failure, HTTP error).

---

### `branch` — Fork a branch session

Creates a branch session from the agent's default session — or from the session `-s/--session` names (a full session key, a session name, or a chat alias) — optionally injects a message, and runs the agent on the branch. The parent session is not interrupted: a copy of its context answers. Used for cron jobs and background tasks that shouldn't pollute the main conversation.

A `-s` name that matches no key, named session or alias creates a **new, empty session** and branches that, so the branch runs with no context. A backend that cannot branch (rare — every shipped backend can) refuses a named session with an error rather than interrupting it; only the agent's default session falls through to a plain send.

By default, branch is **asynchronous** (fire-and-forget): the CLI returns immediately with "queued" and the agent's response is delivered to Telegram. Use `--sync`/`--wait` to block until the response is available.

**Wait gates (deferral, not blocking):** `foci branch` accepts the same `--wait-*` gates as `foci send`, evaluated against the PARENT session. Unlike send there is **no default gate**: a branch with no if/wait flag runs immediately. An unmet `--wait-*` gate never blocks the CLI — the gateway stores the branch request (it survives a restart), answers with a "deferred" receipt immediately (even with `--sync`), and creates the branch once the condition holds, forking from the parent's state at that moment; `--wait-timeout`/`--deadline` (default **2h**) branches anyway. `--no-gate` ignores any wait condition. One gated request never defers: on a backend that cannot branch (rare — every shipped backend can), a `--wait-*` gate with `-s` naming anything but the default session is refused immediately with the same error as an ungated branch — nothing is queued, because the deferral could only ever end in that refusal.

Flags go before the branch text: see [Flag placement](#flag-placement).

**Usage:**
```
foci branch [-a agent] [-s session] [-m model] [--if-warm <duration>] [--if-cold <duration>] [--if-user-active <duration>] [--if-user-inactive <duration>] [--wait-warm <duration>] [--wait-cold <duration>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--agent <id>` / `-a` | Target agent. |
| `--session <sel>` / `-s` | Parent session selector, resolved through one ladder on the server: full session key (`clutch/c123`) → existing named session (`research` → `clutch/iresearch`) → chat alias → create-named. An unknown name creates a new, empty session (the branch then has no context). Empty = the agent's default session. Env `FOCI_SESSION`. |
| `--model <model>` / `-m` | Model override for this branch. Group name, model name, or `developer/model_id`. |
| `--if-warm <dur>` (`--if-active`) | **Session-level gate**: skip if the target session has not run a turn within duration. In-flight counts. |
| `--if-cold <dur>` (`--if-inactive`) | **Session-level gate**: skip if the target session has run a turn within duration. In-flight counts. Keepalive shape. |
| `--if-user-active <dur>` | **User-attention gate**: skip if the user has not touched this agent within duration. |
| `--if-user-inactive <dur>` | **User-attention gate**: skip if the user has touched this agent within duration. |
| `--wait-warm <dur>` (`--wait-active`) | **Deferral gate**: defer until the parent session has run a turn within duration (in-flight counts), then branch. |
| `--wait-cold <dur>` (`--wait-inactive`) | **Deferral gate**: defer until the parent session has been idle the whole duration, then branch. |
| `--wait-user-active <dur>` | **Deferral gate**: defer until the user has touched this agent within duration, then branch. |
| `--wait-user-inactive <dur>` | **Deferral gate**: defer until the user has NOT touched this agent within duration, then branch. |
| `--wait-timeout <dur>` (`--deadline`) | Max deferral before branching anyway. Default **2h**. |
| `--no-gate` | Branch now: ignore any wait condition. |
| `--human` | Declare a human sent this: counts as user attention on the new branch session once it runs. |
| `--sync` / `--wait` | Wait for the agent's reply instead of returning immediately. Not a `--wait-*` gate. |
| `--async` / `--no-wait` | Fire-and-forget mode (default). Returns immediately, response goes to Telegram. |
| `--no-compact` | Skip compaction if context limit is reached during the branch. |
| `--no-reset-hook` | Skip the pre-reset memory hook when the branch session is reclaimed. |
| `--oneshot` | Shorthand for `--no-compact --no-reset-hook --silent`. For quick fire-and-forget tasks. |
| `--message-text <text>` / `-mt` | Explicit message text (alternative to trailing args). |
| `--message-file <path>` / `-mf` | Read message from file. Sends the file contents as the message. |

**Examples:**
```bash
# Fork a branch for a background task (returns immediately)
foci branch -a clutch "run your morning routine"

# Fork and wait for the response
foci branch --sync -a clutch "run your morning routine"

# Quick one-shot task (no compaction, no reset hook)
foci branch --oneshot -a clutch "check disk space and report"

# Branch with a cheap model for routine work
foci branch --oneshot -m cheap -a clutch "run health check"

# Branch with message from file
foci branch --oneshot -a scout -mf /home/foci/shared/prompts/daily-health-check.md

# Only branch if the session has been active recently (in-flight counts)
foci branch --if-warm 12h -a clutch "daily memory review"

# Only branch if the user has reached out recently
foci branch --if-user-active 4h -a clutch "follow-up on this morning's chat"

# Branch once the parent session has been cold for 55m (deferred, not skipped)
foci branch --wait-cold 55m --oneshot -a helen "nightly maintenance"

# A human-typed branch: the branch session counts as touched by its user
foci branch --human --oneshot -a clutch "side quest while we talk"

# Ask a named session a question without interrupting it (reply on stdout)
foci branch -a clutch -s Fabro --sync --silent "question"

# Empty branch (agent wakes up with fork context only)
foci branch -a research
```

**Exit codes:** 0 on success, 1 on error. Returns HTTP 412 if the agent has no default session yet (no Telegram chat has been started), and HTTP 422 if `-s` names a session (other than the default) on an agent whose backend cannot branch — with or without a `--wait-*` gate (a gated request is refused at once, not deferred).

---

### `status` — Query agent status

Returns the agent's current status including session info, model, uptime, and whether the agent is processing.

**Usage:**
```
foci status [-a agent]
```

**Examples:**
```bash
foci status
foci status -a research
```

---

### `score` — Record a human score on a turn

Attaches a score to the turn's Langfuse trace, validated against the rubric of that name when one is loaded (`docs/EVALS.md`). Targets the session's last completed turn unless `--turn` names one.

**Usage:**
```
foci score [-a agent] [-s session] [--turn id] [--obs id] [--user who] <name> <value> [comment...]
```

**Examples:**
```bash
foci score -a fabulo quality 4 "good, but hedged the deploy question"
foci score --turn 'clutch/c123@1789811615564559571' honesty yes
```

---

### `evals` — List scoring rubrics

```
foci evals list [-a agent]
```

Lists the loaded rubrics (name, version, kind, value shape, agents, whether the Langfuse score config is mirrored) and any files that failed to load. `-a` narrows to the human rubrics a score control would offer that agent.

---

### `eval` — Run a shell command via the agent

Wraps a shell command in a prompt asking the agent to execute it and show the output. The command is sent as a message to the agent's main session. Always synchronous (waits for the response).

`-a/--agent` goes before the shell command: see [Flag placement](#flag-placement). `foci eval grep -a foo file.txt` runs `grep -a foo file.txt` and selects no agent.

**Usage:**
```
foci eval [-a agent] <shell command>
```

**Examples:**
```bash
foci eval "df -h"
foci eval -a research "git log --oneline -5"
```

Note: The agent must have the `exec` tool available to actually run the command.

---

### `command` — Dispatch a slash command

Dispatches a slash command directly via the HTTP API and returns the result to the CLI caller. The leading `/` is added automatically if omitted.

**Important:** This bypasses the agent conversation entirely. The agent never sees the command or its output — the command is executed by the gateway's command handler and the result is returned only to the CLI. This is useful for observability and diagnostics without polluting the chat context.

**Usage:**
```
foci command [-a agent] [-s session] [--if-warm <dur>] [--if-cold <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] [--wait-warm <dur>] [--wait-cold <dur>] </cmd> [args]
```

**Target session:** `-s/--session` names the session the command runs on — a full session key, a session name, or a chat alias, resolved through the same server-side ladder as `foci send` (a name nothing matches creates a new, empty session). Unlike send, `foci command` does **not** read `FOCI_SESSION`: a cron or script that happens to have it exported keeps dispatching on the agent's default session. With no `-s`, the command runs on the default session.

Every response carries the same **routing receipt** as `send` — which session the command resolved to and which resolution rung matched (`exact`, `named`, `alias`, `created`, `default`). The CLI prints it to stderr as one timestamped line (`2026-10-11T09:00:00Z session: clutch/ireserch (created) status=`), so a mistyped `-s` is visible in cron logs instead of silently running on a brand-new empty session.

**Activity gates:** `command` accepts the same four activity-gate flags as `send` (and reads the same `FOCI_IF_*` env vars). The gate is evaluated server-side against the session the command targets; a turn in flight always counts as active, so a gated command never interrupts mid-turn work.

**Wait gates (deferral, not blocking):** `command` also accepts the `--wait-*` gates (`--wait-warm`, `--wait-cold`, `--wait-user-active`, `--wait-user-inactive`, `--wait-timeout`/`--deadline`, `--no-gate`; same `FOCI_WAIT_*` env vars), evaluated against the session the command targets. Unlike send there is **no default gate** — a command with no if/wait flag runs immediately. An unmet `--wait-*` gate does not skip and does not block: the gateway stores the command (it survives a restart), answers with a "deferred" receipt immediately, and dispatches it once the condition holds; `--wait-timeout`/`--deadline` (default **2h**) dispatches anyway. `--no-gate` ignores any wait condition.

**`--human`:** declare a human (not a cron) dispatched this command. A command starts no turn, so the declaration itself stamps the user-attention timestamp on the command's target session once the command dispatches. A command that is skipped, deferred or fails to dispatch records nothing.

Flags go before the slash command: see [Flag placement](#flag-placement). Several words are joined with single spaces. To keep the exact spacing, pass the slash command as ONE quoted argument (`foci command -a clutch "/todo done 7 landed --if-active 8h"` records the reason `landed --if-active 8h`).

**Examples:**
```bash
foci command /cache
foci command -a research /status
foci command /config available
foci command /cost today

# Overnight reset that skips if the session ran a turn within 55m (or one is
# in flight):
foci command --if-cold 55m -a helen /reset

# Overnight reset that RUNS once the session has been cold for 55m — the
# deferred shape: instead of skipping and hoping the next cron tick lands,
# the gateway holds the /reset and dispatches it at the moment of idleness:
foci command --wait-cold 55m -a helen /reset

# A human-typed command: the session counts as touched by its user
foci command --human -a helen /reset

# Close a ticket with flag-looking reason words: words after the slash
# command are dispatched typed exactly, so --if-active is reason text
# here, not a gate on the close (quoted or unquoted alike); quoting keeps
# the exact spacing
foci command -a clutch /todo done 2457 "landed 3fc608953 (factory run 42)"
```

---

### `todo` — Close a todo with a reason

**Usage:**
```
foci todo done [-a agent] <id> <reason...>
```

Marks one todo of the named agent done through the gateway, recording the reason as the todo's close reason — the script-facing counterpart of the agent's `foci_todo complete`, shown by `/todo show` and `foci_todo get`. The reason is stored byte for byte — any bytes, not only valid text, up to what a command line can carry (the kernel allows 128 KiB per argument and about 2 MiB across the whole command line); pass it as one quoted argument to keep its exact spacing. The id is digits only (`007` is ticket 7; `+7` is refused as a bad id — the id grammar the `/todo done` slash command shares).

Flags: `-a/--agent` goes before `<id>`, and the connection flags before the `todo` word (see [Flag placement](#flag-placement)). Everything after `<id>` is the reason, kept as typed: `-a`, `--addr` and `-n` there are reason words. An empty reason is refused; a reason of spaces only is accepted and stored as-is, like the agent todo tool. The agent is required: todo ids are per agent, so there is no first-agent default. Name it with `-a/--agent` or `FOCI_AGENT`. Exits 0 on success. Exits non-zero with one stderr line naming the todo if the todo or agent does not exist, the reason is empty, or the gateway is unreachable (one attempt, no retry); and with a line naming the path if an explicit `--socket` is not a socket. This is what services without an agent exec, like the factory's landing, use to close their tickets.

**Example:**
```bash
foci todo done -a clutch 2457 "landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)"
```

---

### `secrets` — Manage secrets

Manage secrets in `secrets.toml` without a running gateway. Useful for initial setup, scripting, and CI.

**Usage:**
```
foci secrets <subcommand> [--config <path>] [args...]
```

**Subcommands:**

| Subcommand | Description |
|-----------|-------------|
| `list` | List all secret names (no values printed) |
| `get <section.key>` | Print secret value to stdout (pipe-friendly, no decoration) |
| `set <section.key> <value>` | Add or update a secret |
| `delete <section.key>` | Remove a secret |

**Flags:**
- `--config` — path to `foci.toml` (secrets.toml is resolved alongside it). Default: `~/config/secrets.toml`.

**Examples:**
```bash
# List all secret names
foci secrets list

# Set a secret
foci secrets set custom.github_token ghp_abc123

# Read a secret (pipe-friendly)
foci secrets get custom.github_token | pbcopy

# Delete a secret
foci secrets delete custom.github_token

# Use a custom config directory
foci secrets --config /etc/foci/foci.toml list
```

---

### `auth` — Set API key for an LLM provider

Save an API key to `secrets.toml`. If a gateway is running, the new credentials are hot-reloaded immediately.

**Usage:**
```
foci auth [--provider NAME] [--api-key KEY] [--config PATH] [--addr HOST:PORT]
```

**Flags:**
- `--provider` — provider name: `anthropic`, `gemini`, `openai`, `openrouter` (default: `anthropic`).
- `--api-key` — API key (prompted interactively if omitted).
- `--config` — path to foci.toml (secrets.toml is written alongside it). Default: `~/config/secrets.toml`.
- `--addr` — gateway address for hot-reload notification. Env: `FOCI_ADDR`. Default: `127.0.0.1:18791`.

**Examples:**
```bash
foci auth                                          # interactive: prompts for API key
foci auth --provider openai --api-key sk-...       # non-interactive
foci auth --config /etc/foci/foci.toml             # custom config directory
foci auth --addr 10.0.0.1:18791      # notify remote gateway
```

---

### `ping` — Liveness check

Shorthand for `foci command /ping`. Returns "pong" with a timestamp if the gateway and agent are running.

**Usage:**
```
foci ping [-a agent]
```

**Examples:**
```bash
foci ping
foci ping -a research
```

---

### `debug` — Tail session files

Tails the session files for a given session key with formatted output. Useful for live debugging of what an agent is writing to its session JSONL.

**Usage:**
```
foci debug session <key>
```

**Examples:**
```bash
foci debug session clutch/c12345
```

---

### `pretool` — Check PreToolUse rules offline

Runs sample tool calls past an agent's PreToolUse deny rules (CONFIG.md → `[[cc_backend.pretool_rules]]`) without a gateway or a CC session. The config file is loaded and validated as the gateway loads it, and the agent's rules are resolved as they are at each CC launch, so what `test` reports is what the next session will enforce.

**Usage:**
```
foci pretool list [--agent <id>] [--config <path>]
foci pretool test [--agent <id>] [--config <path>] (--bash <command> | --tool <name> [--input <json>]) [--cwd <dir>] [-v]
foci pretool test [--agent <id>] [--config <path>] --all [-v]
```

`list` prints the resolved rules. `test` prints the name of the rule that denies the call, or `no match`. `-v` adds the rule's reason and, for Bash, each simple command the `command` patterns are matched against, with its shell facts in brackets (background, subshell, output, `dir=` relative to the cwd or `?` if unknown, `op=`, `pipe=`; CONFIG.md → Shell facts). `--bash -` reads the command from stdin. Rules' `when` checks run for real, in `--cwd` (default: the current directory); a check that fails open is always printed as `when failed open: ...`. `--agent` defaults to the agent in `FOCI_SESSION_KEY`; `--config` to `$FOCI_CONFIG`, else `~/config/foci.toml`.

`test --all` (#2039) runs every rule's own examples (`deny_examples` / `allow_examples`, CONFIG.md → Test examples) against the agent's resolved set: one line per failed example — the rule, the example, the expected rule and the rule that matched instead (or `nothing`) — plus a summary count. Passing examples print nothing unless `-v`, which adds one `ok` line each (naming the rule that legally matched an allow example). It exits non-zero if any example failed. `when` checks run for real here, as the invoking user (never root), so each `when` rule's real decision is tested; `--all` cannot be combined with `--bash`, `--tool`, `--input` or `--cwd`. (`foci-gw -check-config` runs the same examples but never evaluates a `when` script.)

**Examples:**
```bash
foci pretool list --agent clutch
foci pretool test --agent clutch --bash 'cd /r && git add -A'
foci pretool test --agent clutch --bash 'git commit -m x' --cwd /home/rich/git/foci -v
foci pretool test --agent clutch --tool Read --input '{"file_path":"/etc/passwd"}'
foci pretool test --agent clutch --tool Edit --input '{"file_path":"/home/foci/shared/skills/foci-usage/SKILL.md"}'
foci pretool test --agent clutch --all
```

---

### `pair` — Mint Android pairing key

Mints a single-use Android pairing key for registering a new device with the gateway. The key can be used once to complete the `/android` onboarding flow.

**Usage:**
```
foci pair
```

---

## Cron Integration

The CLI is designed for cron jobs. Both `send` and `branch` default to async mode — the cron job returns immediately and the agent's response is delivered to Telegram. Typical patterns:

```crontab
# Daily morning routine — only if the user has reached out in the last 24h
0 7 * * * /home/foci/bin/foci branch --if-user-active 24h -a clutch "run your morning routine"

# Hourly health check — only if the user has been around recently
0 * * * * /home/foci/bin/foci send --if-user-active 8h -a clutch "quick health check"

# Nightly one-shot task (no compaction overhead)
0 2 * * * /home/foci/bin/foci branch --oneshot -a clutch "nightly cleanup"

# Keepalive — only if the session has been idle for 30+ minutes (yields to in-flight turns and recent work)
*/30 * * * * /home/foci/bin/foci branch --oneshot --if-cold 30m -a clutch "Check emails and calendar"

# Force sync if you need the output in the cron log
0 6 * * * /home/foci/bin/foci send --sync -a clutch "morning report" >> /var/log/foci-report.log
```

**Choosing the right gate:**

- `--if-user-active` / `--if-user-inactive` track *user attention* (real platform inbound — Telegram/Discord — or an HTTP request declared human with `--human`). Use these for nudges that should only fire when the user is engaged or specifically away.
- `--if-warm` (`--if-active`) / `--if-cold` (`--if-inactive`) track *session activity* — whether any turn (user, cron, CLI, agent-to-agent) ran on the session, plus an in-flight short-circuit so a turn currently running always counts as active. Use these for keepalives that must yield to running work — they prevent crons piling up behind a long turn.

## HTTP API

The CLI is a thin wrapper around the HTTP gateway. For direct integration, see the endpoint documentation in [WIRING.md](WIRING.md#http-gateway-maingo).

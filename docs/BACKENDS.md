# Coding Agent Backends — API vs Delegated

Foci runs each agent's turns through one of two very different code paths. This doc explains what they are, how they differ, and when to pick which. For the wiring details, see [WIRING.md](WIRING.md). For the exact config keys, see [CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends).

## The two paths

Every `[[agents]]` entry has a `backend` field. It selects one of two turn-handling transports, both implementing the same 20-method `TurnContract` interface (`internal/agent/turn_contract.go`):

- **`backend = "api"` (default) — API transport.** Foci calls the LLM API directly, executes tools in-process, and manages the session history.
- **`backend = "claude-code"` or `"claude-code-tmux"` — Delegated transport.** Foci spawns Claude Code as a subprocess. CC handles inference, tool execution, and its own context management; Foci feeds it prompts and reads back the assistant output.

The orchestrator (`OrchestrateFullTurn`) calls both transports through the same interface, which is why nearly every Foci feature — reminders, scratchpad, todos, tasks, nudges, multi-platform delivery, steering — works identically on both paths.

## What each owns

| Concern | API backend | Delegated backend |
|---|---|---|
| LLM inference | Foci → `provider.Send` | CC subprocess |
| Tool registry & execution | Foci's `tools/` package | CC's built-in tools + MCP |
| Session history | Foci-managed JSONL in `data/sessions/` | CC-managed JSONL in `~/.claude/projects/` |
| Context / compaction | Foci `compaction/compact.go` | CC's own context manager (triggered via `/compact`) |
| Rate limiting | Foci per-endpoint gate | CC's own limiter |
| Turn serialization | Foci per-session lock | CC serializes internally |
| Model fallback chain | Foci `[groups.fallbacks]` | CC picks its own fallback |
| Prompt caching | Foci's append-only cache contract | CC manages its own cache |
| Branching / `/branch` | Full support | **Rejected** (HTTP 400) |

Anything not in this table — platform I/O, command dispatch, nudges, reminders, task list, memory search, attachments, message transforms — is identical across both backends because it happens outside `RunInference`.

## Delegated backend flavours

There are two implementations of the delegated path. They share the `TurnContract` surface, the `DelegatedManager` plumbing, and the permission system — they only differ in how they talk to the `claude` binary.

### `claude-code` — ccstream (preferred)

Structured NDJSON over stdin/stdout. CC runs with `--input-format stream-json --output-format stream-json --permission-prompt-tool stdio`. Each wire message is one JSON object; the `type` field (`user`, `assistant`, `result`, `system`, `control_request`, `tool_progress`, `stream_event`) discriminates the kind.

Pros: no tmux dependency, no screen-scraping, structured permission prompts, precise turn boundaries via `result` messages, token-level streaming via `stream_event`, clean `/stop` via `control_request` interrupt, per-tool completion hooks (`foci-cc-hook`) give real-time tool_result visibility.

ccstream uses a **two-lifetime callback split** (TODO #747): `SessionEvents` (delivery — `OnText`, `OnTextDelta`, `OnThinkingDelta`, `OnToolStart`, `OnToolEnd`) is installed once per session via `Backend.AttachSessionEvents` and stored in an `atomic.Pointer` that's never nil after first attach, so text/tool emission paths never drop on a per-turn handler nilling. `TurnEvents` (bookkeeping — `OnTurnComplete`, `PostToolNudgeFunc`, `PreAnswerNudgeFunc`) is installed via `Inject.Turn` and cleared in `OnResult`. The pre-TODO #747 design bundled both into one combined per-turn handler that nilled per-turn — its replacement isn't optional, it's the structural fix that makes "the turn ended but CC kept emitting" handle correctly. cctmux implements the same split: its JSONL watcher dispatches delivery into `SessionEvents` and completion into `TurnEvents` on the `Backend`. See [WIRING.md — ccstream Backend](WIRING.md#ccstream-backend-internaldelegatorccstream).

### `claude-code-tmux` — cctmux (legacy)

CC runs interactively in a tmux pane. Foci pastes input via `load-buffer` / `paste-buffer` and tails CC's session JSONL file via fsnotify for output. Still supported; used when you want a human-visible CC pane or need CC's full TUI (for interactive slash commands, `/login` flows, etc.).

Pros: the pane is a real terminal — you can attach, observe, and interact directly. Useful for debugging or when the agent needs a human to take over momentarily.

Cons: screen-scraped permissions, JSONL file-watching for turn boundaries, send-keys based `/stop`, tmux as a hard dependency.

**Unless you specifically need the interactive pane, use `claude-code`.**

## What still applies on the delegated path

All of this works unchanged when you delegate to CC:

- **Reminders, scratchpad, todos, task list** — Foci-side state, injected into each prompt as text blocks.
- **Nudges** — regex and every-N-turn triggers prepend to the user message.
- **Message metadata** — `[meta]` / `[reminders]` / `[state]` prefix is composed by `composeTurnText` and joined into flat text via `JoinPrompt()` (instead of rich content blocks).
- **Platform connections** — Telegram, Discord, Android, HTTP, voice — the reply stream is the same.
- **Command dispatch** — `/sessions`, `/config`, `/mana`, `/stop`, `/reset`, `/facet`, etc. Foci handles them normally. `/model` goes via the ControlSender pattern. `/compact` — both manual (`/compact` command) and auto (threshold / mana-refresh) — dispatches through `Agent.runDelegatedCompact`, which sends `/compact <foci-summary-prompt>` to CC and waits for the `compact_boundary` stream event. `/pass` and a small set of other forward-only commands (e.g. unhandled CC slash commands) are sent to the backend via `Backend.Inject(SourcePass)` — a fire-and-forget send that bypasses the turn handler so a forwarded `/context` doesn't get treated as a user turn.
- **Attachments** — images and documents become `[Image saved to: ...]` path annotations so CC can `Read` them from disk.
- **Steering** — mid-turn user messages are dispatched directly via `Backend.Inject(SourceSteer)`, which sends the text via `writer.SendUser` at queue priority `"now"`. CC's mid-turn drain (`claude-code/src/query.ts:1570-1589`) folds the message into the current `ask()` as an attachment to the next tool-result batch — the model addresses it in the same turn, the in-flight tool finishes naturally, and the response reaches the original handler. Steer no longer aborts the in-flight turn; for "stop right now" semantics use `/reset hard`. The agent's per-session `Inbox.Enqueue` handles the routing decision — it calls `Inject(SourceSteer)` directly for CC backends; the steer buffer is only used by API-mode agents.
- **Memory formation** — injected into the live CC session as a prompt (not branched).
- **Memory consolidation / nudge extraction** — run via `RunOnce` (`claude --print`), a headless one-shot subprocess with no tmux / no watcher / no session.
- **Auto-approval** — foci-level `[permissions]` rules are checked before any CC permission request reaches the user. Plus a static `--allowedTools` list at CC launch (merged from `[cc_backend] default_allowed_tools` and per-agent `backend_config.allowed_tools`) for rules CC can evaluate without a round-trip.

## What's skipped on the delegated path

These are no-ops or handled by CC:

- **Foci's tool registry** — CC has its own tools; Foci's `tools/` package is not consulted.
- **Compactor** — `RunCompaction` sends `/compact` to CC instead of running Foci's compaction pipeline.
- **Cache management** — CC manages its own prompt cache.
- **Fallback chain** — `[groups.fallbacks]` has no effect; CC picks its own fallback.
- **Server tools** — web search, web fetch, etc. are provided by CC's own tools, not Anthropic server tools.
- **MCP** — Foci's MCP integration is unused; add MCP servers to CC directly.
- **Spawn / sub-agent tool** — CC's own `Agent` tool replaces Foci's spawn mechanism.
- **Session branching** — `/branch` returns HTTP 400. CC sessions cannot be branched.
- **Session repair** — `LoadAndRepairSession` is a no-op; CC owns the JSONL.

## Sync vs async turn completion

A subtle but important difference:

- **API turns** close `TurnState.CompletionChan` synchronously before `RunInference` returns. Post-turn work (save, metadata, compaction, logging) runs inline.
- **Delegated turns** close `CompletionChan` only when the backend fires `OnTurnComplete` (ccstream: on `result` message; cctmux: on `end_turn` in JSONL). The post-turn goroutine blocks inline waiting for it with an **activity-based timeout** — 2 minutes of stream silence ends the wait, not a fixed deadline. Activity is tracked via the backend's `LastActivity()`, seeded at turn start and refreshed on every stream event.

This means long tool calls on the delegated path don't time out as long as CC is still emitting progress heartbeats.

## Choosing a backend

### Pick `api` when

- You need branching (`/branch`, session-end memory formation via async branch, background todo sessions).
- You need Foci's compaction pipeline with its specific prompts and thresholds.
- You want the fallback chain (`[groups.fallbacks]`) to kick in on 529/5xx.
- You want per-turn prompt cache visibility and control.
- You want Foci-side tools (`shell`, `http_request`, `tmux`, etc.) and the exec bridge.
- You're running a non-coding agent (research assistant, chat persona, voice-only) where CC's coding optimisation is irrelevant.

### Pick `claude-code` (ccstream) when

- The agent's primary job is coding on a real workspace.
- You want CC's native tool suite (Read/Write/Edit/Bash/Glob/Grep/Agent/TodoWrite) without Foci duplicating any of it.
- You want CC's built-in context manager to handle compaction.
- You want a clean subprocess boundary (no tmux pane, no screen scraping).

### Pick `claude-code-tmux` (cctmux) only when

- You specifically need a human-attachable TUI pane (debugging, manual takeover, interactive `/login`).
- You are running a legacy config and haven't migrated yet.

## Config snippets

Traditional API agent (default — you don't need to write `backend = "api"`):

```toml
[[agents]]
id = "scout"
name = "Scout"
model = "claude-sonnet-4-6"
workspace = "/home/foci/scout"
```

Delegated CC agent (preferred ccstream backend):

```toml
[[agents]]
id = "coder"
name = "Coder"
backend = "claude-code"
workspace = "/home/coder/projects/myapp"

[agents.backend_config]
model = "sonnet"
allowed_tools = ["Bash(git:*)", "Bash(make:*)"]
```

Global CC defaults — applied to every CC agent, merged with per-agent `allowed_tools`:

```toml
[cc_backend]
default_allowed_tools = [
    "Read(/tmp/**)",
    "Write(/tmp/**)",
    "Edit(/tmp/**)",
    "MultiEdit(/tmp/**)",
]
```

Foci-level permission auto-approval (applies to both CC flavours, before the user is prompted):

```toml
[permissions]
auto_approve_common_readonly = true
auto_approve_common_safe_write = false
allow = ["Bash(git status)", "Bash(git diff*)"]
```

## Further reading

- [WIRING.md — The Agent Loop](WIRING.md#the-agent-loop-agentagentgo) — `TurnContract`, `OrchestrateFullTurn`, phase-by-phase breakdown.
- [WIRING.md — ccstream Backend](WIRING.md#ccstream-backend-internaldelegatorccstream) — stream-json protocol, hook integration, permission handling.
- [WIRING.md — Backend Watcher (tmux)](WIRING.md#backend-watcher--tmux-internaldelegatorcctmuxwatchergo) — cctmux watcher internals.
- [CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends) — all config keys.
- [SPEC.md — Coding Agent Backends (TurnContract)](SPEC.md) — design intent.

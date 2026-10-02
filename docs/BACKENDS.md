# Coding Agent Backends — API vs Delegated

Foci runs each agent's turns through one of several different code paths. This doc explains what they are, how they differ, and when to pick which. The second half is a feature-by-feature [parity reference](#delegated-backend-feature-parity) for the delegated backends. For the wiring details, see [WIRING.md](WIRING.md). For the exact config keys, see [CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends).

## The turn transports

Every `[[agents]]` entry has a `backend` field. It selects the API transport or one of four registered delegated backends. Both transports implement the same 19-method `TurnContract` interface (`internal/agent/turn_contract.go`); the delegated transport drives its backend through the 20-method `delegator.Delegator` interface (`internal/delegator/backend.go`) plus optional capability interfaces.

- **`backend = "api"` (default) — API transport.** Foci calls the LLM API directly, executes tools in-process, and manages the session history.
- **`backend = "claude-code"` (ccstream) — Delegated transport.** Foci spawns Claude Code as a subprocess via structured NDJSON over stdin/stdout. CC handles inference, tool execution, and its own context management; Foci feeds it prompts and reads back the assistant output.
- **`backend = "claude-code-tmux"` (cctmux) — Delegated transport, legacy.** Same role as ccstream but talks to the `claude` binary by screen-scraping a tmux pane.
- **`backend = "codex"` — Delegated transport.** OpenAI Codex CLI driven via `codex app-server` JSON-RPC 2.0 over stdio. Persistent subprocess like ccstream; the server holds the session and Foci drives it with RPC calls. **Not currently in use** (WIRING.md: "codex is disabled; left for its re-enable"): it is still registered and selectable, but it is not exercised in production.
- **`backend = "opencode"` — Delegated transport.** OpenCode driven via its HTTP/SSE server. Foci spawns one `opencode serve` subprocess per agent (shared across that agent's sessions and refcounted) and talks to it over HTTP.

The orchestrator (`OrchestrateFullTurn`) calls both transports through the same interface, which is why most Foci features — reminders, scratchpad, todos, tasks, nudges, multi-platform delivery, steering — work across all of them. Where the delegated backends differ from each other, the [parity reference](#delegated-backend-feature-parity) says how.

## What each owns

The table below contrasts the API backend with the delegated backends in general. Backend-specific notes (Codex, OpenCode) appear in their own sections below.

| Concern | API backend | Delegated backend (CC / Codex / OpenCode) |
|---|---|---|
| LLM inference | Foci → `provider.Send` | Subprocess (CC / `codex app-server` / `opencode serve`) |
| Tool registry & execution | Foci's `tools/` package | Backend's built-in tools + MCP |
| Session history | Foci-managed JSONL in `data/sessions/` | Backend-managed (CC: `~/.claude/projects/`; Codex/OpenCode: their own stores) |
| Context / compaction | Foci `compaction/compact.go` | Backend's own context manager (CC: `/compact`; Codex: `thread/compact/start`; OpenCode: its own mechanism) |
| Rate limiting | Foci per-endpoint gate | Backend's own limiter |
| Turn serialization | Foci per-session lock | Backend serializes internally |
| Model fallback chain | Foci `[groups.fallbacks]` | Backend picks its own fallback |
| Prompt caching | Foci's append-only cache contract | Backend manages its own cache |
| Branching / `/branch` | Full support | Backend fork via `BackendBrancher.ForkSession` on all three (whole session only; see [R3](#9-sessions-resume-and-branching)) |

Anything not in this table — platform I/O, command dispatch, nudges, reminders, task list, memory search, attachment normalisation, message transforms — happens outside `RunInference` and is shared by both transports.

## Delegated backend flavours

There are four registered delegated backends. The two CC flavours share the `TurnContract` surface, the `DelegatedManager` plumbing, and the permission system — they only differ in how they talk to the `claude` binary. Codex and OpenCode are independent backends behind the same `delegator.Delegator` interface.

### `claude-code` — ccstream (preferred)

Structured NDJSON over stdin/stdout. CC runs with `--input-format stream-json --output-format stream-json --permission-prompt-tool stdio`. Each wire message is one JSON object; the `type` field (`user`, `assistant`, `result`, `system`, `control_request`, `tool_progress`, `stream_event`) discriminates the kind.

Pros: no tmux dependency, no screen-scraping, structured permission prompts, precise turn boundaries via `result` messages, token-level streaming via `stream_event`, clean `/stop` via `control_request` interrupt, per-tool completion hooks (`foci-cc-hook`) give real-time tool_result visibility.

ccstream uses a **two-lifetime callback split** (TODO #747): `SessionEvents` (delivery — `OnText`, `OnTextDelta`, `OnThinkingDelta`, `OnToolStart`, `OnToolEnd`) is installed once per session via `Backend.AttachSessionEvents` and stored in an `atomic.Pointer` that's never nil after first attach, so text/tool emission paths never drop on a per-turn handler nilling. `TurnEvents` (bookkeeping — `OnTurnComplete`, `PostToolNudgeFunc`, `PreAnswerNudgeFunc`) is installed via `Inject.Turn` and cleared in `OnResult`. The pre-TODO #747 design bundled both into one combined per-turn handler that nilled per-turn — its replacement isn't optional, it's the structural fix that makes "the turn ended but CC kept emitting" handle correctly. cctmux implements the same split: its JSONL watcher dispatches delivery into `SessionEvents` and completion into `TurnEvents` on the `Backend`. See [WIRING.md — ccstream Backend](WIRING.md#ccstream-backend-internaldelegatorccstream).

### `claude-code-tmux` — cctmux (legacy)

CC runs interactively in a tmux pane. Foci pastes input via `load-buffer` / `paste-buffer` and tails CC's session JSONL file via fsnotify for output. Still supported; used when you want a human-visible CC pane or need CC's full TUI (for interactive slash commands, `/login` flows, etc.). It is not offered by the setup wizard and is left out of the parity reference.

Pros: the pane is a real terminal — you can attach, observe, and interact directly. Useful for debugging or when the agent needs a human to take over momentarily.

Cons: screen-scraped permissions, JSONL file-watching for turn boundaries, send-keys based `/stop`, tmux as a hard dependency.

**Unless you specifically need the interactive pane, use `claude-code`.**

### `codex` — OpenAI Codex via app-server

`backend = "codex"`. Foci drives the OpenAI Codex CLI through `codex app-server`, which speaks JSON-RPC 2.0 over stdio. One app-server process runs per agent, shared by that agent's sessions (threads) and refcounted: the server holds the session state and Foci issues RPC calls to advance turns, list models, and trigger compaction.

- **Model catalogue** is fetched live via `model/list` RPC, populating the capability catalogue (`modelcaps`) the same way the Anthropic `/v1/models` fetch does for ccstream.
- **Compaction** uses Codex's own `thread/compact/start` RPC, not Foci's compaction pipeline.
- **Server-side session naming.** Codex assigns and tracks session names itself, which is why the `set_session_alias` tool is disabled for this backend — the alias would be overwritten by the server.
- **Known gaps before re-enabling** are in [Known issues](#known-issues): unreachable `sandbox`/`api_key` config, unanswered server requests, attachments not sent as content.

### `opencode` — OpenCode via HTTP/SSE

`backend = "opencode"`. Foci drives OpenCode through its HTTP/SSE server. One `opencode serve` subprocess is spawned per agent (not per session); the subprocess is shared across that agent's sessions and **refcounted**, so it stays alive while any session needs it and exits when the last session releases it.

- **System-prompt suppression** is done via a `blank-system.ts` plugin loaded into the OpenCode instance, so Foci owns the system prompt instead of OpenCode.
- **No MCP elicitation.** OpenCode's MCP client does not advertise elicitation upstream, so there is nothing to wire into Foci's `ask` flow; use Foci's `ask` tool directly if a human decision is needed. (OpenCode's own question tool does reach the chat as buttons; see [Q1](#4-questions-plan-mode-elicitation).)
- **No auto-relogin.** Unlike ccstream's startup readiness probe, the OpenCode backend does not perform credential revival — a dead session surfaces the error to the user rather than recovering silently.

## What still applies on the delegated path

All of this works unchanged when you delegate to CC (other delegated backends: see the parity reference for each row's coverage):

- **Reminders, scratchpad, todos, task list** — Foci-side state, injected into each prompt as text blocks.
- **Nudges** — regex and every-N-turn triggers prepend to the user message. Mid-turn (post-tool and pre-answer) nudges are ccstream-only ([T13, T14](#1-turn-lifecycle)).
- **Message metadata** — the `[meta]`/`[state]` statusline block (rendered from the `statusline` template; default reproduces the historical two lines) plus any `[reminders]` block is composed by `composeTurnText` and joined into flat text via `JoinPrompt()` (instead of rich content blocks), **blank-line separated** so the agent can tell foci's injected header from the human's own text (#1627).
- **Platform connections** — Telegram, Discord, Android, HTTP, voice — the reply stream is the same.
- **Command dispatch** — `/sessions`, `/config`, `/stop`, `/reset`, `/facet`, etc. Foci handles them normally. `/model` goes via the ControlSender pattern. `/compact` — both manual (`/compact` command) and auto (threshold) — dispatches through `Agent.runDelegatedCompact`, which sends `/compact <foci-summary-prompt>` to CC and waits for the `compact_boundary` stream event. `/pass` and a small set of other forward-only commands (e.g. unhandled CC slash commands) are sent to the backend via `Backend.Inject(SourcePass)` — a fire-and-forget send that bypasses the turn handler so a forwarded `/context` doesn't get treated as a user turn.
- **Attachments** — both transports run attachments through the shared `prepareAttachments` step (#2095): convertible documents (docx/xlsx/pptx/html/csv/txt) become prompt text, images are downscaled, and every saved file also gets a `[<label> saved to: <path>]` note. ccstream then sends images and PDFs to the model as image/document content blocks, and an image in a mid-turn steer folds into the running turn. opencode sends them as file parts; codex sends only the text, so its model must open the file from the path note ([I5, I6](#2-input-and-steering)).
- **Steering** — mid-turn user messages are dispatched directly via `Backend.Inject(SourceSteer)`. On ccstream this sends the text via `writer.SendUser` at queue priority `"next"`. CC's mid-turn drain folds the message into the current `ask()` as an attachment to the next tool-result batch — the model addresses it in the same turn, the in-flight tool finishes naturally, and the response reaches the original handler. Priority `"now"` would make CC abort the in-flight ask and is deliberately not used; for "stop right now" semantics use `/reset hard`. The agent's per-session `Inbox.Enqueue` handles the routing decision — it calls `Inject(SourceSteer)` directly for delegated backends; the steer buffer is only used by API-mode agents. codex folds via `turn/steer`; opencode has no mid-turn fold and aborts and re-sends instead ([I1](#2-input-and-steering)).
- **Memory formation** — injected into the live CC session as a prompt (not branched).
- **Memory consolidation / nudge extraction / foci_summary** — batch runs via `DelegatedManager.RunBatch`: an ordinary stream-json turn on an ephemeral child session (caller's system prompt, model default `sonnet`, no permission prompts), closed as soon as the turn ends. Recorded in api.db and traced like any turn, labelled with `purpose` (#1962). On opencode and codex a batch run uses the agent's own model and can still raise a permission prompt ([P5](#3-permissions-and-safety)).
- **Auto-approval** — foci-level `[permissions]` rules are checked before any CC permission request reaches the user. Plus a static `--allowedTools` list at CC launch (merged from `[cc_backend] default_allowed_tools` and per-agent `backend_config.allowed_tools`) for rules CC can evaluate without a round-trip.
- **PreToolUse deny rules** (`claude-code` only): configurable rules, plus two preinstalled ones for `AskUserQuestion` and `CronCreate`, refuse named tool calls before they run and tell the agent why. See CONFIG.md → `[[cc_backend.pretool_rules]]`.
- **Stop rules** (`claude-code` only, per agent, none preinstalled): block the end of a turn whose final reply announces work ("starting X now") when the turn launched no background job, telling the agent to start it or say it is deferred. At most one block per turn. See CONFIG.md → `[[agents.backend_config.stop_rules]]`.
- **First-run onboarding** — the new-agent setup prompt is delivered on both backends. It is stored in `FirstRunMessage` and consumed exactly-once via `consumeFirstRunMessage`, which both transports call (the delegated path prepends it in `ComposePrompt`). Earlier it was consumed only on the API path, so claude-code agents silently lost it (#853). See WIRING.md → first-run onboarding delivery.

## Startup readiness check

Delegated backends implement `CheckReady(ctx)` on the `delegator.Delegator` interface (`delegator/backend.go`) — a startup-only probe, separate from the per-turn `TurnContract`. At boot, `checkDelegatedReadiness` calls it for every delegated agent before any startup turn is injected. ccstream shells `claude auth status` and triggers the automated re-login flow if the shared OAuth credential is dead; cctmux reports ready unconditionally; codex only checks its binary is on PATH; opencode is skipped by name (its probe needs a server that only `Start` creates); API agents are skipped. This means a boot with an already-expired CC token recovers proactively instead of failing the first user turn (which would otherwise take the first-run onboarding down with it). See WIRING.md → startup readiness probe.

## What's skipped on the delegated path

These are no-ops or handled by CC:

- **Foci's tool registry** — CC has its own tools; Foci's `tools/` package is not consulted.
- **Compactor** — `RunCompaction` sends `/compact` to CC instead of running Foci's compaction pipeline.
- **Cache management** — CC manages its own prompt cache.
- **Fallback chain** — `[groups.fallbacks]` has no effect; CC picks its own fallback.
- **Server tools** — web search, web fetch, etc. are provided by CC's own tools, not Anthropic server tools.
- **MCP** — Foci's MCP integration is unused; add MCP servers to CC directly.
- **Spawn / sub-agent tool** — CC's own `Agent` tool replaces Foci's spawn mechanism.
- **Foci's session branching** — `/branch` does not copy Foci JSONL; it asks the backend to fork its own session (`ForkSession`: ccstream copies the transcript, opencode calls `POST /session/:id/fork`, codex calls `thread/fork`). A fork always takes the whole session: a truncated fork (`TruncateAfter > 0`) is rejected by all three.
- **Session repair** — `LoadAndRepairSession` is a no-op; CC owns the JSONL.

## Sync vs async turn completion

A subtle but important difference:

- **API turns** close `TurnState.CompletionChan` synchronously before `RunInference` returns. Post-turn work (save, metadata, compaction, logging) runs inline.
- **Delegated turns** close `CompletionChan` only when the backend fires `OnTurnComplete` (ccstream: on `result` message; cctmux: on `end_turn` in JSONL). The post-turn goroutine blocks inline waiting for it with an **activity-based timeout** — 2 minutes of stream silence ends the wait, not a fixed deadline. Activity is tracked via the backend's `LastActivity()`, seeded at turn start and refreshed on every stream event.

This means long tool calls on the delegated path don't time out as long as CC is still emitting progress heartbeats.

## Choosing a backend

### Pick `api` when

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
- You want the fullest delegated feature set: ccstream is the reference column in the parity tables below.

### Pick `opencode` when

- You want a model provider Claude Code does not serve, and can live with the gaps marked ✗ in its column (no mid-turn fold for steers, no pretool/stop rules, no mid-turn nudges).

### Pick `claude-code-tmux` (cctmux) only when

- You specifically need a human-attachable TUI pane (debugging, manual takeover, interactive `/login`).
- You are running a legacy config and haven't migrated yet.

## Delegated backend feature parity

Feature-by-feature comparison of the three structured delegated backends, from the #2151 survey (origin/main `5d1dbf759`, 2026-10-02). ccstream (`backend = "claude-code"`) is the reference. cctmux is left out. **codex is not currently in use**, so its column describes code that is not running in production.

Path shorthand: `cc/` = `internal/delegator/ccstream/`, `oc/` = `internal/delegator/opencode/`,
`cx/` = `internal/delegator/codex/`, `dg/` = `internal/delegator/`, `ag/` = `internal/agent/`,
`gw/` = `cmd/foci-gw/`.

Cell key: **✓** equivalent · **✗** absent · **n/a** cannot apply to that backend (footnote says
why) · **[n]** partial or different, see [footnote](#parity-footnotes) n. Every row has an ID.
The [evidence](#parity-evidence) list gives file:symbol for each ✓ and partial, and what was
grepped for each ✗.

### 1. Turn lifecycle

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| T1 | Launch | Start the agent process/server for a session | ✓ | ✓ [1] | ✓ [1] |
| T2 | Ready wait | `WaitReady`: block until the backend accepts prompts | ✓ | ✓ [2] | ✓ |
| T3 | Begin turn | User input at idle opens a tracked turn | ✓ | ✓ | ✓ |
| T4 | Turn-end signal | Authoritative "turn finished" event drives `OnTurnComplete` | ✓ | ✓ | ✓ |
| T5 | Turn result | `TurnResult` text, tool count, usage, model | ✓ | ✓ | ✓ |
| T6 | Wait/in-flight | `WaitForTurn`, `IsTurnInFlight` | ✓ | ✓ | ✓ |
| T7 | Interrupt | `/stop` aborts the in-flight turn | ✓ | ✓ | ✓ |
| T8 | Typing indicator | `SetTypingFunc` driven through the turn | ✓ | ✓ | [3] |
| T9 | Activity clock | `ActivityChecker.LastActivity` for idle reaping and stream-silence timeout | ✓ | ✓ | ✓ |
| T10 | Process death | In-flight turn completes on death; next `Get` respawns | ✓ | ✓ | ✓ |
| T11 | Bounded shutdown | `Close` cannot hang (graceful, then kill) | ✓ | ✓ | ✓ |
| T12 | Autonomous runs | Backend-initiated runs adopted as foci turns; `AwaitingAutonomousRun` holds system injects | ✓ | ✗ [4] | ✗ [4] |
| T13 | Pre-answer nudge | Verify-before-answer re-dispatch inside the same turn | ✓ | [5] | ✗ |
| T14 | Post-tool nudge | every_n_tools / after_error / tool_pattern nudges mid-turn | ✓ | ✗ [5] | ✗ |
| T15 | Transport keep-alive | Liveness heartbeat on the transport | ✓ | ✓ [6] | ✗ |

### 2. Input and steering

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| I1 | Steer mid-turn | `SourceSteer` folds into the running turn | ✓ | [7] | ✓ |
| I2 | User follow-up mid-turn | `SourceUser` while in flight | ✓ | [8] | ✓ |
| I3 | System never steers | `SourceSystem` in flight returns `ErrTurnInFlight`; idle check + begin are atomic | ✓ | ✓ | ✓ |
| I4 | Steer-at-idle guard | Steer at idle with no `Turn` returns `ErrTurnNotInFlight` | ✓ | ✓ | [9] |
| I5 | Attachments, fresh turn | Images/PDF sent as content to the model | ✓ | ✓ | ✗ [10] |
| I6 | Attachments, folded | `FoldAttachmentCarrier`: image in a steer folds mid-turn | ✓ | ✗ | ✗ |
| I7 | Slash passthrough | `SourcePass` forwards an unhandled `/cmd` | ✓ | ✓ | ✗ [11] |
| I8 | Delivery tracking | `DeliveryTracker`: persist unconsumed inputs, ack, redeliver after a crash; `TranscriptChecker` | ✓ | ✗ | ✗ |
| I9 | Closed-transport error | `ErrBackendClosed` marks a write that raced teardown (lower log severity) | ✓ | ✗ | ✗ |

### 3. Permissions and safety

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| P1 | Permission prompt | Tool approval as chat buttons, answer returned to backend | ✓ | ✓ | ✓ |
| P2 | Always allow | Persist an approval beyond this call | ✓ | ✓ [12] | ✗ |
| P3 | Foci auto-approve | `[permissions]` rules + foci shell rules + exec-guard veto, before the user sees a prompt | ✓ | ✓ | [13] |
| P4 | Launch allowlist | `allowed_tools` / `[cc_backend] default_allowed_tools` pre-approval | ✓ | n/a [14] | n/a [14] |
| P5 | Unattended mode | `skip_permissions` and `StartOptions.SkipPermissions` (batch runs): never prompt a chat | ✓ | [15] | ✗ [15] |
| P6 | Permission-mode switch | `SetPermissionModeRequest` at runtime | ✓ | ✓ | [16] |
| P7 | Prompt cancel listeners | Backend-cancelled prompt disables its stale buttons | ✓ | ✓ | ✗ [17] |
| P8 | Prompts-cleared drain | `SetOnPromptsCleared` unblocks `WaitForPermission` | ✓ | ✓ | ✓ |
| P9 | Rich prompt body | Edit diffs; ExitPlanMode plan sent as a document | ✓ | ✗ | [18] |
| P10 | PreToolUse deny rules | `pretool_rules` (incl. defaults redirecting AskUserQuestion/CronCreate) | ✓ | ✗ | ✗ [19] |
| P11 | Stop rules | `stop_rules`: block a final reply announcing un-launched work | ✓ | ✗ | ✗ [19] |

### 4. Questions, plan mode, elicitation

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| Q1 | Native question tool | `QuestionResponder`: backend's ask-the-user tool as buttons | ✓ [20] | ✓ | ✗ [21] |
| Q2 | /stop cancels question | `CancelPendingQuestion` | ✓ | ✓ | ✗ |
| Q3 | MCP elicitation | `ElicitationResponder` (form, URL, completion) | ✓ | n/a [22] | ✗ [21] |
| Q4 | /plan | `RegisterPlan` delivery | ✓ | ✓ [23] | ✗ |
| Q5 | Plan approval gate | `PlanResponder`: plan approval prompt; typed message = revision feedback | ✓ | ✗ | ✗ |

### 5. Subagents and background work

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| S1 | Lifecycle events | `OnSubagentStart` / `OnSubagentEnd` | ✓ | ✓ | ✓ |
| S2 | Subagent text | `OnSubagentText` | ✓ | [24] | ✓ |
| S3 | Spawn prompt | Start event carries the instruction given to the subagent | ✓ | ✗ | ✗ |
| S4 | Reactivation runs | Re-woken subagent opens run 2, 3, ... under the same group | ✓ | n/a [25] | ✓ |
| S5 | Follow-up to running subagent | `OnSubagentPrompt` | ✓ | ✗ | ✗ |
| S6 | Status / running list | `SetOnSubagentStatus`, `SetOnSubagentRunning` (status line, pending-work gate) | ✓ | ✓ | ✗ [26] |
| S7 | Background commands | Track `run_in_background` shells as pending work | ✓ | ✗ | ✗ |
| S8 | Stop subagents / commands | `/stop subagents`, `/stop commands` | ✓ | ✗ | ✗ |
| S9 | Nested subagents | Grandchild attribution to the depth-1 chit | ✓ | ✗ [27] | ✗ [27] |
| S10 | Workflow agents | Tail `local_workflow` runs | ✓ | n/a [28] | n/a [28] |
| S11 | Subagent model | Record the model each subagent actually ran | ✓ | ✗ | ✗ |
| S12 | Subagent spend | Subagent calls booked with an actor in the ledger | ✓ | ✓ | ✓ |

### 6. Compaction and context

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| C1 | Trigger compaction | `SourceCompact` (manual `/compact` and threshold) | ✓ | ✓ | ✓ |
| C2 | Foci summary prompt | Compaction uses foci's `compaction-summary.md` | ✓ | [29] | [29] |
| C3 | Done wait | `CompactionWaiter` | ✓ | ✓ | ✓ |
| C4 | Start wait | `CompactionStartWaiter` (orders the "Compacting..." notice) | ✓ | ✓ | ✗ |
| C5 | Declined compaction | `ErrCompactionNoBoundary` returns at once instead of timing out | ✓ | ✗ | ✗ |
| C6 | Summary capture | `CompactionSummarizer` (tappable summary chit) | ✓ | ✓ | ✗ |
| C7 | Start/done callbacks | `SetOnCompactionStart` / `SetOnCompactionDone` | [30] | [30] | ✗ |
| C8 | Reload bounce | `reload_on_compact` bounce + resume nudge (agent layer) | ✓ | ✓ | ✓ [31] |
| C9 | Context window | `ContextWindowQuerier` | ✓ | ✓ | ✓ |
| C10 | Cache TTL | `CacheTTLProvider` (keepalive interval validation) | ✓ | ✗ | ✓ |

### 7. Cost, usage and limits

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| U1 | Ledger booking | `LedgerBooker` per-call cost ledger | ✓ | ✓ | ✓ |
| U2 | Self-closing activity | `TurnActivityCloser`: spend may outlive the turn | ✓ | ✗ | ✗ [32] |
| U3 | Cost divergence check | Backend-reported vs calculated cost | ✓ | ✓ | ✗ |
| U4 | Expectation guards | `ExpectationGuard` invariants + version tracking | ✓ | ✗ | ✓ |
| U5 | Resume cost baseline | Cumulative counters on `--resume` | ✓ | n/a [33] | n/a [33] |
| U6 | Utilisation notice | Rate-limit utilisation notice to the human's chat | ✓ | ✗ | ✗ |
| U7 | Limit gate | Usage/session limit engages `Agent.EngageRateLimit` | ✓ | ✓ | ✗ [34] |
| U8 | /mana | `RegisterUsage` subscription-usage query | ✓ | ✗ | ✗ |
| U9 | Turn usage | Token usage on `TurnResult` | ✓ | ✓ | ✓ |

### 8. Models and effort

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| M1 | Launch model | Model ladder (override, config, default) applied at Start | ✓ | ✓ | ✓ |
| M2 | Runtime model switch | `SetModelRequest` | ✓ | [35] | [35] |
| M3 | Model resolver | `ModelResolver`: alias to canonical id, persisted | n/a [36] | [36] | ✓ |
| M4 | Live catalogue | `modelcaps` live model list | ✓ [37] | ✗ | ✓ |
| M5 | Effort | Launch `EffortFunc` + runtime `ApplyFlagSettingsRequest` | ✓ | n/a [38] | [35] |
| M6 | Voice mode | `VoiceModer` low effort for voice turns | ✓ | n/a [38] | ✓ |
| M7 | Batch models | `BatchModelDefaulter` / `BatchCheapModeler` | ✓ | ✗ | ✗ |
| M8 | Silent model swap | Backend-side model fallback is surfaced | ✓ | ✗ | [39] |
| M9 | Thinking | Reasoning stream (`OnThinkingDelta`) | ✓ | ✓ | ✓ |

### 9. Sessions, resume and branching

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| R1 | Session id | `SetOnSessionReady` persists the backend session id | ✓ | ✓ | ✓ |
| R2 | Resume | `ResumeSessionID` after respawn/restart | ✓ | ✓ | ✓ |
| R3 | Fork | `BackendBrancher.ForkSession` (branch, facet, spawn clone) | ✓ [40] | ✓ [40] | ✓ [40] |
| R4 | Cleanup / sweep | `CleanupSession` (+ `RunningBackendCleaner` when it needs a live server) | ✓ | ✓ | ✓ |
| R5 | Retention period | Backend transcript retention for the resume-missed notice | ✓ | ✗ | ✗ |
| R6 | Session file path | `SessionFilePath` | ✓ | ✗ | ✓ |
| R7 | Batch runs | `DelegatedManager.RunBatch` ephemeral session | ✓ | [41] | [41] |
| R8 | Auto session naming | `ThreadNameConsumer` / `TurnResult.ThreadName` auto-alias | ✗ | ✗ | ✓ |

### 10. UI surfaces

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| D1 | Text blocks | `OnText` | ✓ | ✓ | ✓ |
| D2 | Text deltas | `OnTextDelta` streaming | ✓ | ✓ | ✓ |
| D3 | Tool start | `OnToolStart` | ✓ | ✓ | ✓ |
| D4 | Tool result | `OnToolEnd` with output (tool-call display, "Show results") | ✓ [42] | ✓ | ✓ |
| D5 | Tool runtime note | Slow-call runtime note into the model's context (#2125) | ✓ | ✗ | ✗ |
| D6 | /status detail | `StatusDetail` | ✓ | ✗ | ✓ |

### 11. Ops and health

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| O1 | Startup readiness probe | `CheckReady` before startup turns | ✓ | n/a [43] | [43] |
| O2 | Auth-failure detection | Recognise a dead credential | ✓ | ✓ [44] | ✗ |
| O3 | Automated re-login | `/login` + 401-triggered relogin driver | ✓ | n/a [44] | ✗ |
| O4 | Stderr capture | Subprocess stderr logged | ✓ | ✓ | ✗ |
| O5 | Per-session exec bridge | `FOCI_SOCK`/`BASH_ENV`/`FOCI_SESSION_KEY` reach the right session's shell | ✓ | ✓ | ✓ [45] |
| O6 | System prompt ownership | Foci's prompt replaces the backend's own | ✓ | ✓ | ✓ |
| O7 | Pool acquire serialisation | One server per agent, no double-spawn | n/a [46] | ✓ | ✓ |
| O8 | Orphan reaping | Kill stray backend processes from a previous gateway | ✗ [47] | ✓ | ✗ [47] |
| O9 | Unhandled server requests | Every backend-to-foci request gets an answer | ✓ | ✓ | ✗ [21] |

### 12. Config keys that apply to only some backends

| Key | ccstream | opencode | codex | Notes |
|---|---|---|---|---|
| `backend_config.model` | ✓ (via ladder) | ✓ | ✓ (also reads `cfg["model"]`) | |
| `backend_config.binary` / `[cc_backend].binary` / `[opencode_backend].binary` | ✓ | ✓ | ✓ | no `[codex_backend]` section |
| `backend_config.env` | ✓ | ✓ | ✓ | |
| `backend_config.idle_timeout` | ✓ | ✓ | ✓ | generic (DelegatedManager) |
| `allowed_tools`, `[cc_backend].default_allowed_tools` | ✓ | ✗ | ✗ | folded only for CC names (`gw/agents_delegated.go:configureDelegated`) |
| `pretool_rules`, `[cc_backend].pretool_rules` | ✓ | ✗ | ✗ | wired only `if backendName == "claude-code"` |
| `stop_rules` | ✓ | ✗ | ✗ | same |
| `skip_permissions` | ✓ | ✗ | ✗ | |
| `[cc_backend].background_task_max_age` | ✓ | ✗ | ✗ | `StartOptions.SubagentMaxAge`, read only by ccstream |
| `hostname`, `port`, `server_auth`, `log_level`, `default_permission` | ✗ | ✓ | ✗ | `[opencode_backend]` folded in |
| `sandbox`, `api_key` | ✗ | ✗ | [48] | read by codex but unreachable from config |
| `socket_path` | ✗ | ✗ | ✗ | cctmux only |
| `reload_on_compact` (agent) | ✓ | ✓ | ✓ | agent layer |

### Parity footnotes

1. opencode and codex run ONE process per agent, shared by that agent's sessions and refcounted
   (`oc/opencode.go:acquireServer`; `cx/codex.go` facades over a `sharedPool` owner). ccstream
   runs one `claude` per session.
2. opencode waits for two signals, `/global/health` and the SSE subscriber attaching
   (`oc/server.go:waitForSubscriber`, #1722).
3. codex sets typing on `turn/started` and clears it at completion only
   (`cx/handlers.go:onTurnStarted`, `completeTurn`). ccstream also re-asserts it on tool progress
   and subagent activity, so long tool calls keep the indicator alive.
4. Nothing in opencode or codex adopts a run it did not start. Whether either backend can start
   a root-session run on its own (as CC does after a background task finishes) is unverified.
5. Turn-time capability gating: `dg/backend.go:CapabilitiesForBackend` returns
   `PostToolNudge=false, PreAnswerNudge=false` for opencode and codex, and
   `gw/agents_setup.go:setupNudgeSystem` disables mid-turn rules by name (`isOpencode`).
   opencode nonetheless has a working pre-answer branch (`oc/handlers.go:onSessionIdle`, calls
   `turn.PreAnswerNudgeFunc` and re-sends) that the agent never arms. Post-tool nudges need a
   per-tool hook; ccstream gets it from `foci-cc-hook`.
6. opencode's SSE stream carries server heartbeats inbound (`oc/subscriber.go:Subscriber.onHeartbeat`);
   foci sends nothing outbound. ccstream sends `keep_alive` every 30s (`cc/lifecycle.go:runKeepAlive`).
7. opencode has no mid-turn fold: a steer buffers in `steerBuf`, POSTs `/abort` and is
   re-sent as a fresh turn once the abort burst drains (`oc/inject.go:injectSteer`,
   `flushSteerBuf`). The in-flight work is lost, not folded.
8. opencode queues a mid-turn user message and sends it as a new turn at `session.idle`
   (`oc/inject.go:injectUser`). It is answered after the current turn, not inside it.
9. codex returns `ErrTurnNotInFlight` only when `turn/steer` races completion
   (`cx/inject.go:steerTurn`). A `SourceSteer` arriving at idle goes to `beginTurn(text, inj.Turn)`
   even when `inj.Turn` is nil, opening an untracked turn (no `OnTurnComplete`, no usage),
   which is the failure ccstream and opencode guard against.
10. `cx/inject.go:beginTurn` builds `Input: []turnInput{{Type: "text", ...}}` only;
    `inj.Attachments` is never read (`grep Attachments cx/*.go`: no hits). The model sees only the
    agent layer's "saved to" path notes, so it must open images from disk.
11. `cx/inject.go:ImmediateInject` returns nil for `SourcePass` after a debug log. A forwarded
    slash command silently does nothing.
12. opencode's "Always Allow" sends `remember=true` (opencode's own rule store,
    `oc/permissions.go:RespondToPermission(permID, allow, remember)`). ccstream instead adds a
    prefix rule via `RespondToPermissionWithRule`. The agent supports both signatures
    (`ag/delegated_permission.go:SendPermissionResponse`).
13. codex auto-approves command approvals only (`cx/permissions.go:tryAutoApprove`, called from
    `onCommandApproval`). File-change approvals always prompt, and `item/permissions/requestApproval`
    is always declined (`onPermissionApproval`).
14. `--allowedTools` is a Claude Code launch flag. opencode's analogue is
    `default_permission` + PATCH `/config`; codex's would be the sandbox/approval policy.
15. ccstream passes `--dangerously-skip-permissions` and denies anything CC still asks
    (`cc/permissions.go:denyUnattended`, #2096). Neither opencode nor codex reads
    `StartOptions.SkipPermissions` (`grep SkipPermissions|skip_permissions`: no hits), and
    `DelegatedManager` wires the chat permission prompt for batch sessions too
    (`ag/delegated_manager.go`, `SetPermissionPromptFunc` is not gated on `isBatch`). So a
    batch run (consolidation, nudge extraction, summary) on these backends can put a permission
    prompt in the owner's chat. opencode can be configured `default_permission = "allow"`
    agent-wide; codex has no reachable equivalent (footnote 48).
16. codex queues the approval policy for the next `turn/start` (`cx/control.go:applyPendingControls`);
    `plan` maps to `on-request`, so there is no plan mode.
17. `cx/callbacks.go:RegisterPromptCancelListener` is a TODO no-op. `serverRequest/resolved` clears
    the pending entry and fires prompts-cleared (`cx/handlers.go:onServerRequestResolved`) but
    stale buttons stay live.
18. codex shows the command text, or the list of changed files for a file change
    (`cx/permissions.go:onFileChangeApproval`, `lookupItemDetail`). No diff.
19. codex already installs a `PreToolUse` hook (`cx/hooks.go`, `cmd/foci-codex-hook`), currently
    only to rewrite Bash commands for per-thread env. That is the natural carrier for pretool
    rules; codex's Stop-hook support was not checked.
20. Works, but the default pretool rule `ask_user_question` denies CC's AskUserQuestion and
    redirects the model to `foci_ask`, so this path is a fallback.
21. `cx/reader.go:handleServerRequest` handles three approval methods; any other server request
    (e.g. a user-input or MCP elicitation request) hits `default: logDebugf("unhandled server
    request ...")` and is never answered. If codex sends one, that turn waits forever.
    Which request methods codex 0.145 can send was not verified.
22. opencode's MCP client does not advertise elicitation upstream (WIRING.md, opencode issue
    #23066).
23. opencode sends the prompt with per-request `agent: "plan"` (`oc/plan.go:planDelivery`).
    ccstream injects a turn asking CC to call EnterPlanMode (`cc/plan.go:planDelivery`).
24. opencode surfaces only completed child text parts, no deltas (`oc/handlers.go:handleChildEvent`).
    ccstream also tails foreground subagent transcripts (`cc/subagent_tail.go`).
25. Hard-coded run 1 in `oc/handlers.go` ("opencode has no reactivation → run 1").
26. codex keeps its own `subagentTracker` (`cx/subagent.go`) instead of
    `dg/agent_tracker.go:SubagentTracker`, and has no `SetOnSubagentStatus`/`SetOnSubagentRunning`.
    So no status-line subagent detail, no running list, and no pending-work gate on codex.
27. Not handled. Whether opencode child sessions or codex child threads can themselves spawn
    children was not verified.
28. Workflow runs are a Claude Code feature (`local_workflow` tasks, `cc/workflow_tail.go`).
29. The agent sends `/compact <foci prompt>` on every call (`ag/compaction.go:runDelegatedCompact`).
    opencode ignores that text: its plugin's `session.compacting` hook reads a file written at
    Start (`oc/blank_system.go:WriteSessionCompactFile`). codex passes `-c compact_prompt=...` at
    app-server launch (`cx/lifecycle.go:appServerArgs`). Both use the prompt resolved at launch,
    not at the time of the compaction.
30. Implemented on ccstream (`cc/compaction.go`) and opencode (`oc/opencode.go`) but **no caller
    sets either** (`grep SetOnCompactionStart|SetOnCompactionDone` outside `dg/`: none). Dead
    hooks: delete them or wire them (e.g. to notice backend-initiated auto-compaction).
31. Agent-layer generic: closes the backend keeping the resume id; codex re-sends
    `baseInstructions` on `thread/resume` (`cx/lifecycle.go:resumeThread`).
32. WIRING.md: "A codex child that outlives its turn still reads `still_running` false early
    (codex is disabled; left for its re-enable)."
33. opencode and codex book each message/usage event as it arrives, so there is no cumulative
    counter to rebase.
34. `grep -i 'limit|ratelimit'` in `cx/`: no handler; unknown notifications fall to
    `logDebugf("unhandled notification")`.
35. Not live: opencode stores the model for the next prompt body after validating it with
    `opencode models` (`oc/control.go:SendControl`); codex queues model and effort for the next
    `turn/start` (`cx/control.go`). ccstream applies `set_model` / `apply_flag_settings` to the
    running process.
36. ccstream passes raw aliases (`opus`, `sonnet`) and CC resolves them. opencode resolves by unique
    substring (`oc/model_validate.go:resolveModel`) but not through `ModelResolver`, so the
    persisted id stays the user's alias.
37. Fetched at gateway level, not by the backend (`gw/main.go`, Anthropic `/v1/models` into
    `modelcaps.BackendCCStream`). `modelcaps.BackendKey("opencode")` falls to the default case, so
    opencode has no catalogue and no persister.
38. opencode has no effort concept (`oc/control.go`: ApplyFlagSettings is a logged no-op).
39. codex handles `model/rerouted` by updating its model and logging at INFO (`cx/reader.go`). ccstream
    logs `model_refusal_fallback` at WARN with CC's own text (#1968).
40. `TruncateAfter > 0` is rejected by all three. ccstream copies the transcript and appends
    fork-boundary closures for open tool calls and background tasks (`cc/branch.go:forkTranscript`).
    opencode uses `POST /session/:id/fork` on the already-running server. codex uses `thread/fork`
    and requires a running backend (`RunningBackendForker`).
41. Generic in `DelegatedManager.RunBatch`, but see footnote 15 (permission prompts) and M7 (no
    batch default model: opencode/codex batch runs fall back to the agent's own model).
42. Tool results come from `foci-cc-hook` PostToolUse/PostToolUseFailure hooks
    (`cc/hooks.go:handleHookResponse`); if the hook binary is missing ccstream runs without
    tool results. opencode and codex get results natively from their event streams.
43. The probe skips opencode by name (`gw/notifications.go`): its `CheckReady` needs a server
    that only `Start` creates, and `/global/health` cannot see provider auth. codex's
    `CheckReady` only checks the binary is on PATH (`cx/lifecycle.go:CheckReady`).
44. opencode detects `ProviderAuthError` and HTTP 401 and fans out to every session on the
    server (`oc/authfail.go`), but `gw/agents_delegated.go` only logs it: auth is per-provider
    (`opencode auth login <provider>`), so there is no single relogin flow to automate.
45. Three mechanisms: ccstream has a process per session, so the env is baked in; opencode uses a
    generated `shell.env` plugin reading `session-env/<id>.json` (`oc/session_env.go`); codex
    rewrites each Bash command with a PreToolUse hook (`cx/hooks.go:bindThreadEnv`). All share
    `dg/sessionenv`.
46. ccstream has no per-agent pool (WIRING.md "Per-agent acquire serialisation").
47. `opencode.ReapOrphanedServers` runs at startup (`gw/main.go`) and `CloseAllServers` at shutdown.
    `grep -i reap|orphan` found no equivalent for `claude` or `codex app-server` processes.
    Whether those children survive a gateway crash was not checked.
48. `cx/codex.go:sandboxMode` reads `cfg["sandbox"]` and `cx/lifecycle.go:buildEnv` reads
    `cfg["api_key"]`, but `config.BackendConfig` has neither field and `BackendConfig.ToMap`
    never emits them (`internal/config/types.go`). So the sandbox is always `workspace-write`
    and `CODEX_API_KEY` can come only from `backend_config.env`. Fix before re-enabling codex.


### Parity evidence

Each line is `ID: ccstream | opencode | codex`. A ✗ names the grep run in that package.

**Turn lifecycle**
- T1: `cc/lifecycle.go:Start` | `oc/backend_lifecycle.go:Start`, `oc/opencode.go:acquireServer` | `cx/lifecycle.go:Start`, `launchAppServer`
- T2: `cc/lifecycle.go:WaitReady` (init / initialize ack) | `oc/backend_lifecycle.go:WaitReady`, `oc/server.go:waitForSubscriber` | `cx/lifecycle.go:WaitReady`, `initialize`
- T3: `cc/inject.go:beginTurnWithText` | `oc/inject.go:injectUser` | `cx/inject.go:beginTurn`
- T4: `cc/handlers.go:OnSystem` `session_state_changed` → `cc/complete.go:onSessionIdle` | `oc/handlers.go:onSessionIdle` | `cx/handlers.go:onTurnCompleted`
- T5: `cc/handlers.go:OnResult` (stash), `cc/complete.go:completeTurn` | `oc/handlers.go:onSessionIdle`/`onMessageUpdated` | `cx/handlers.go:onTurnCompleted`, `turnUsageLocked`
- T6: `cc/inject.go:WaitForTurn`, `IsTurnInFlight` | `oc/inject.go` same | `cx/codex.go:WaitForTurn`, `IsTurnInFlight`
- T7: `cc/control.go:Interrupt` | `oc/control.go:Interrupt` | `cx/lifecycle.go:Interrupt` (`turn/interrupt`)
- T8: `cc/callbacks.go:SetTypingFunc` + calls in `complete.go`, `handlers.go` | `oc/handlers.go` (`typingFunc` x4) | `cx/handlers.go:onTurnStarted`, `completeTurn`
- T9: `cc/ccstream.go:LastActivity` | `oc/activity.go:LastActivity` | `cx/codex.go:LastActivity`
- T10: `cc/lifecycle.go:finalizeExit` | `oc/lifecycle.go:Server.finalizeExit`, `oc/opencode.go:IsRunning` | `cx/reader.go:onReaderStopped`
- T11: `cc/lifecycle.go:Close` | `oc/lifecycle.go:Server.Close` | `cx/lifecycle.go:Close` (`closeGracefulWait` then Kill)
- T12: `cc/inject.go:AdoptRunningTurn`, `AwaitingAutonomousRun`, `cc/callbacks.go:SetOnAutonomousOpen` | ✗ grep `AdoptRunningTurn|AwaitingAutonomousRun|SetOnAutonomousOpen` | ✗ same
- T13: `cc/complete.go:tryPreAnswerRedispatch` | `oc/handlers.go:onSessionIdle` (gated off) | ✗ grep `PreAnswerNudgeFunc`
- T14: `cc/hooks.go:handleHookResponse` (`PostToolNudgeFunc`) | ✗ grep `PostToolNudgeFunc` | ✗ same
- T15: `cc/lifecycle.go:runKeepAlive` | `oc/subscriber.go:onHeartbeat` | ✗ grep `keep.?alive|heartbeat`

**Input and steering**
- I1: `cc/inject.go:sendFold` (priority `next`) | `oc/inject.go:injectSteer`, `flushSteerBuf` | `cx/inject.go:steerTurn` (`turn/steer`)
- I2: `cc/inject.go:ImmediateInject` | `oc/inject.go:injectUser` (steerBuf) | `cx/inject.go:ImmediateInject` → `steerTurn`
- I3: `cc/inject.go:tryBeginTurn` | `oc/inject.go:injectSystem` → `tryBeginTurn` | `cx/inject.go:beginTurn` (`turnActive` under `turnMu`)
- I4: `cc/inject.go:ImmediateInject` | `oc/inject.go:injectSteer` | `cx/inject.go:steerTurn` only
- I5: `cc/inject.go:sendToPaneWithAttachments`, `contentBlocks`, `attachmentBlockType` | `oc/inject.go:buildPromptBody` (file parts) | ✗ grep `Attachments|attachment|mime` (only `imageGeneration` tool items)
- I6: `cc/inject.go:FoldsAttachment` | ✗ grep `FoldsAttachment` | ✗ same
- I7: `cc/inject.go:ImmediateInject` (SourcePass) | `oc/inject.go:injectCommand` | ✗ `cx/inject.go:ImmediateInject` returns nil
- I8: `cc/delivery.go:SetDeliveryHooks`, `OnInputAck`, `InputInTranscript`; `gw/agents_delegated.go:transcriptCheckerFor` | ✗ grep `SetDeliveryHooks|Redeliveries|Refs` | ✗ same
- I9: `cc/lifecycle.go:transportGone` + `ErrBackendClosed` use | ✗ grep `ErrBackendClosed` | ✗ same

**Permissions and safety**
- P1: `cc/permissions.go:handleToolRequest`, `RespondToPermission` | `oc/permissions.go:onPermissionUpdated`, `surfacePermission`, `RespondToPermission` | `cx/permissions.go:onCommandApproval`, `onFileChangeApproval`, `RespondToPermission`
- P2: `cc/permissions.go:RespondToPermissionWithRule` | `oc/permissions.go:RespondToPermission(remember)` | ✗ only Allow/Deny choices in `cx/permissions.go`
- P3: `cc/autoapprove.go:autoApprovePermission` | `oc/permissions.go:checkAutoApprove` | `cx/permissions.go:tryAutoApprove`
- P4: `cc/lifecycle.go:Start` (`--allowedTools` from `cfg["allowed_tools"]`) | n/a | n/a
- P5: `cc/lifecycle.go:Start` (`--dangerously-skip-permissions`), `cc/permissions.go:denyUnattended` | `oc/backend_lifecycle.go` `cfg["default_permission"]` | ✗ grep `SkipPermissions|skip_permissions|dangerously`
- P6: `cc/control.go:SendControl` (`set_permission_mode`) | `oc/control.go:SendControl` (`patchConfig`, `mapPermissionMode`) | `cx/control.go:SendControl`, `codexApprovalPolicy`
- P7: `cc/callbacks.go:RegisterPromptCancelListener` + `dg/outstanding.go` | `oc/opencode.go:RegisterPromptCancelListener`, `oc/permissions.go:onPermissionReplied` | ✗ `cx/callbacks.go` TODO
- P8: `cc/callbacks.go:SetOnPromptsCleared` | `oc/opencode.go:SetOnPromptsCleared` | `cx/permissions.go:respondApproval`, `cx/handlers.go:onServerRequestResolved`
- P9: `cc/permissions.go:formatEditDiff`, `planAttachmentPath` | ✗ `oc/permissions.go:surfacePermission` sends title only (grep `diff`) | `cx/permissions.go:lookupItemDetail`
- P10: `dg/pretool`, `cc/hooks.go:buildHookSettingsJSON`, `cc/callbacks.go:SetPreToolRules` | ✗ grep `pretool\.` | ✗ same
- P11: `dg/stoprule`, `cc/callbacks.go:SetStopRules`, `cc/hooks.go:logStopVerdict` | ✗ grep `stoprule\.` | ✗ same

**Questions, plan, elicitation**
- Q1: `cc/userquestion.go:handleUserQuestion`, `RespondToQuestion` | `oc/permissions.go:handleQuestionPermission`, `RespondToQuestion` | ✗ grep `RespondToQuestion|userInput|request_user_input`
- Q2: `cc/userquestion.go:CancelQuestion`, `HasPendingQuestion` | `oc/permissions.go:CancelQuestion`, `HasPendingQuestion` | ✗ (needs QuestionResponder)
- Q3: `cc/elicitation.go:OnElicitationRequest`, `RespondToElicitation`, `OnElicitationComplete` | n/a | ✗ grep `elicit`
- Q4: `cc/plan.go:planDelivery` | `oc/plan.go:planDelivery` | ✗ grep `RegisterPlan`
- Q5: `cc/permissions.go:HasPendingPlanPermission`, `CancelPlanWithFeedback`; `ag/inbox.go` plan-cancel-by-message | ✗ grep `HasPendingPlanPermission` | ✗ same

**Subagents**
- S1: `cc/handlers.go:OnSystem` `task_started`/`task_notification`, `cc/hooks.go` Agent PreToolUse | `oc/handlers.go:handleToolPart` | `cx/handlers.go:openSubagentRun`, `handleSubagentNotification`
- S2: `cc/handlers.go:OnAssistant` (non-top-level), `cc/subagent_tail.go` | `oc/handlers.go:handleChildEvent` | `cx/handlers.go:handleSubagentNotification`
- S3: `cc/subagent_runs.go:setAgentPrompt` | `oc/handlers.go` passes `""` | `cx/handlers.go:openSubagentRun` passes `""`
- S4: `cc/subagent_runs.go:onTaskStarted`, `stashResumePrompt` | n/a | `cx/subagent.go:start` (runIndex++)
- S5: `cc/handlers.go:OnAssistant` SendMessage → `OnSubagentPrompt` | ✗ grep `OnSubagentPrompt` | ✗ same
- S6: `cc/callbacks.go:SetOnSubagentStatus`, `SetOnSubagentRunning` | `oc/opencode.go` same | ✗ grep `SetOnSubagentStatus|SetOnSubagentRunning|delegator.SubagentTracker`
- S7: `cc/handlers.go:OnAssistant` (`ExtractBashBackground`, `SubagentKindCommand`) | ✗ grep `background|SubagentKind` | ✗ same
- S8: `cc/control.go:StopSubagents`, `StopCommands` | ✗ grep `StopSubagents|StopCommands` | ✗ same
- S9: `cc/nested_subagents.go` | ✗ grep `nested` | ✗ same
- S10: `cc/workflow_tail.go` | n/a | n/a
- S11: `cc/ccstream.go` (`b.agents.SetModel`) | ✗ grep `SetModel(` on tracker | ✗ same
- S12: `cc/ccbook.go:subLine` | `oc/ledger.go:bookMessage(msg, childCallID)` | `cx/ledger.go:bookCycle`, `cx/subagent.go:spend`

**Compaction and context**
- C1: `cc/inject.go` (SourceCompact → slash) | `oc/inject.go:sendSummarize` | `cx/lifecycle.go:triggerCompaction`
- C2: per-call text | `oc/blank_system.go:WriteSessionCompactFile` | `cx/lifecycle.go:appServerArgs`
- C3: `cc/compaction.go:ArmCompactionWait`, `WaitForCompaction` | `oc/compaction.go` same | `cx/lifecycle.go` same
- C4: `cc/compaction.go:ArmCompactionStartWait` | `oc/compaction.go:ArmCompactionStartWait`, `oc/handlers.go:handleCompactionPart` | ✗ grep `ArmCompactionStartWait`
- C5: `cc/compaction.go:signalCompactionAbort` | ✗ grep `ErrCompactionNoBoundary` | ✗ same
- C6: `cc/compaction.go:CompactionSummary` | `oc/compaction.go:CompactionSummary` | ✗ grep `CompactionSummary`
- C7: `cc/compaction.go:SetOnCompactionStart/Done` | `oc/opencode.go` same | ✗ (and no caller anywhere)
- C8: `ag/compaction.go:runDelegatedCompact`, `ag/delegated_manager.go:BounceSessionIfPromptChanged` (generic)
- C9: `cc/control.go:GetContextWindow` | `oc/context_usage.go:GetContextWindow` | `cx/context_window.go:GetContextWindow`
- C10: `cc/control.go:CacheTTL` (1h) | ✗ grep `CacheTTL` | `cx/context_window.go:CacheTTL` (5m)

**Cost, usage, limits**
- U1: `cc/ccledger.go:LedgerBackend`, `cc/ccbook.go` | `oc/ledger.go` | `cx/ledger.go`
- U2: `cc/ccledger.go:ClosesTurnActivity` | ✗ grep `ClosesTurnActivity` | ✗ same
- U3: `cc/ccbook.go:checkDivergence` | `oc/ledger.go` via `dg/costcheck.go:CostDivergenceChecker` | ✗ grep `Divergence`
- U4: `cc/expectations.go` | ✗ grep `ExpectationGuard|violated` | `cx/expectations.go:checkTokenUsage`
- U5: `cc/cost.go:resumeBaseline` | n/a | n/a
- U6: `cc/handlers.go:OnRateLimit`, `cc/ratelimit.go:FormatRateLimitNotice`, `gw/agents_delegated.go` `sb.SetOnRateLimited` | ✗ (its `SetOnRateLimited` is the gate, U7) | ✗ grep `rate.?limit`
- U7: `cc/handlers.go:OnAssistant` (`syntheticSessionLimitText`) → `SetOnSessionLimit` | `oc/ratelimit.go:handleRateLimitRetry` → `SetOnRateLimited` | ✗ grep `limit`
- U8: `cc/usage_oneshot.go:QueryUsage`, `cc/ccstream.go:init` RegisterUsage | ✗ grep `RegisterUsage` | ✗ same
- U9: `cc/handlers.go:OnResult` | `oc/handlers.go:onMessageUpdated` | `cx/handlers.go:onTokenUsage`

**Models**
- M1: `cc/lifecycle.go:Start` (`--model`) | `oc/backend_lifecycle.go:Start` (`resolveModelFn`) | `cx/lifecycle.go:prepareConfiguredModel`
- M2: `cc/control.go:sendSetModel` | `oc/control.go:SendControl` | `cx/control.go:SendControl`
- M3: n/a | `oc/model_validate.go:resolveModel` (not the interface) | `cx/model_resolver.go:ResolveModel`
- M4: `gw/main.go` modelcaps fetcher for `BackendCCStream` | ✗ grep `modelcaps` | `cx/modelcaps.go:refreshModelCaps`, `SetOnModelCaps`
- M5: `cc/lifecycle.go:Start` (`--effort`), `cc/control.go:SendControl` | n/a | `cx/control.go` `pendingEffort`
- M6: `cc/voicemode.go` | n/a | `cx/voicemode.go`
- M7: `cc/ccstream.go:BatchDefaultModel`, `BatchCheapModel` | ✗ grep `BatchDefaultModel|BatchCheapModel` | ✗ same
- M8: `cc/handlers.go:OnSystem` `model_refusal_fallback` | ✗ grep `fallback|reroute` | `cx/reader.go` `model/rerouted`
- M9: `cc/handlers.go:OnStreamEvent` | `oc/handlers.go:handleReasoningPart` | `cx/handlers.go:onReasoningDelta`

**Sessions**
- R1: `cc/handlers.go:OnSystem` init → `onSessionReady` | `oc/opencode.go:SetOnSessionReady` | `cx/callbacks.go:SetOnSessionReady`
- R2: `cc/lifecycle.go:Start` (`--resume`) | `oc/backend_lifecycle.go:resumeSession` | `cx/lifecycle.go:resumeThread`
- R3: `cc/branch.go:ForkSession`, `forkTranscript` | `oc/branch.go:ForkSession` | `cx/branch.go:ForkSession`, `ForkRequiresRunningBackend`
- R4: `cc/branch.go:CleanupSession` | `oc/branch.go:CleanupSession`, `OpenCleanupScope` | `cx/branch.go:CleanupSession`, `OpenCleanupScope`
- R5: `cc/retention.go:CleanupPeriod`, `gw/agents_delegated.go:resumeRetentionFor` | ✗ (name switch returns 0) | ✗ same
- R6: `cc/ccstream.go:SessionFilePath` | ✗ `oc/opencode.go:SessionFilePath` returns `""` | `cx/codex.go:SessionFilePath`
- R7: `ag/delegated_manager.go` `batchSpecFor` (generic)
- R8: ✗ grep `ThreadName` | ✗ same | `cx/codex.go:ConsumeThreadName`, `cx/reader.go` `thread/name/updated`

**UI**
- D1-D3: `cc/handlers.go:OnAssistant`, `OnStreamEvent` | `oc/handlers.go:handleTextPart`, `handleToolPart` | `cx/handlers.go:onItemStarted`, `onItemCompleted`, `onAgentMessageDelta`
- D4: `cc/hooks.go:handleHookResponse` | `oc/handlers.go:handleToolPart` | `cx/handlers.go:onItemCompleted`
- D5: `cmd/foci-cc-hook` `slowToolThreshold`, `cc/hooks.go` `tool_runtime_shown` | ✗ grep `runtime|duration_ms` | ✗ same
- D6: `cc/control.go:StatusDetail` | ✗ `oc/context_usage.go:StatusDetail` returns `""` | `cx/codex.go:StatusDetail`

**Ops**
- O1: `cc/readiness.go:CheckReady` | `oc/backend_lifecycle.go:CheckReady` (skipped in `gw/notifications.go`) | `cx/lifecycle.go:CheckReady`
- O2: `cc/authfail.go:isAuthFailure` | `oc/authfail.go:authCheckingTransport`, `fanOutAuthFailure` | ✗ grep `auth|401`
- O3: `gw/agents_delegated.go:triggerRelogin` (claude-code only), `internal/relogin` | n/a | ✗
- O4: `cc/lifecycle.go:captureStderr` | `oc/lifecycle.go:captureStderr` | ✗ grep `Stderr` in `cx/lifecycle.go`
- O5: `cc/env.go:buildEnv` | `oc/session_env.go` | `cx/hooks.go:bindThreadEnv`
- O6: `cc/lifecycle.go` initialize `systemPrompt` | `oc/blank_system.go:EnsureBlankSystemPlugin` | `cx/lifecycle.go` `BaseInstructions`
- O7: n/a | `oc/opencode.go:acquireServer` (`acquireLocks`) | `cx/lifecycle.go:Start` (`acquireLocks`)
- O8: ✗ grep `Reap|orphan` | `oc/reap.go:ReapOrphanedServers` | ✗ same
- O9: `cc/reader.go:dispatch` (all `control_request` subtypes answered) | SSE (no server-to-client requests) | ✗ `cx/reader.go:handleServerRequest` default


### Known issues

Found by the parity survey, beyond the table:

- **Dead hooks:** `SetOnCompactionStart` / `SetOnCompactionDone` on ccstream and opencode have
  no caller (C7, footnote 30).
- **Unreachable codex config:** `sandbox` and `api_key` (footnote 48).
- **codex can hang on an unknown server request** (footnote 21).
- **Batch runs on opencode/codex can prompt the owner's chat** (footnote 15).
- **Nudge capability is decided by name in two places** that can disagree:
  `dg/backend.go:CapabilitiesForBackend` (switch on name) and `gw/agents_setup.go` (`isOpencode`).
  codex gets `CanPostTool: true` from the nudge scheduler but `PostToolNudge: false` from
  Capabilities.

## Adding a new delegated backend

### Interface-level minimum (the `dg.Delegator` interface, 20 methods)

Everything a backend must provide to run turns at all:

1. **Process:** `Start`, `WaitReady`, `CheckReady`, `IsRunning`, `Close`. `Close` must be bounded.
   `IsRunning` must turn false on death, or `DelegatedManager.Get` hands back a corpse forever
   (the opencode server-death bug).
2. **Turns:** `ImmediateInject` honouring the whole routing table in `dg/backend.go` (User,
   Steer, System, Compact, Pass). In particular: System in flight → `ErrTurnInFlight`, atomically.
   Steer at idle with nil `Turn` → `ErrTurnNotInFlight`. Never open a turn with nil `TurnEvents`.
   Also `WaitForTurn`, `IsTurnInFlight`, `Interrupt`, and exactly one `OnTurnComplete` per turn,
   including on process death.
3. **Delivery:** `AttachSessionEvents` (session-lifetime sink; never drop text because no turn is
   armed), `SetTypingFunc`.
4. **Prompts:** `SetPermissionPromptFunc`, `SetOnPromptsCleared` (must fire, or
   `WaitForPermission` wedges the session), `RegisterPromptCancelListener`.
5. **Identity:** `SessionID`, `SetOnSessionReady` (without it there is no resume),
   `SessionFilePath` (may be `""`), `StatusDetail` (may be `""`).
6. **TUI leftovers:** `SendKeystroke` / `SendSpecialKey` return "not supported".

### Optional capabilities, in suggested order

- **Effectively required:**
  - `LedgerBooker`: without it no cost is booked and `LogUsage` logs the gap.
  - `BackendCapabilities`: **if absent the agent assumes `PostToolNudge` and `PreAnswerNudge`
    are TRUE** (`ag/turn_delegated.go:buildTurnEvents`). Implement it, and add the name to
    `CapabilitiesForBackend`.
  - `ActivityChecker`: idle reaping and stream-silence timeouts.
  - `CompactionWaiter` + `CompactionStartWaiter`: without them compaction falls back to
    `WaitForTurn`.
  - `ContextWindowQuerier`.
  - A permission responder with one of the two `RespondToPermission` signatures
    (`ag/delegated_permission.go`).
- **High value:**
  - `ControlSender` (model and permission mode).
  - `BackendBrancher` (+ `RunningBackendForker` / `RunningBackendCleaner` if the store lives
    behind a live server).
  - `CompactionSummarizer`, `CacheTTLProvider`, `QuestionResponder`.
  - `SetOnSubagentStatus` / `SetOnSubagentRunning`: reuse `dg.SubagentTracker` rather than a
    private tracker, so the status line and pending-work gate work.
  - Auth-failure and rate-limit callbacks wired in `gw/agents_delegated.go:configureDelegated`.
  - Honour `StartOptions.SkipPermissions`.
- **Feature-specific:** `VoiceModer` and effort (only if the backend has an effort knob),
  `ModelResolver` + modelcaps catalogue, `PlanResponder`, `ElicitationResponder`,
  `DeliveryTracker`, `AutonomousRunAwaiter` / `AdoptRunningTurn`, `FoldAttachmentCarrier`,
  `ThreadNameConsumer`, `BatchModelDefaulter` / `BatchCheapModeler`, `TurnActivityCloser`, a
  per-tool hook (pretool rules, stop rules, post-tool nudges, runtime notes), expectation guards.

### Name-based branches to update (not capability checks)

- `gw/agents_delegated.go`: `backendDefaultModel`, config folding blocks, relogin, pretool/stop
  rule wiring, per-backend callback type switches, `transcriptCheckerFor`, `resumeRetentionFor`.
- `gw/agents_setup.go:setupNudgeSystem` (`isOpencode`).
- `gw/environment.go` (command-approval block is `claude-code` only).
- `gw/notifications.go` (readiness probe skip).
- `internal/config/resolved.go:isAutoNamingBackend` (prefix `codex`).
- `dg/backend.go:CapabilitiesForBackend`, `HumanReadableBackendName`.
- `internal/modelcaps/modelcaps.go:BackendKey`.
- `internal/command/settings.go` (`/thinking` BackendGate).
- `cmd/foci/cmd_pretool.go` (claude-code only).
- `gw/shutdown.go` / `gw/main.go` if the backend pools processes (cf. opencode `CloseAllServers`,
  `ReapOrphanedServers`).

Per-session shared-process backends must also route the exec bridge per session through
`dg/sessionenv` (see O5).

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

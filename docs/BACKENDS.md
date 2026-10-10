# Coding Agent Backends — API vs Delegated

Foci runs each agent's turns through one of several different code paths. This doc explains what they are, how they differ, and when to pick which. The second half is a feature-by-feature [parity reference](#delegated-backend-feature-parity) for the delegated backends. For the wiring details, see [WIRING.md](WIRING.md). For the exact config keys, see [CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends).

## The turn transports

Every `[[agents]]` entry has a `backend` field. It selects the API transport or one of three registered delegated backends. Both transports implement the same 19-method `TurnContract` interface (`internal/agent/turn_contract.go`); the delegated transport drives its backend through the 18-method `delegator.Delegator` interface (`internal/delegator/backend.go`) plus optional capability interfaces.

- **`backend = "api"` (default) — API transport.** Foci calls the LLM API directly, executes tools in-process, and manages the session history.
- **`backend = "claude-code"` (ccstream) — Delegated transport.** Foci spawns Claude Code as a subprocess via structured NDJSON over stdin/stdout. CC handles inference, tool execution, and its own context management; Foci feeds it prompts and reads back the assistant output.
- **`backend = "codex"` — Delegated transport.** OpenAI Codex CLI driven via `codex app-server` JSON-RPC 2.0 over stdio. Persistent subprocess like ccstream; the server holds the session and Foci drives it with RPC calls.
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
| Branching / `/branch` | Full support | Backend fork via `BackendBrancher.ForkSession` on all three (whole session only; see `branch` [footnote 40](#parity-footnotes)) |

Anything not in this table — platform I/O, command dispatch, nudges, reminders, task list, memory search, attachment normalisation, message transforms — happens outside `RunInference` and is shared by both transports.

## Delegated backend flavours

There are three registered delegated backends: Claude Code, Codex and OpenCode. All three sit behind the same `delegator.Delegator` interface and share the `TurnContract` surface and the `DelegatedManager` plumbing. (The tmux-pane Claude Code backend, `claude-code-tmux`, was removed in #2179.)

### `claude-code` — ccstream (preferred)

Structured NDJSON over stdin/stdout. CC runs with `--input-format stream-json --output-format stream-json --permission-prompt-tool stdio`. Each wire message is one JSON object; the `type` field (`user`, `assistant`, `result`, `system`, `control_request`, `tool_progress`, `stream_event`) discriminates the kind.

Pros: no tmux dependency, no screen-scraping, structured permission prompts, precise turn boundaries via `result` messages, token-level streaming via `stream_event`, clean `/stop` via `control_request` interrupt, per-tool completion hooks (`foci-cc-hook`) give real-time tool_result visibility.

ccstream uses a **two-lifetime callback split** (TODO #747): `SessionEvents` (delivery — `OnText`, `OnTextDelta`, `OnThinkingDelta`, `OnToolStart`, `OnToolEnd`) is installed once per session via `Backend.AttachSessionEvents` and stored in an `atomic.Pointer` that's never nil after first attach, so text/tool emission paths never drop on a per-turn handler nilling. `TurnEvents` (bookkeeping — `OnTurnComplete`, `PostToolNudgeFunc`, `PreAnswerNudgeFunc`) is installed via `Inject.Turn` and cleared in `OnResult`. The pre-TODO #747 design bundled both into one combined per-turn handler that nilled per-turn — its replacement isn't optional, it's the structural fix that makes "the turn ended but CC kept emitting" handle correctly. See [WIRING.md — ccstream Backend](WIRING.md#ccstream-backend-internaldelegatorccstream).

### `codex` — OpenAI Codex via app-server

`backend = "codex"`. Foci drives the OpenAI Codex CLI through `codex app-server`, which speaks JSON-RPC 2.0 over stdio. One app-server process runs per agent, shared by that agent's sessions (threads) and refcounted: the server holds the session state and Foci issues RPC calls to advance turns, list models, and trigger compaction.

- **Model catalogue** is fetched live via `model/list` RPC, populating the capability catalogue (`modelcaps`) the same way the Anthropic `/v1/models` fetch does for ccstream.
- **Compaction** uses Codex's own `thread/compact/start` RPC, not Foci's compaction pipeline.
- **Server-side session naming.** Codex assigns and tracks session names itself, which is why the `set_session_alias` tool is disabled for this backend — the alias would be overwritten by the server.
- **Known gaps before re-enabling** are in [Known issues](#known-issues): unreachable `sandbox`/`api_key` config, unanswered server requests, attachments not sent as content.

### `opencode` — OpenCode via HTTP/SSE

`backend = "opencode"`. Foci drives OpenCode through its HTTP/SSE server. One `opencode serve` subprocess is spawned per agent (not per session); the subprocess is shared across that agent's sessions and **refcounted**, so it stays alive while any session needs it and exits when the last session releases it.

- **System-prompt suppression** is done via a `blank-system.ts` plugin loaded into the OpenCode instance, so Foci owns the system prompt instead of OpenCode.
- **No MCP elicitation.** OpenCode's MCP client does not advertise elicitation upstream, so there is nothing to wire into Foci's `ask` flow; use Foci's `ask` tool directly if a human decision is needed. (OpenCode's own question tool does reach the chat as buttons; see `questions` in [Declared capabilities](#declared-capabilities).)
- **No auto-relogin.** Unlike ccstream's startup readiness probe, the OpenCode backend does not perform credential revival — a dead session surfaces the error to the user rather than recovering silently.

## What still applies on the delegated path

All of this works unchanged when you delegate to CC (other delegated backends: see the parity reference for each row's coverage):

- **Reminders, scratchpad, todos, task list** — Foci-side state, injected into each prompt as text blocks.
- **Nudges** — regex and every-N-turn triggers prepend to the user message. Pre-answer nudges work on ccstream and opencode; post-tool nudges are ccstream-only (`pre_answer_nudge`, `post_tool_nudge` in [Declared capabilities](#declared-capabilities)).
- **Message metadata** — the `[meta]`/`[state]` statusline block (rendered from the `statusline` template; default reproduces the historical two lines) plus any `[reminders]` block is composed by `composeTurnText` and joined into flat text via `JoinPrompt()` (instead of rich content blocks), **blank-line separated** so the agent can tell foci's injected header from the human's own text (#1627).
- **Platform connections** — Telegram, Discord, Android, HTTP, voice — the reply stream is the same.
- **Command dispatch** — `/sessions`, `/config`, `/stop`, `/reset`, `/facet`, etc. Foci handles them normally. `/model` goes via the ControlSender pattern. `/compact` — both manual (`/compact` command) and auto (threshold) — dispatches through `Agent.runDelegatedCompact`, which sends `/compact <foci-summary-prompt>` to CC and waits for the `compact_boundary` stream event. `/pass` and a small set of other forward-only commands (e.g. unhandled CC slash commands) are sent to the backend via `Backend.Inject(SourcePass)` — a fire-and-forget send that bypasses the turn handler so a forwarded `/context` doesn't get treated as a user turn.
- **Attachments** — both transports run attachments through the shared `prepareAttachments` step (#2095): convertible documents (docx/xlsx/pptx/html/csv/txt) become prompt text, images are downscaled, and every saved file also gets a `[<label> saved to: <path>]` note. ccstream then sends images and PDFs to the model as image/document content blocks, and an image in a mid-turn steer folds into the running turn. opencode sends them as file parts; codex sends only the text, so its model must open the file from the path note ([I5](#2-input-and-steering), `fold_attachments` in [Declared capabilities](#declared-capabilities)).
- **Steering** — mid-turn user messages are dispatched directly via `Backend.Inject(SourceSteer)`. On ccstream this sends the text via `writer.SendUser` at queue priority `"next"`. CC's mid-turn drain folds the message into the current `ask()` as an attachment to the next tool-result batch — the model addresses it in the same turn, the in-flight tool finishes naturally, and the response reaches the original handler. Priority `"now"` would make CC abort the in-flight ask and is deliberately not used; for "stop right now" semantics use `/reset hard`. The agent's per-session `Inbox.Enqueue` handles the routing decision — it calls `Inject(SourceSteer)` directly for delegated backends; the steer buffer is only used by API-mode agents. codex folds via `turn/steer`; opencode has no mid-turn fold and aborts and re-sends instead ([I1](#2-input-and-steering)).
- **Memory formation** — injected into the live CC session as a prompt (not branched).
- **Memory consolidation / nudge extraction / foci_summary** — batch runs via `DelegatedManager.RunBatch`: an ordinary stream-json turn on an ephemeral child session (caller's system prompt, model default `sonnet`, no permission prompts), closed as soon as the turn ends. Recorded in api.db and traced like any turn, labelled with `purpose` (#1962). On opencode and codex a batch run uses the agent's own model and can still raise a permission prompt ([P5](#3-permissions-and-safety)).
- **Auto-approval** — foci-level `[permissions]` rules are checked before any CC permission request reaches the user. Plus a static `--allowedTools` list at CC launch (merged from `[cc_backend] default_allowed_tools` and per-agent `backend_config.allowed_tools`) for rules CC can evaluate without a round-trip.
- **PreToolUse deny rules** (`claude-code` only): configurable rules, plus two preinstalled ones for `AskUserQuestion` and `CronCreate`, refuse named tool calls before they run and tell the agent why. See CONFIG.md → `[[cc_backend.pretool_rules]]`.
- **Stop rules** (`claude-code` only, per agent, none preinstalled): block the end of a turn whose final reply announces work ("starting X now") when the turn launched no background job, telling the agent to start it or say it is deferred. At most one block per turn. See CONFIG.md → `[[agents.backend_config.stop_rules]]`.
- **First-run onboarding** — the new-agent setup prompt is delivered on both backends. It is stored in `FirstRunMessage` and consumed exactly-once via `consumeFirstRunMessage`, which both transports call (the delegated path prepends it in `ComposePrompt`). Earlier it was consumed only on the API path, so claude-code agents silently lost it (#853). See WIRING.md → first-run onboarding delivery.

## Startup readiness check

Delegated backends implement `CheckReady(ctx)` on the `delegator.Delegator` interface (`delegator/backend.go`) — a startup-only probe, separate from the per-turn `TurnContract`. At boot, `checkDelegatedReadiness` calls it for every delegated agent before any startup turn is injected. ccstream shells `claude auth status` and triggers the automated re-login flow if the shared OAuth credential is dead; codex only checks its binary is on PATH; opencode is skipped because its Spec lacks `unstarted_readiness_probe` (its probe needs a server that only `Start` creates); API agents are skipped. This means a boot with an already-expired CC token recovers proactively instead of failing the first user turn (which would otherwise take the first-run onboarding down with it). See WIRING.md → startup readiness probe.

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
- **Delegated turns** close `CompletionChan` only when the backend fires `OnTurnComplete` (ccstream: on its `result` message). The post-turn goroutine blocks inline waiting for it with an **activity-based timeout** — 2 minutes of stream silence ends the wait, not a fixed deadline. Activity is tracked via the backend's `LastActivity()`, seeded at turn start and refreshed on every stream event.

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

- You want a model provider Claude Code does not serve, and can live with the gaps marked ✗ in its column (no mid-turn fold for steers, no pretool/stop rules, no post-tool nudges).

## Declared capabilities

Every delegated backend declares every capability in its `delegator.Spec` (`internal/delegator/capabilities.go`, registered from each backend's `spec.go`, #2154). The table below is generated from those declarations and checked by `internal/delegator/all`'s tests, so it cannot drift: edit the Spec, then paste the block the failing test prints. Behavioural capabilities (no method of their own) must name a proving test in `internal/delegator/all/spec_test.go`.

<!-- BEGIN GENERATED CAPABILITIES: internal/delegator/all TestBackendsDoc_CapabilityTable. Edit the Specs, not this block. -->
| Capability | Meaning | claude-code | codex | opencode |
|---|---|---|---|---|
| `post_tool_nudge` | calls TurnEvents.PostToolNudgeFunc after each tool and injects what it returns (every_n_tools, after_error, tool_pattern nudges) | ✓ | ✗ | ✗ |
| `pre_answer_nudge` | calls TurnEvents.PreAnswerNudgeFunc at the final answer and re-dispatches the turn with what it returns (pre_answer nudges) | ✓ | ✗ | ✓ |
| `streaming` | emits SessionEvents.OnTextDelta/OnThinkingDelta during a turn (live stream_output) | ✓ | ✓ | ✓ |
| `control` | accepts runtime control requests (ControlSender) | ✓ | ✓ | ✓ |
| `control_model` | applies a SetModelRequest mid-session | ✓ | ✓ | ✓ |
| `control_effort` | applies an effort change (ApplyFlagSettingsRequest effortLevel) | ✓ | ✓ | ✗ |
| `control_permission_mode` | applies a SetPermissionModeRequest mid-session | ✓ | ✓ | ✓ |
| `thinking_control` | honours the session thinking setting (/thinking) | ✗ | ✗ | ✗ |
| `model_resolve` | resolves model aliases against its own catalogue (ModelResolver) | ✗ | ✓ | ✗ |
| `voice_mode` | runs voice-originated turns at low effort (VoiceModer) | ✓ | ✓ | ✗ |
| `compaction_wait` | signals when a requested compaction finished (CompactionWaiter) | ✓ | ✓ | ✓ |
| `compaction_start_wait` | signals when a requested compaction started (CompactionStartWaiter) | ✓ | ✗ | ✓ |
| `compaction_summary` | recovers its compaction summary text (CompactionSummarizer) | ✓ | ✗ | ✓ |
| `activity` | reports its last stream activity, for activity-based timeouts (ActivityChecker) | ✓ | ✓ | ✓ |
| `autonomous_runs` | tracks background work and autonomous runs (AutonomousRunAwaiter) | ✓ | ✗ | ✗ |
| `turn_adoption` | starts runs on its own and lets foci adopt them (TurnAdopter) | ✓ | n/a | n/a |
| `stop_subagents` | stops its running subagents (SubagentStopper) | ✓ | n/a | ✗ |
| `stop_commands` | stops its background shell commands (CommandStopper) | ✓ | ✗ | ✗ |
| `subagent_status` | reports subagent status and the running background work (SubagentReporter) | ✓ | n/a | ✓ |
| `permission_response` | delivers the user's answer to a permission prompt (PermissionResponder) | ✓ | ✓ | ✓ |
| `permission_rules` | turns PermissionDecision.RulePrefix into a persistent "allow always" rule | ✓ | ✗ | ✗ |
| `questions` | routes answers to the agent's own questions (QuestionResponder) | ✓ | ✗ | ✓ |
| `elicitation` | answers MCP elicitation requests (ElicitationResponder) | ✓ | ✗ | ✗ |
| `plan_permission` | turns a typed reply into plan-revision feedback (PlanResponder) | ✓ | n/a | ✗ |
| `fold_attachments` | delivers attachments folded into a running turn (FoldAttachmentCarrier) | ✓ | ✗ | ✗ |
| `delivery_tracking` | proves each input reached the model and hands back the rest (DeliveryTracker + Spec.TranscriptChecker) | ✓ | ✗ | ✗ |
| `thread_naming` | names its own sessions (ThreadNameConsumer) | ✗ | ✓ | ✗ |
| `context_window` | reports the model's context window and usage (ContextWindowQuerier) | ✓ | ✓ | ✓ |
| `live_model` | reports the model its live process last named, without a turn result (LiveModelReporter) | ✓ | ✗ | ✗ |
| `branch` | forks and deletes its own sessions (BackendBrancher) | ✓ | ✓ | ✓ |
| `scoped_cleanup` | needs a live server to delete sessions and opens it once per sweep (RunningBackendCleaner) | ✗ | ✓ | ✓ |
| `host_hooks` | takes the gateway's HostHooks: auth-failure and rate-limit reports, pretool and stop rules (HostHooksAcceptor) | ✓ | ✗ | ✓ |
| `unstarted_readiness_probe` | CheckReady works on a constructed but unstarted backend (the startup probe) | ✓ | ✓ | ✗ |
| `pretool_rules` | enforces PreToolUse deny rules (pretool_rules) | ✓ | ✗ | ✗ |
| `stop_rules` | enforces Stop-hook rules (stop_rules) | ✓ | ✗ | ✗ |
| `relogin` | reports auth failures that foci's Claude Code re-login driver can fix (/login) | ✓ | ✗ | ✗ |
| `command_approval_allowlist` | auto-approves prompts matching foci's [permissions] allowlist (StartOptions.AutoApproveRules) | ✓ | ✓ | ✓ |
| `plan_mode` | has a /plan delivery (Spec.PlanDelivery) | ✓ | ✗ | ✓ |
| `usage_query` | reports plan usage for /mana (Spec.UsageQuery) | ✓ | ✗ | ✗ |

Why not:

- `post_tool_nudge`: codex: no mid-turn injection point is wired for the app-server; opencode: opencode exposes no per-tool point to inject a nudge at.
- `pre_answer_nudge`: codex: no mid-turn injection point is wired for the app-server.
- `control_effort`: opencode: opencode has no effort setting; SendControl accepts and ignores the request.
- `thinking_control`: claude-code: the session thinking setting is read only by the API transport; codex: the session thinking setting is read only by the API transport; opencode: the session thinking setting is read only by the API transport.
- `model_resolve`: claude-code: CC resolves model aliases itself; opencode: model names pass through to opencode unresolved.
- `voice_mode`: opencode: opencode has no effort setting to lower.
- `compaction_start_wait`: codex: the app-server reports only compaction completion.
- `compaction_summary`: codex: compaction output is server-side encrypted.
- `autonomous_runs`: codex: does not track background work; opencode: does not track background work.
- `turn_adoption`: codex (n/a): no autonomous runs to adopt; opencode (n/a): no autonomous runs to adopt.
- `stop_subagents`: codex (n/a): codex has no subagents; opencode: no API to stop a running subagent.
- `stop_commands`: codex: no API to stop a background command; opencode: no API to stop a background command.
- `subagent_status`: codex (n/a): codex has no subagents.
- `permission_rules`: codex: approvals are accept or decline only; opencode: "always" is sent as opencode's own remember flag, not a foci rule.
- `questions`: codex: the app-server has no question tool.
- `elicitation`: codex: the app-server does not surface MCP elicitation requests; opencode: opencode does not surface MCP elicitation requests.
- `plan_permission`: codex (n/a): no plan mode; opencode: plan approval is not a permission request on opencode.
- `fold_attachments`: codex: a message folded into a running turn carries text only; opencode: a message folded into a running turn carries text only.
- `delivery_tracking`: codex: inputs are fire-and-forget: no proof of consumption; opencode: inputs are fire-and-forget: no proof of consumption.
- `thread_naming`: claude-code: CC does not name its sessions; agents get the set_session_alias tool instead; opencode: foci does not take opencode's session titles.
- `live_model`: codex: its model reaches foci only through thread start and turn results; opencode: its model is reported only at session idle.
- `scoped_cleanup`: claude-code: fork and cleanup are local transcript file operations, no server needed.
- `host_hooks`: codex: reports no auth failures or rate limits foci acts on, and has no pretool or stop hooks.
- `unstarted_readiness_probe`: opencode: CheckReady needs the server that only Start creates.
- `pretool_rules`: codex: no PreToolUse rule engine is wired to the codex hook; opencode: opencode has no PreToolUse hook.
- `stop_rules`: codex: no Stop hook; opencode: opencode has no Stop hook.
- `relogin`: codex: the re-login driver logs Claude Code in; opencode: the re-login driver logs Claude Code in.
- `plan_mode`: codex: no /plan delivery.
- `usage_query`: codex: no plan usage to report; opencode: no plan usage to report.

<!-- END GENERATED CAPABILITIES -->

## Delegated backend feature parity

Feature-by-feature comparison of the three structured delegated backends, from the #2151 survey (origin/main `5d1dbf759`, 2026-10-02). ccstream (`backend = "claude-code"`) is the reference.

For any feature that is a declared capability, the [generated table](#declared-capabilities) is the source of truth for yes/no. The tables below hold only the features it does not cover. The rows that duplicated it were removed (#2183); their footnotes and evidence are kept, named by capability (e.g. `questions`), because they say how a backend does it, or where a ✓ is partial.

Path shorthand: `cc/` = `internal/delegator/ccstream/`, `oc/` = `internal/delegator/opencode/`,
`cx/` = `internal/delegator/codex/`, `dg/` = `internal/delegator/`, `ag/` = `internal/agent/`,
`gw/` = `cmd/foci-gw/`.

Cell key: **✓** equivalent · **✗** absent · **n/a** cannot apply to that backend (footnote says
why) · **[n]** partial or different, see [footnote](#parity-footnotes) n. Every row has an ID.
The [evidence](#parity-evidence) list gives file:symbol for each ✓ and partial, and what was
grepped for each ✗. Row IDs are not renumbered, so the gaps are the removed rows.

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
| T10 | Process death | In-flight turn completes on death; next `Get` respawns | ✓ | ✓ | ✓ |
| T11 | Bounded shutdown | `Close` cannot hang (graceful, then kill) | ✓ | ✓ | ✓ |
| T15 | Transport keep-alive | Liveness heartbeat on the transport | ✓ | ✓ [6] | ✗ |

### 2. Input and steering

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| I1 | Steer mid-turn | `SourceSteer` folds into the running turn | ✓ | [7] | ✓ |
| I2 | User follow-up mid-turn | `SourceUser` while in flight | ✓ | [8] | ✓ |
| I3 | System never steers | `SourceSystem` in flight returns `ErrTurnInFlight`; idle check + begin are atomic | ✓ | ✓ | ✓ |
| I4 | Steer-at-idle guard | Steer at idle with no `Turn` returns `ErrTurnNotInFlight` | ✓ | ✓ | [9] |
| I5 | Attachments, fresh turn | Images/PDF sent as content to the model | ✓ | ✓ | ✗ [10] |
| I7 | Slash passthrough | `SourcePass` forwards an unhandled `/cmd` | ✓ | ✓ | ✗ [11] |
| I9 | Closed-transport error | `ErrBackendClosed` marks a write that raced teardown (lower log severity) | ✓ | ✗ | ✗ |

### 3. Permissions and safety

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| P2 | Always allow | Persist an approval beyond this call | ✓ | ✓ [12] | ✗ |
| P4 | Launch allowlist | `allowed_tools` / `[cc_backend] default_allowed_tools` pre-approval | ✓ | n/a [14] | n/a [14] |
| P5 | Unattended mode | `skip_permissions` and `StartOptions.SkipPermissions` (batch runs): never prompt a chat | ✓ | ✓ [15] | ✗ [15] |
| P7 | Prompt cancel listeners | Backend-cancelled prompt disables its stale buttons | ✓ | ✓ | ✗ [17] |
| P8 | Prompts-cleared drain | `SetOnPromptsCleared` unblocks `WaitForPermission` | ✓ | ✓ | ✓ |
| P9 | Rich prompt body | Edit diffs; ExitPlanMode plan sent as a document | ✓ | ✗ | [18] |

### 4. Questions, plan mode, elicitation

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| Q2 | /stop cancels question | `CancelPendingQuestion` | ✓ | ✓ | ✗ |

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
| C5 | Declined compaction | `ErrCompactionNoBoundary` returns at once instead of timing out | ✓ | ✗ | ✗ |
| C8 | Reload bounce | `reload_on_compact` bounce + resume nudge (agent layer) | ✓ | ✓ | ✓ [31] |
| C10 | Cache TTL | `Spec.CacheTTL` (keepalive interval validation) | ✓ | ✗ | ✓ |

### 7. Cost, usage and limits

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| U1 | Ledger booking | `Spec.LedgerKey` per-call cost ledger | ✓ | ✓ | ✓ |
| U2 | Self-closing activity | `Spec.ClosesTurnActivity`: spend may outlive the turn | ✓ | ✗ | ✗ [32] |
| U3 | Cost divergence check | Backend-reported vs calculated cost | ✓ | ✓ | ✗ |
| U4 | Expectation guards | `ExpectationGuard` invariants + version tracking | ✓ | ✗ | ✓ |
| U5 | Resume cost baseline | Cumulative counters on `--resume` | ✓ | n/a [33] | n/a [33] |
| U6 | Utilisation notice | Rate-limit utilisation notice to the human's chat | ✓ | ✗ | ✗ |
| U7 | Limit gate | Usage/session limit engages `Agent.EngageRateLimit` | ✓ | ✓ | ✗ [34] |
| U9 | Turn usage | Token usage on `TurnResult` | ✓ | ✓ | ✓ |

### 8. Models and effort

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| M1 | Launch model | Model ladder (override, config, default) applied at Start | ✓ | ✓ | ✓ |
| M4 | Live catalogue | `modelcaps` live model list | ✓ [37] | ✗ | ✓ |
| M5 | Effort | Launch `EffortFunc` + runtime `ApplyFlagSettingsRequest` | ✓ | n/a [38] | [35] |
| M7 | Batch models | `Spec.BatchDefaultModel` / `BatchCheapModel` | ✓ | ✗ | ✗ |
| M8 | Silent model swap | Backend-side model fallback is surfaced | ✓ | ✗ | [39] |

### 9. Sessions, resume and branching

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| R1 | Session id | `SetOnSessionReady` persists the backend session id | ✓ | ✓ | ✓ |
| R2 | Resume | `ResumeSessionID` after respawn/restart | ✓ | ✓ | ✓ |
| R4 | Cleanup / sweep | `CleanupSession` (+ `RunningBackendCleaner` when it needs a live server) | ✓ | ✓ | ✓ |
| R5 | Retention period | Backend transcript retention for the resume-missed notice | ✓ | ✗ | ✗ |
| R6 | Session file path | `SessionFilePath` | ✓ | ✗ | ✓ |
| R7 | Batch runs | `DelegatedManager.RunBatch` ephemeral session | ✓ | [41] | [41] |

### 10. UI surfaces

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| D1 | Text blocks | `OnText` | ✓ | ✓ | ✓ |
| D3 | Tool start | `OnToolStart` | ✓ | ✓ | ✓ |
| D4 | Tool result | `OnToolEnd` with output (tool-call display, "Show results") | ✓ [42] | ✓ | ✓ |
| D5 | Tool runtime note | Slow-call runtime note into the model's context (#2125) | ✓ | ✗ | ✗ |
| D6 | /status detail | `StatusDetail` | ✓ | ✗ | ✓ |

### 11. Ops and health

| ID | Feature | What | ccstream | opencode | codex |
|---|---|---|---|---|---|
| O2 | Auth-failure detection | Recognise a dead credential | ✓ | ✓ [44] | ✗ |
| O4 | Stderr capture | Subprocess stderr logged | ✓ | ✓ | ✗ |
| O5 | Per-session exec bridge | `FOCI_SOCK`/`BASH_ENV`/`FOCI_SESSION_KEY` reach the right session's shell | ✓ | ✓ | ✓ [45] |
| O6 | System prompt ownership | Foci's prompt replaces the backend's own | ✓ | ✓ | ✓ |
| O7 | Pool acquire serialisation | One server per agent, no double-spawn | n/a [46] | ✓ | ✓ |
| O8 | Orphan reaping | Kill stray backend processes from a previous gateway | ✗ [47] | ✓ | ✗ [47] |
| O9 | Unhandled server requests | Every backend-to-foci request gets an answer | ✓ | ✓ | ✗ [21] |

### 12. Config keys that apply to only some backends

Generated from each backend's `Spec.ConfigKeys` / `Spec.StartFields` (`internal/delegator/config_schema.go`, #2178). `make lint` checks those declarations against each backend package's source, and the gateway warns at startup (and `foci-gw -check-config` lists) every `backend_config` key an agent sets that its backend ignores.

<!-- BEGIN GENERATED CONFIG SCHEMA: internal/delegator/all TestBackendsDoc_ConfigSchemaTable. Edit the Specs, not this block. -->
| `backend_config` key | claude-code | codex | opencode |
|---|---|---|---|
| `model` (gateway) | ✓ | ✓ | ✓ |
| `allowed_tools` | ✓ | ✗ | ✗ |
| `pretool_rules` (gateway) | ✓ | ✗ | ✗ |
| `stop_rules` (gateway) | ✓ | ✗ | ✗ |
| `binary` | ✓ | ✓ | ✓ |
| `idle_timeout` (gateway) | ✓ | ✓ | ✓ |
| `skip_permissions` | ✓ | ✗ | ✓ |
| `env` (gateway) | ✓ | ✓ | ✓ |
| `hostname` | ✗ | ✗ | ✓ |
| `server_auth` | ✗ | ✗ | ✓ |
| `log_level` | ✗ | ✗ | ✓ |
| `port` | ✗ | ✗ | ✓ |
| `default_permission` | ✗ | ✗ | ✓ |

(gateway) = read by the gateway for the backend, not by the backend package; `env` reaches a backend that reads `StartOptions.Env`, `pretool_rules` / `stop_rules` one declaring `pretool_rules` / `stop_rules`.

- claude-code folds the global `[cc_backend]` section into its backend_config (Spec.ConfigFamily).
- opencode folds the global `[opencode_backend]` section into its backend_config (Spec.ConfigFamily).
- codex also reads `api_key`, `sandbox`, which no config key supplies.

| `StartOptions` field | claude-code | codex | opencode |
|---|---|---|---|
| `WorkDir` | ✓ | ✓ | ✓ |
| `SystemPrompt` | ✓ | ✓ | ✓ |
| `Model` | ✓ | ✓ | ✓ |
| `AgentID` | ✓ | ✓ | ✓ |
| `Label` | ✓ | ✓ | ✗ |
| `ResumeSessionID` | ✓ | ✓ | ✓ |
| `SessionKey` | ✓ | ✓ | ✓ |
| `BatchOnly` | ✗ | ✓ | ✗ |
| `SkipPermissions` | ✓ | ✗ | ✓ |
| `ExecRegistry` | manager | manager | manager |
| `Env` | ✓ | ✓ | ✓ |
| `AutoApproveRules` | ✓ | ✓ | ✓ |
| `SubagentMaxAge` | ✓ | ✗ | ✗ |
| `SystemPromptFunc` | manager | manager | manager |
| `Effort` | ✓ | ✓ | ✗ |
| `EffortFunc` | manager | manager | manager |
| `ModelFunc` | manager | manager | manager |
| `CompactionPromptFunc` | ✗ | ✓ | ✓ |

manager = resolved by the DelegatedManager before Start; no backend reads it.

<!-- END GENERATED CONFIG SCHEMA -->

Notes:
- `skip_permissions` on opencode: per-ask answers, `oc/permissions.go:answerUnattended`.
- `[cc_backend].background_task_max_age` reaches the backend as `StartOptions.SubagentMaxAge`.
- `reload_on_compact` (agent level, not `backend_config`) applies to every backend.
- codex's `api_key` / `sandbox`: footnote 48.

### Parity footnotes

1. opencode and codex run ONE process per agent, shared by that agent's sessions and refcounted
   (`oc/opencode.go:acquireServer`; `cx/codex.go` facades over a `sharedPool` owner). ccstream
   runs one `claude` per session.
2. opencode waits for two signals, `/global/health` and the SSE subscriber attaching
   (`oc/server.go:waitForSubscriber`, #1722).
3. codex sets typing on `turn/started` and clears it at completion only
   (`cx/handlers.go:onTurnStarted`, `completeTurn`). ccstream also re-asserts it on tool progress
   and subagent activity, so long tool calls keep the indicator alive.
4. `autonomous_runs` / `turn_adoption`: nothing in opencode or codex adopts a run it did not start. Whether either backend can start
   a root-session run on its own (as CC does after a background task finishes) is unverified.
5. `pre_answer_nudge` / `post_tool_nudge`: codex declares both No; opencode declares
   `pre_answer_nudge` Yes and `post_tool_nudge` No. Both the turn
   (`ag/turn_delegated.go`, which arms the nudge funcs) and the nudge scheduler
   (`gw/agents_setup.go:nudgeCapabilities`, which skips unsupported rules with a warning) read
   that one declaration. opencode re-dispatches at `session.idle`
   (`oc/handlers.go:tryPreAnswerRedispatch`): the follow-up is a fresh `prompt_async` inside the
   same turn, and a failed send completes the turn with the first-round answer. With no post-tool
   callback, the gate's `nudge_pre_answer_min_tools` reads the result's tool count (#2176).
   Post-tool nudges need a per-tool hook; ccstream gets it from `foci-cc-hook`.
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
12. Every backend answers through one `delegator.PermissionResponder` taking a
    `PermissionDecision{Allow, Remember, Message, RulePrefix}` (#2154 Phase 3);
    `ag/delegated_permission.go:permissionDecision` builds it from the button data. opencode's
    "Always Allow" (`always`) is `Remember`, sent as opencode's own remember flag. ccstream's
    `allow_always:<prefix>` also carries `RulePrefix` and becomes a session prefix rule (it declares
    `permission_rules`). codex has neither and answers a plain accept.
13. `command_approval_allowlist`, partial on codex: it auto-approves command approvals only (`cx/permissions.go:tryAutoApprove`, called from
    `onCommandApproval`). File-change approvals always prompt, and `item/permissions/requestApproval`
    is always declined (`onPermissionApproval`).
14. `--allowedTools` is a Claude Code launch flag. opencode's analogue is
    `default_permission` + PATCH `/config`; codex's would be the sandbox/approval policy.
15. ccstream passes `--dangerously-skip-permissions` and denies anything CC still asks
    (`cc/permissions.go:denyUnattended`, #2096). opencode has no launch flag and shares one
    server across the agent's sessions, so `surfacePermission` answers each ask of a
    skip-permissions session itself (`oc/permissions.go:answerUnattended`, #2153): permissions
    are allowed once and questions declined, with no prompt. codex does not read
    `StartOptions.SkipPermissions` (`grep SkipPermissions|skip_permissions`: no hits), and
    `DelegatedManager` wires the chat permission prompt for batch sessions too
    (`ag/delegated_manager.go`, `SetPermissionPromptFunc` is not gated on `isBatch`). So a
    batch run (consolidation, nudge extraction, summary) on codex can put a permission
    prompt in the owner's chat; codex has no reachable equivalent (footnote 48).
16. `control_permission_mode`, not live on codex: it queues the approval policy for the next `turn/start` (`cx/control.go:applyPendingControls`);
    `plan` maps to `on-request`, so there is no plan mode.
17. `cx/callbacks.go:RegisterPromptCancelListener` is a TODO no-op. `serverRequest/resolved` clears
    the pending entry and fires prompts-cleared (`cx/handlers.go:onServerRequestResolved`) but
    stale buttons stay live.
18. codex shows the command text, or the list of changed files for a file change
    (`cx/permissions.go:onFileChangeApproval`, `lookupItemDetail`). No diff.
19. `pretool_rules` / `stop_rules`: codex already installs a `PreToolUse` hook (`cx/hooks.go`, `cmd/foci-codex-hook`), currently
    only to rewrite Bash commands for per-thread env. That is the natural carrier for pretool
    rules; codex's Stop-hook support was not checked.
20. `questions` on ccstream works, but the default pretool rule `ask_user_question` denies CC's AskUserQuestion and
    redirects the model to `foci_ask`, so this path is a fallback.
21. `cx/reader.go:handleServerRequest` handles three approval methods; any other server request
    (e.g. a user-input or MCP elicitation request) hits `default: logDebugf("unhandled server
    request ...")` and is never answered. If codex sends one, that turn waits forever.
    Which request methods codex 0.145 can send was not verified.
22. `elicitation`: opencode's MCP client does not advertise elicitation upstream (WIRING.md, opencode issue
    #23066).
23. `plan_mode`: opencode sends the prompt with per-request `agent: "plan"` (`oc/plan.go:planDelivery`).
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
30. Unused. (Was the C7 row: the `SetOnCompactionStart` / `SetOnCompactionDone` callbacks on
    ccstream and opencode, which had no caller and were deleted in #2154.)
31. Agent-layer generic: closes the backend keeping the resume id; codex re-sends
    `baseInstructions` on `thread/resume` (`cx/lifecycle.go:resumeThread`).
32. A codex child that outlives its turn still reads `still_running` false early (not yet
    handled for codex).
33. opencode and codex book each message/usage event as it arrives, so there is no cumulative
    counter to rebase.
34. `grep -i 'limit|ratelimit'` in `cx/`: no handler; unknown notifications fall to
    `logDebugf("unhandled notification")`.
35. `control_model` and runtime effort are not live: opencode stores the model for the next prompt body after validating it with
    `opencode models` (`oc/control.go:SendControl`); codex queues model and effort for the next
    `turn/start` (`cx/control.go`). ccstream applies `set_model` / `apply_flag_settings` to the
    running process.
36. `model_resolve`: ccstream passes raw aliases (`opus`, `sonnet`) and CC resolves them. opencode resolves by unique
    substring (`oc/model_validate.go:resolveModel`) but not through `ModelResolver`, so the
    persisted id stays the user's alias.
37. Fetched at gateway level, not by the backend (`gw/main.go`, Anthropic `/v1/models` into
    `modelcaps.BackendCCStream`). `modelcaps.BackendKey("opencode")` falls to the default case, so
    opencode has no catalogue and no persister.
38. opencode has no effort concept (`oc/control.go`: ApplyFlagSettings is a logged no-op).
39. codex handles `model/rerouted` by updating its model and logging at INFO (`cx/reader.go`). ccstream
    logs `model_refusal_fallback` at WARN with CC's own text (#1968).
40. `branch`: `TruncateAfter > 0` is rejected by all three. ccstream copies the transcript and appends
    fork-boundary closures for open tool calls and background tasks (`cc/branch.go:forkTranscript`).
    opencode uses `POST /session/:id/fork` on the already-running server. codex uses `thread/fork`
    and requires a running backend (`Spec.ForkNeedsRunning`).
41. Generic in `DelegatedManager.RunBatch`, but see footnote 15 (permission prompts) and M7 (no
    batch default model: opencode/codex batch runs fall back to the agent's own model).
42. Tool results come from `foci-cc-hook` PostToolUse/PostToolUseFailure hooks
    (`cc/hooks.go:handleHookResponse`); if the hook binary is missing ccstream runs without
    tool results. opencode and codex get results natively from their event streams.
43. `unstarted_readiness_probe`: the startup probe skips a backend without it (`gw/notifications.go`). opencode's `CheckReady` needs a server
    that only `Start` creates, and `/global/health` cannot see provider auth. codex's
    `CheckReady` only checks the binary is on PATH (`cx/lifecycle.go:CheckReady`).
44. `relogin`: opencode detects `ProviderAuthError` and HTTP 401 and fans out to every session on the
    server (`oc/authfail.go`), but `gw/agents_delegated.go` only logs it: auth is per-provider
    (`opencode auth login <provider>`), so there is no single relogin flow to automate.
45. Three mechanisms: ccstream has a process per session, so the env is baked in; opencode uses a
    generated `shell.env` plugin reading `session-env/<id>.json` (`oc/session_env.go`); codex
    rewrites each Bash command with a PreToolUse hook (`cx/hooks.go:bindThreadEnv`). All share
    `dg/sessionenv`.
46. ccstream has no per-agent pool (WIRING.md "Per-agent acquire serialisation").
47. `opencode.ReapOrphanedServers` runs at startup and `CloseAllServers` at shutdown, as the opencode Spec's `OnGatewayStart` / `OnGatewayShutdown` (`gw/main.go`, `gw/shutdown.go` run every Spec's hooks).
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
- `activity`: `cc/ccstream.go:LastActivity` | `oc/activity.go:LastActivity` | `cx/codex.go:LastActivity`
- T10: `cc/lifecycle.go:finalizeExit` | `oc/lifecycle.go:Server.finalizeExit`, `oc/opencode.go:IsRunning` | `cx/reader.go:onReaderStopped`
- T11: `cc/lifecycle.go:Close` | `oc/lifecycle.go:Server.Close` | `cx/lifecycle.go:Close` (`closeGracefulWait` then Kill)
- `autonomous_runs`, `turn_adoption`: `cc/inject.go:AdoptRunningTurn`, `AwaitingAutonomousRun`, `cc/callbacks.go:SetOnAutonomousOpen` | ✗ grep `AdoptRunningTurn|AwaitingAutonomousRun|SetOnAutonomousOpen` | ✗ same
- `pre_answer_nudge`: `cc/complete.go:tryPreAnswerRedispatch` | `oc/handlers.go:tryPreAnswerRedispatch` | ✗ grep `PreAnswerNudgeFunc`
- `post_tool_nudge`: `cc/hooks.go:handleHookResponse` (`PostToolNudgeFunc`) | ✗ grep `PostToolNudgeFunc` | ✗ same
- T15: `cc/lifecycle.go:runKeepAlive` | `oc/subscriber.go:onHeartbeat` | ✗ grep `keep.?alive|heartbeat`

**Input and steering**
- I1: `cc/inject.go:sendFold` (priority `next`) | `oc/inject.go:injectSteer`, `flushSteerBuf` | `cx/inject.go:steerTurn` (`turn/steer`)
- I2: `cc/inject.go:ImmediateInject` | `oc/inject.go:injectUser` (steerBuf) | `cx/inject.go:ImmediateInject` → `steerTurn`
- I3: `cc/inject.go:tryBeginTurn` | `oc/inject.go:injectSystem` → `tryBeginTurn` | `cx/inject.go:beginTurn` (`turnActive` under `turnMu`)
- I4: `cc/inject.go:ImmediateInject` | `oc/inject.go:injectSteer` | `cx/inject.go:steerTurn` only
- I5: `cc/inject.go:sendToPaneWithAttachments`, `contentBlocks`, `attachmentBlockType` | `oc/inject.go:buildPromptBody` (file parts) | ✗ grep `Attachments|attachment|mime` (only `imageGeneration` tool items)
- `fold_attachments`: `cc/inject.go:FoldsAttachment` | ✗ grep `FoldsAttachment` | ✗ same
- I7: `cc/inject.go:ImmediateInject` (SourcePass) | `oc/inject.go:injectCommand` | ✗ `cx/inject.go:ImmediateInject` returns nil
- `delivery_tracking`: `cc/delivery.go:SetDeliveryHooks`, `OnInputAck`, `InputInTranscript` (`Spec.TranscriptChecker`) | ✗ grep `SetDeliveryHooks|Redeliveries|Refs` | ✗ same
- I9: `cc/lifecycle.go:transportGone` + `ErrBackendClosed` use | ✗ grep `ErrBackendClosed` | ✗ same

**Permissions and safety**
- `permission_response`: `cc/permissions.go:handleToolRequest`, `RespondToPermission` | `oc/permissions.go:onPermissionUpdated`, `surfacePermission`, `RespondToPermission` | `cx/permissions.go:onCommandApproval`, `onFileChangeApproval`, `RespondToPermission`
- P2: `cc/permissions.go:respondWithRule` (`RulePrefix`) | `oc/permissions.go:RespondToPermission` (`Remember`) | ✗ only Allow/Deny choices in `cx/permissions.go`
- `command_approval_allowlist`: `cc/autoapprove.go:autoApprovePermission` | `oc/permissions.go:checkAutoApprove` | `cx/permissions.go:tryAutoApprove`
- P4: `cc/lifecycle.go:Start` (`--allowedTools` from `cfg["allowed_tools"]`) | n/a | n/a
- P5: `cc/lifecycle.go:Start` (`--dangerously-skip-permissions`), `cc/permissions.go:denyUnattended` | `oc/backend_lifecycle.go:Start` (`skipPermissions`), `oc/permissions.go:answerUnattended` | ✗ grep `SkipPermissions|skip_permissions|dangerously`
- `control_permission_mode`: `cc/control.go:SendControl` (`set_permission_mode`) | `oc/control.go:SendControl` (`patchConfig`, `mapPermissionMode`) | `cx/control.go:SendControl`, `codexApprovalPolicy`
- P7: `cc/callbacks.go:RegisterPromptCancelListener` + `dg/outstanding.go` | `oc/opencode.go:RegisterPromptCancelListener`, `oc/permissions.go:onPermissionReplied` | ✗ `cx/callbacks.go` TODO
- P8: `cc/callbacks.go:SetOnPromptsCleared` | `oc/opencode.go:SetOnPromptsCleared` | `cx/permissions.go:respondApproval`, `cx/handlers.go:onServerRequestResolved`
- P9: `cc/permissions.go:formatEditDiff`, `planAttachmentPath` | ✗ `oc/permissions.go:surfacePermission` sends title only (grep `diff`) | `cx/permissions.go:lookupItemDetail`
- `pretool_rules`: `dg/pretool`, `cc/hooks.go:buildHookSettingsJSON`, `cc/callbacks.go:SetHostHooks` (`PreToolRules`) | ✗ grep `pretool\.` | ✗ same
- `stop_rules`: `dg/stoprule`, `cc/callbacks.go:SetHostHooks` (`StopRules`), `cc/hooks.go:logStopVerdict` | ✗ grep `stoprule\.` | ✗ same

**Questions, plan, elicitation**
- `questions`: `cc/userquestion.go:handleUserQuestion`, `RespondToQuestion` | `oc/permissions.go:handleQuestionPermission`, `RespondToQuestion` | ✗ grep `RespondToQuestion|userInput|request_user_input`
- Q2: `cc/userquestion.go:CancelQuestion`, `HasPendingQuestion` | `oc/permissions.go:CancelQuestion`, `HasPendingQuestion` | ✗ (needs QuestionResponder)
- `elicitation`: `cc/elicitation.go:OnElicitationRequest`, `RespondToElicitation`, `OnElicitationComplete` | n/a | ✗ grep `elicit`
- `plan_mode`: `cc/plan.go:planDelivery` | `oc/plan.go:planDelivery` | ✗ `codex/spec.go` declares `plan_mode` No
- `plan_permission`: `cc/permissions.go:HasPendingPlanPermission`, `CancelPlanWithFeedback`; `ag/inbox.go` plan-cancel-by-message | ✗ grep `HasPendingPlanPermission` | ✗ same

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
- `compaction_wait`: `cc/compaction.go:ArmCompactionWait`, `WaitForCompaction` | `oc/compaction.go` same | `cx/lifecycle.go` same
- `compaction_start_wait`: `cc/compaction.go:ArmCompactionStartWait` | `oc/compaction.go:ArmCompactionStartWait`, `oc/handlers.go:handleCompactionPart` | ✗ grep `ArmCompactionStartWait`
- C5: `cc/compaction.go:signalCompactionAbort` | ✗ grep `ErrCompactionNoBoundary` | ✗ same
- `compaction_summary`: `cc/compaction.go:CompactionSummary` | `oc/compaction.go:CompactionSummary` | ✗ grep `CompactionSummary`
- C8: `ag/compaction.go:runDelegatedCompact`, `ag/delegated_manager.go:BounceSessionIfPromptChanged` (generic)
- `context_window`: `cc/control.go:GetContextWindow` | `oc/context_usage.go:GetContextWindow` | `cx/context_window.go:GetContextWindow`
- C10: `cc/control.go:CacheTTL` (1h) | ✗ grep `CacheTTL` | `cx/context_window.go:CacheTTL` (5m)

**Cost, usage, limits**
- U1: `cc/ccledger.go:LedgerBackend`, `cc/ccbook.go` | `oc/ledger.go` | `cx/ledger.go`
- U2: `cc/ccledger.go:ClosesTurnActivity` | ✗ grep `ClosesTurnActivity` | ✗ same
- U3: `cc/ccbook.go:checkDivergence` | `oc/ledger.go` via `dg/costcheck.go:CostDivergenceChecker` | ✗ grep `Divergence`
- U4: `cc/expectations.go` | ✗ grep `ExpectationGuard|violated` | `cx/expectations.go:checkTokenUsage`
- U5: `cc/cost.go:resumeBaseline` | n/a | n/a
- U6: `cc/handlers.go:OnRateLimit`, `cc/ratelimit.go:FormatRateLimitNotice`, `HostHooks.OnRateLimitNotice` | ✗ | ✗ grep `rate.?limit`
- U7: `cc/handlers.go:OnAssistant` (`syntheticSessionLimitText`) → `HostHooks.EngageRateLimit` | `oc/ratelimit.go:handleRateLimitRetry` → `HostHooks.EngageRateLimit` | ✗ grep `limit`
- `usage_query`: `cc/usage_oneshot.go:QueryUsage`, `cc/spec.go` `UsageQuery` | ✗ `oc/spec.go` declares `usage_query` No | ✗ same
- U9: `cc/handlers.go:OnResult` | `oc/handlers.go:onMessageUpdated` | `cx/handlers.go:onTokenUsage`

**Models**
- M1: `cc/lifecycle.go:Start` (`--model`) | `oc/backend_lifecycle.go:Start` (`resolveModelFn`) | `cx/lifecycle.go:prepareConfiguredModel`
- `control_model`: `cc/control.go:sendSetModel` | `oc/control.go:SendControl` | `cx/control.go:SendControl`
- `model_resolve`: n/a | `oc/model_validate.go:resolveModel` (not the interface) | `cx/model_resolver.go:ResolveModel`
- `live_model`: `cc/livemodel.go:LiveModel` (reads `lastModel`, set by `OnSystem` init, top-level `OnAssistant` and `get_context_usage` responses; a confirmed `set_model` clears it until one of those restates the new id) | ✗ grep `LiveModel` | ✗ same
- M4: `gw/main.go` modelcaps fetcher for `BackendCCStream` | ✗ grep `modelcaps` | `cx/modelcaps.go:refreshModelCaps` → `publishModelCaps`
- M5: `cc/lifecycle.go:Start` (`--effort`), `cc/control.go:SendControl` | n/a | `cx/control.go` `pendingEffort`
- `voice_mode`: `cc/voicemode.go` | n/a | `cx/voicemode.go`
- M7: `cc/ccstream.go:BatchDefaultModel`, `BatchCheapModel` | ✗ grep `BatchDefaultModel|BatchCheapModel` | ✗ same
- M8: `cc/handlers.go:OnSystem` `model_refusal_fallback` | ✗ grep `fallback|reroute` | `cx/reader.go` `model/rerouted`
- `streaming` (thinking): `cc/handlers.go:OnStreamEvent` | `oc/handlers.go:handleReasoningPart` | `cx/handlers.go:onReasoningDelta`

**Sessions**
- R1: `cc/handlers.go:OnSystem` init → `onSessionReady` | `oc/opencode.go:SetOnSessionReady` | `cx/callbacks.go:SetOnSessionReady`
- R2: `cc/lifecycle.go:Start` (`--resume`) | `oc/backend_lifecycle.go:resumeSession` | `cx/lifecycle.go:resumeThread`
- `branch`: `cc/branch.go:ForkSession`, `forkTranscript` | `oc/branch.go:ForkSession` | `cx/branch.go:ForkSession`, `Spec.ForkNeedsRunning`
- R4: `cc/branch.go:CleanupSession` | `oc/branch.go:CleanupSession`, `OpenCleanupScope` | `cx/branch.go:CleanupSession`, `OpenCleanupScope`
- R5: `cc/retention.go:CleanupPeriod` (`Spec.ResumeRetention`) | ✗ (no `ResumeRetention`) | ✗ same
- R6: `cc/ccstream.go:SessionFilePath` | ✗ `oc/opencode.go:SessionFilePath` returns `""` | `cx/codex.go:SessionFilePath`
- R7: `ag/delegated_manager.go` `batchSpecFor` (generic)
- `thread_naming`: ✗ grep `ThreadName` | ✗ same | `cx/codex.go:ConsumeThreadName`, `cx/reader.go` `thread/name/updated`

**UI**
- D1, D3, `streaming` (text): `cc/handlers.go:OnAssistant`, `OnStreamEvent` | `oc/handlers.go:handleTextPart`, `handleToolPart` | `cx/handlers.go:onItemStarted`, `onItemCompleted`, `onAgentMessageDelta`
- D4: `cc/hooks.go:handleHookResponse` | `oc/handlers.go:handleToolPart` | `cx/handlers.go:onItemCompleted`
- D5: `cmd/foci-cc-hook` `slowToolThreshold`, `cc/hooks.go` `tool_runtime_shown` | ✗ grep `runtime|duration_ms` | ✗ same
- D6: `cc/control.go:StatusDetail` | ✗ `oc/context_usage.go:StatusDetail` returns `""` | `cx/codex.go:StatusDetail`

**Ops**
- `unstarted_readiness_probe`: `cc/readiness.go:CheckReady` | `oc/backend_lifecycle.go:CheckReady` (skipped in `gw/notifications.go`) | `cx/lifecycle.go:CheckReady`
- O2: `cc/authfail.go:isAuthFailure` | `oc/authfail.go:authCheckingTransport`, `fanOutAuthFailure` | ✗ grep `auth|401`
- `relogin`: `gw/agents_delegated.go:triggerRelogin` (claude-code only), `internal/relogin` | n/a | ✗
- O4: `cc/lifecycle.go:captureStderr` | `oc/lifecycle.go:captureStderr` | ✗ grep `Stderr` in `cx/lifecycle.go`
- O5: `cc/env.go:buildEnv` | `oc/session_env.go` | `cx/hooks.go:bindThreadEnv`
- O6: `cc/lifecycle.go` initialize `systemPrompt` | `oc/blank_system.go:EnsureBlankSystemPlugin` | `cx/lifecycle.go` `BaseInstructions`
- O7: n/a | `oc/opencode.go:acquireServer` (`acquireLocks`) | `cx/lifecycle.go:Start` (`acquireLocks`)
- O8: ✗ grep `Reap|orphan` | `oc/reap.go:ReapOrphanedServers` | ✗ same
- O9: `cc/reader.go:dispatch` (all `control_request` subtypes answered) | SSE (no server-to-client requests) | ✗ `cx/reader.go:handleServerRequest` default


### Known issues

Found by the parity survey, beyond the table:

- **Unreachable codex config:** `sandbox` and `api_key` (footnote 48).
- **codex can hang on an unknown server request** (footnote 21).
- **Batch runs on opencode/codex can prompt the owner's chat** (footnote 15).

## Adding a new delegated backend

### Interface-level minimum (the `dg.Delegator` interface, 18 methods)

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

### Declare a Spec first

A backend exists only through `delegator.Register(Spec)`, called from its package's `spec.go`
`init()`, and its package must be blank-imported by `internal/delegator/all`. The Spec declares
every capability (`Yes()`, `No(reason)` or `NotApplicable(reason)`; there is no default) plus the
static data that used to live in name switches. `go test ./internal/delegator/all/` (run it with
`make test-one`) fails until the Spec is complete and true: a declaration that disagrees with the
backend's method set, a behavioural Yes without a proving test, or a stale capability table in
this file. Outside `internal/delegator`, `make lint` (`scripts/find-backend-capability-bypass`)
rejects a backend-name comparison, a type assertion on a `Delegator` and a backend-package import.
See [Declared capabilities](#declared-capabilities) and
`internal/delegator/capabilities.go`.

### Optional capabilities, in suggested order

- **Effectively required:**
  - Book your calls in the cost ledger and set `Spec.LedgerKey`: without it no cost is booked and
    `LogUsage` logs the gap.
  - `ActivityChecker`: idle reaping and stream-silence timeouts.
  - `CompactionWaiter` + `CompactionStartWaiter`: without them compaction falls back to
    `WaitForTurn`.
  - `ContextWindowQuerier`.
  - A permission responder with one of the two `RespondToPermission` signatures
    (`ag/delegated_permission.go`).
- **High value:**
  - `ControlSender` (model and permission mode).
  - `BackendBrancher` (+ `Spec.ForkNeedsRunning` / `RunningBackendCleaner` if the store lives
    behind a live server).
  - `CompactionSummarizer`, `Spec.CacheTTL`, `QuestionResponder`.
  - `SetOnSubagentStatus` / `SetOnSubagentRunning`: reuse `dg.SubagentTracker` rather than a
    private tracker, so the status line and pending-work gate work.
  - Auth-failure and rate-limit callbacks wired in `gw/agents_delegated.go:configureDelegated`.
  - Honour `StartOptions.SkipPermissions`.
- **Feature-specific:** `VoiceModer` and effort (only if the backend has an effort knob),
  `ModelResolver` + modelcaps catalogue, `PlanResponder`, `ElicitationResponder`,
  `DeliveryTracker`, `AutonomousRunAwaiter` / `AdoptRunningTurn`, `FoldAttachmentCarrier`,
  `ThreadNameConsumer`, `Spec.BatchDefaultModel` / `BatchCheapModel`, `Spec.ClosesTurnActivity`, a
  per-tool hook (pretool rules, stop rules, post-tool nudges, runtime notes), expectation guards.

### Host callbacks: HostHooks

The gateway builds one `delegator.HostHooks` per agent (auth failure, rate-limit notice,
rate-limit gate, pretool rules, stop rules) with no backend-specific branching, and hands it to
every instance declaring `host_hooks` (`gw/agents_delegated.go:configureDelegated`, #2154 Phase 3).
A backend uses the fields its capabilities say it honours. The Claude Code re-login is chosen by
`relogin`; elsewhere an auth failure is logged. codex publishes its model catalogue to modelcaps
itself. The only backend-package imports left in the gateway are the ccstream auto-approve rule
lists and ledger-shadow helpers (#2154 Q8, Phase 4).

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

Foci-level permission auto-approval (applies to every delegated backend, before the user is prompted):

```toml
[permissions]
auto_approve_common_readonly = true
auto_approve_common_safe_write = false
allow = ["Bash(git status)", "Bash(git diff*)"]
```

## Further reading

- [WIRING.md — The Agent Loop](WIRING.md#the-agent-loop-agentagentgo) — `TurnContract`, `OrchestrateFullTurn`, phase-by-phase breakdown.
- [WIRING.md — ccstream Backend](WIRING.md#ccstream-backend-internaldelegatorccstream) — stream-json protocol, hook integration, permission handling.
- [CONFIG.md — Coding Agent Backends](CONFIG.md#coding-agent-backends) — all config keys.
- [SPEC.md — Coding Agent Backends (TurnContract)](SPEC.md) — design intent.

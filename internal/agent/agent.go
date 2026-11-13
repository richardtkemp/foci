package agent

import (
	"context"
	"fmt"
	"foci/internal/usagehold"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/modelinfo"
	"foci/internal/nudge"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/telemetry"
	"foci/internal/tools"
	"foci/internal/tools/shell"
	"foci/internal/turnevent"
	"foci/internal/warnings"
	"foci/internal/workspace"
	"foci/shared/prompts"
)

var (
	convertLog            = log.NewComponentLogger("convert")
	delegatedLog          = log.NewComponentLogger("delegated")
	message_transformsLog = log.NewComponentLogger("message_transforms")
)

// NoResponseSentinel is the marker that prompts instruct the model to emit
// when it has nothing to say. The agent strips it before delivery so the
// user never sees it (and the platform treats it as an empty response).
const NoResponseSentinel = "[[NO_RESPONSE]]"

// The nudge framing text — preamble, reply instruction, and user boundary —
// wraps every injected nudge. Each is a per-agent-overridable prompt resolved
// through the standard prompts.ResolvePrompt path (agent workspace/prompts →
// shared/prompts → embedded default), the SAME mechanism as the
// compaction-summary prompt. Embedded defaults live in shared/prompts/nudge-*.md
// and are seeded as editable copies (seedDefaultPrompts); drop a file of the
// same name in an agent's prompts dir to override it for that agent. The
// resolved values are stored trimmed — the wrappers below reintroduce the exact
// separators, so an override file need not preserve leading/trailing whitespace.

// nudgePreamble resolves the background-nudge preamble that frames an injected
// nudge as a private note to the agent rather than user input.
func (a *Agent) nudgePreamble() string {
	return prompts.ResolvePrompt("", "nudge-preamble.md", prompts.NudgePreamble(), a.PromptSearchDirs...)
}

// nudgeReplyInstruction resolves the silence-vs-reply guidance appended to
// standalone nudges. If the nudge calls for action or the agent has other
// pending work it should respond; otherwise it emits NoResponseSentinel so the
// platform delivers nothing.
func (a *Agent) nudgeReplyInstruction() string {
	return prompts.ResolvePrompt("", "nudge-reply-instruction.md", prompts.NudgeReplyInstruction(), a.PromptSearchDirs...)
}

// nudgeUserBoundary resolves the delimiter that closes the nudge region when
// one or more nudges are bundled with a real user message in the same turn. It
// lets the agent see exactly where the background nudge ends and the user's
// text begins. Emitted once, after the last nudge, at the bundling join point —
// never per-nudge, and never on the standalone mid-loop paths (after-tools,
// pre-answer) where there is no following user text.
func (a *Agent) nudgeUserBoundary() string {
	return prompts.ResolvePrompt("", "nudge-user-boundary.md", prompts.NudgeUserBoundary(), a.PromptSearchDirs...)
}

// wrapStandaloneNudge composes a full nudge message: preamble + reminder body +
// reply instruction. Used by the standalone mid-loop delivery paths
// (after-tools, pre-answer) where the nudge is its own injected user message and
// the NO_RESPONSE guidance is the correct silence-vs-reply steer.
func (a *Agent) wrapStandaloneNudge(reminder string) string {
	return a.nudgePreamble() + "\n" + reminder + "\n\n" + a.nudgeReplyInstruction()
}

// wrapBundledNudge wraps a nudge that is prepended to a real user message
// in the same turn (the start-of-turn interval/regex injection path). It
// omits the reply instruction because a reply to the user is always required on
// that path — the [[NO_RESPONSE]] guidance would contradict the user's message.
// The closing nudgeUserBoundary is emitted separately at the join point so a
// single delimiter appears after all bundled nudges, not one per nudge.
func (a *Agent) wrapBundledNudge(reminder string) string {
	return a.nudgePreamble() + "\n" + reminder
}

// CacheBustFunc is called when a cache bust is detected (cache_read drops
// significantly compared to the previous request).
// session is the session key, prevRead is what we had, curRead is what we got.
type CacheBustFunc func(session string, prevRead, curRead int)

// Agent is the core agent loop.
type Agent struct {
	Client          provider.Client
	ClientProvider  provider.ClientProvider // provides access to API clients for different endpoint:format pairs
	Sessions        *session.Store
	Tools           *tools.Registry
	Bootstrap       *workspace.Bootstrap
	Compactor       *compaction.Compactor   // nil disables auto-compaction
	AsyncNotifier   *tools.AsyncNotifier    // nil disables async-pending compaction guard
	AskRouter       *tools.AskRouter        // nil disables typed-answer routing for the foci-native `ask` tool
	SpawnRestore    func()                  // resumes the spawns a restart interrupted (#2589, Spawner.RestoreSpawns); nil = the agent has no spawn tool
	DepthRestore    func()                  // rebuilds the depth-mode trees after a restart (#2588, DepthManager.Restore); nil = depth mode off
	DepthOnReset    func(sessionKey string) // a reset of a depth-mode tree root drops its whole tree (#2588); nil = depth mode off
	Reminders       *memory.ReminderStore   // nil disables reminder injection
	TaskListStore   *memory.TaskListStore   // nil disables task list in state dashboard
	TodoStore       *memory.TodoStore       // nil disables todo count in state dashboard
	ScratchpadStore *memory.Scratchpad      // nil disables scratchpad count in state dashboard
	AgentID         string                  // unique agent identifier (for per-agent DB queries)
	Model           string                  // "developer/model_id" format (e.g. "anthropic/claude-opus-4-6")
	Format          string                  // wire format resolved at startup (e.g. "anthropic", "gemini", "openai")
	Endpoint        string                  // agent's default endpoint (sessions inherit this unless overridden)
	Log             *log.ComponentLogger    // structured logger for this agent

	EnvironmentBlock              string                                  // static environment block (fallback; tests). Prod uses EnvironmentBlockFunc.
	EnvironmentBlockFunc          EnvBlockFunc                            // per-session environment block (platform-aware); preferred over EnvironmentBlock
	DailyDigestFunc               func(sessionKey string) string          // per-session daily-memory digest (#2827), appended last; nil or "" = none
	ExtraSystemBlocks             []provider.SystemBlock                  // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy                 string                                  // "auto" (top-level) or "explicit" (manual breakpoints) — from primary model config
	CacheBustDetect               bool                                    // detect cache busts (cache_read drop >50%)
	CacheBustAlert                HookList[CacheBustFunc]                 // callbacks for cache bust alerts
	DuplicateMessages             bool                                    // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages bool                                    // accumulate mid-turn text; send concatenated on turn end (default false = send immediately)
	BatchPartialJoiner            string                                  // separator between batched partial messages (default "")
	MaxResultChars                int                                     // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir             string                                  // where to write large tool results
	UserShell                     *shell.UserRunner                       // runs chat "!" commands (#2812); nil refuses them
	UserCommandOperators          func(platformID string) []string        // a platform's resolved operator_users, for "!" commands
	ResultSummariser              tools.Summariser                        // summarises an oversized result on a delegated agent (no API client); nil on API agents
	SummaryContextTurns           int                                     // recent conversation turns for summary context
	SummaryContextChars           int                                     // max chars of context to send to cheap model
	MaxSummaryChars               int                                     // max chars to auto-summarise (skip cheap model above this)
	MaxSummaryInputChars          int                                     // max chars of tool result embedded in summary prompt (0 = no limit)
	GroupResolver                 *config.GroupResolver                   // resolves call sites to model groups
	FallbackFunc                  provider.FallbackFunc                   // nil disables automatic model fallback on transient errors
	MaxImagePixels                int                                     // max pixels (w*h) for images before downscaling; 0 disables
	AutoSummarise                 bool                                    // enable auto-summarise of oversized tool results (default true)
	WarningQueue                  *warnings.Queue                         // nil disables warning injection into session
	ChatWarningQueue              *warnings.Queue                         // nil disables chat warning notifications
	MaxTokensWarnFunc             HookList[func(string)]                  // callbacks when stop_reason=max_tokens (response truncated)
	RateLimitFunc                 HookList[func(resetTime time.Time)]     // callbacks when API returns 429 (rate limited)
	ReloadOnCompact               bool                                    // delegated: after compaction, bounce the CC session (resume) so character/skill files reload from disk (#828)
	TaskListNotifyFunc            HookList[func(string, string)]          // callbacks for task list changes (session key, message)
	CompactionMemoryFunc          HookList[func(string)]                  // fires before compaction to save memories (session key)
	CompactionStartFunc           HookList[func(string, string)]          // callbacks for compaction start (session key, message) — sent immediately, not buffered
	CompactionNotifyFunc          HookList[func(string, string, string)]  // callbacks for compaction notifications (session key, message, summary — summary is "" when compaction produced no summary, e.g. delegated-backend compactions)
	CompactionDebugFunc           HookList[func(string, string)]          // callbacks for compaction debug (session key, summary text)
	OnActivity                    HookList[func(string)]                  // callbacks when a session has activity (session key)
	Redact                        func(string) string                     // redact secrets from tool output; nil disables
	SessionIndex                  *session.SessionIndex                   // nil disables state persistence
	MessageTransforms             []CompiledTransform                     // compiled regex rules for inbound message transformation
	CompactionHandoffMsg          string                                  // inline handoff message; empty resolves from search dirs or embedded default
	PromptSearchDirs              []string                                // directories to search for prompt files (agent workspace, shared)
	MaxToolLoops                  int                                     // max tool iterations per turn (default config.DefaultMaxToolLoops)
	MaxOutputTokens               int                                     // max tokens in model response (default 16384)
	Nudger                        *nudge.Scheduler                        // nil disables nudge reminders; pre-answer gate/min-tools live in its settings
	NudgeReloadFunc               func()                                  // called after bootstrap reload to refresh nudge rules; nil disables
	FirstRunMessage               atomic.Value                            // string; prepended as separate content block on first HandleMessage, then cleared
	OnFirstRunConsumed            func()                                  // fired once when FirstRunMessage is actually consumed into a delivered turn (nil disables); marks onboarding complete
	FirstRunAddendum              func() string                           // called once, on the consume that wins FirstRunMessage; a non-empty result is appended to the onboarding text (the install's one-time import, #3049). nil disables
	TurnLockWarnThreshold         time.Duration                           // warn if turn lock wait exceeds this (default 3m)
	ShowToolCalls                 string                                  // agent-level default: "off"/"preview"/"full" (per-session overrides via /display)
	Statusline                    string                                  // per-agent [meta]/[state] header template; "" = DefaultStatuslineTemplate (#831)
	Streaming                     bool                                    // use streaming API when provider supports it
	ModelMetaFn                   func(model string) modelinfo.ModelMeta  // per-model meta from config (context window)
	ModelDefaultsFn               func(model string) config.ModelDefaults // returns per-model defaults from [models.*] config; nil = no model defaults
	CanRunBackground              string                                  // path to an executable gating background work; exit 0 = allowed, non-zero = skip; "" = always allowed
	LiveConfigFn                  func() *config.ResolvedAgentConfig      // when set, per-turn getters read the LIVE resolved config so edits apply without restart; nil in direct-constructed agents (tests) → static fields above
	ServerTools                   []provider.ToolDef                      // server-side tools (web_search, web_fetch) — executed by Anthropic, not client
	DelegatedManager              *DelegatedManager                       // nil = traditional agent loop; non-nil = lazy per-session delegated transport management
	Spawns                        *tools.SpawnTracker                     // background spawns listed by the {spawns} statusline field (#2127); nil = tools.Spawns
	Backend                       string                                  // configured transport backend name; empty/api = traditional API loop
	ReloginTrigger                func(reason, sessionKey string) bool    // nil unless an ccstream backend is wired; starts the #843 re-login flow, returns false if one is already in flight. sessionKey (may be "") targets the chat that gets the login URL; "" falls back to the agent's default chat.
	Reflection                    config.ResolvedReflection               // resolved reflection config (agent+global merged)
	Keepalive                     config.ResolvedKeepalive                // resolved keepalive config (agent+global merged); static-cfg fallback, see keepalive()
	Background                    config.ResolvedBackground               // resolved background config (agent+global merged); static-cfg fallback, see background()
	Maintenance                   config.ResolvedMaintenance              // resolved maintenance config (agent+global merged); static-cfg fallback, see maintenance()
	Workspace                     string                                  // agent workspace dir; a relative read path in a tool call resolves against it (SkillScope, #2392)
	SkillDirs                     *skills.LiveDirs                        // skill directories (shared, extra, per-agent) for reflection creation/update detection; nil-safe, swapped live (#2657)
	SkillChangeNotify             func(string, string, string)            // called with (sessionKey, skillName, markdown) when a GIT-REPO skill change is attributed to a commit that landed during the window (see skills.AttributeToGit); nil = disabled
	SkillChangeNotifyText         func(string, string)                    // called with (sessionKey, formatted message) for a NON-git-repo skill change (skills.FormatChanges) — preserves the pre-#1404 plain-text behaviour there exactly; nil = disabled
	DefaultPlatform               string                                  // configured default_platform (per-agent, else global); preferred for default-session resolution and delivery fallback
	ResetOrientTemplateFn         func() string                           // resolves orientation template for session reset; nil = no orientation
	ReloadSystemFn                func() ([]provider.SystemBlock, int)    // reloads skills/extra blocks; returns new blocks + count; nil = no-op
	TurnLimit                     *TurnLimiter                            // per-agent cap on concurrent user turns across all its sessions; nil = unlimited (#2281)
	GlobalTurnLimit               *TurnLimiter                            // gateway-wide cap shared by every agent (one pointer, built once in main); nil = unlimited
	UsageHoldRegistry             *usagehold.Registry                     // account-wide usage hold (#2491); nil = usagehold.Default
	UsageHoldNotifyFunc           func(text string)                       // posts a usage-hold start/end notice in the agent's default chat (#2491); nil = none
	UsageHoldChangeFunc           []func(usagehold.Change)                // further reactions to a usage-hold phase change (#2491)
	usageHold                     usageHoldState

	// ReloginLinkTarget is set with ReloginTrigger: where a sign-in link
	// repeated in one of this agent's sessions goes (#3011). nil: the agent
	// cannot carry the link, and its messages are only held.
	ReloginLinkTarget func(sessionKey string) *relogin.LinkTarget

	// ReloginRetry is set with ReloginTrigger: after a user-typed turn in
	// sessionKey met an auth failure while a login was pending, it starts one
	// new attempt whose link goes to that chat (#3015), through
	// relogin.G.StartAfterUserFailure(attempt). Reports whether it started.
	ReloginRetry func(sessionKey string, attempt uint64) bool

	// Restart restore of held input (#2491, usage_hold_store.go).
	// UsageHeldDriver drives a restored held message; UsageHeldInjectRestore
	// rebuilds a restored held injection. Nil = that input waits for a later
	// start.
	UsageHeldDriver        func(sessionKey string) Driver
	UsageHeldInjectRestore func(sessionKey string, d DurableInject, id string)

	// RestartResumeNotice posts a turn-free notice line in sessionKey's chat
	// when a restart resumes (or gives up resuming) the turn it cut (#2999,
	// restart_resume.go). Nil = no notice.
	RestartResumeNotice func(sessionKey, text string)
	cutTurn             cutTurnState
	queuedInputMu       sync.Mutex // serialises read-modify-write of the queued_input lists (#2999)

	// PromptAnswerRefusedNotifyFunc tells sessionKey's chat that a typed answer
	// did not reach the backend prompt waiting for it (#2762). Fired in its own
	// goroutine. Nil = no notice.
	PromptAnswerRefusedNotifyFunc func(sessionKey, text string)

	// OnInputDelivery reports how platform messages (refs) reached the agent
	// (#2992): one of the Delivery* outcomes, the ledger id of the turn they
	// started, joined or were steered into ("" while queued), and when. Called
	// synchronously from the inbox and turn transport; must not block. nil =
	// not reported.
	OnInputDelivery func(sessionKey string, refs []delegator.InputRef, outcome, turnID string, at time.Time)

	// liveTurns maps a session key to the ledger id of the turn running on it
	// (liveturn.go).
	liveTurns sync.Map

	TurnQueuedNotifyFunc func(sessionKey, text string) // fired (in its own goroutine) when a user turn must wait for a turn-limit slot; nil = no queued notice

	platforms  map[string]platform.Sender // per-agent platforms (telegram, discord, etc.); key = platform name
	platformMu sync.RWMutex               // protects platforms map access

	rateLimitGates     map[string]*RateLimitGate // per-endpoint gates; key = endpoint name, lazy-init
	rateLimitGatesMu   sync.RWMutex              // protects rateLimitGates map access
	inFlightMu         sync.Mutex                // protects inFlight + inFlightDelivering + inFlightChanged + lastTurnEndAt map access
	inFlight           map[string]int32          // per-session count of in-flight OrchestrateFullTurn calls — see IsTurnInFlight
	inFlightDelivering map[string]int32          // per-session-base count of in-flight turns whose sink reports DeliversToPlatform=true — see IsInFlightDelivering
	inFlightChanged    map[string]chan struct{}  // per-session-base close-and-replace channel; closes on each state change so waiters wake — see InFlightWaitCh
	lastTurnEndAt      map[string]time.Time      // per-session-base wall-clock moment the last turn ended (inFlight→0) — see LastTurnEnd; in-memory turn-END activity signal; last_cache_touch is the durable warmth signal (model-response evidence, #2509)
	unattendedTurns    map[string]int32          // per-session count of in-flight turns that are unattended (turnUnattended) — see TurnUnattended; under inFlightMu
	rateLimitRetry     map[string]time.Time      // per-session end of the rate-limit (429) retry sleep its backend reported last, cleared by progress or the turn's end — see InRateLimitRetry (#3044); under inFlightMu
	injectGatePoll     time.Duration             // test seam: the inject gate's poll backstop; zero = injectGatePollInterval (#2537)
	pendingHoldMax     time.Duration             // test seam: how long background work may hold a system inject; zero = maxPendingHold (#2792)
	askHoldMax         time.Duration             // test seam: how long an open foci_ask may hold a system inject; zero = maxAskHold (#2828)
	turnDetailsMu      sync.Mutex
	turnDetails        map[uint64]*TurnDetail // keyed by unique turn ID
	turnIDCounter      uint64                 // atomic: monotonic turn ID
	turnLocksMu        sync.Mutex
	shutdown           shutdownGate           // drain latch — see BeginShutdown (#2059)
	turnLocks          map[string]*sync.Mutex // per-session turn serialization
	metaMu             sync.Mutex
	meta               map[string]*sessionMeta // per-session metadata

	// branchRootWarms holds, per inheriting branch, the root stamp its first
	// evidenced model response will make (#2509, cache_evidence.go).
	branchRootMu    sync.Mutex
	branchRootWarms map[string]branchRootWarm
	// keepaliveStopped holds the sessions a fork of which missed their cache
	// since their last own call: keepalive skips them (#3089). In memory: after
	// a restart the next keepalive fork misses once and sets it again.
	keepaliveStopped map[string]bool

	// compacting latches compaction-in-flight per session so /status reports
	// "compacting" rather than "idle" while a summary turn runs. Keyed by
	// session key → time.Time deadline for self-heal. Set/cleared around the
	// compaction brackets in compaction.go; read via IsCompacting (#725).
	compacting sync.Map

	// Inbox subsystem (Phase 6 — TODO #739): per-session message queue
	// + worker. Bot calls Enqueue(envelope) after filtering; agent owns
	// queueing, batching, steer dispatch, and turn execution via the
	// platform-supplied Driver. See inbox.go.
	inboxes               map[string]*sessionInbox                                          // per-session inbox registry, lazy-created
	inboxesMu             sync.Mutex                                                        // protects inboxes map + StartInbox idempotency
	inboxWorkers          atomic.Int64                                                      // running session workers (#2283 eviction; diagnostics and tests)
	inboxEvictions        atomic.Int64                                                      // idle inboxes evicted since start (#2283; diagnostics and tests)
	inboxIdleAfter        time.Duration                                                     // idle-eviction threshold override (tests); 0 = inboxIdleAfter (3h)
	inboxReapEvery        time.Duration                                                     // idle-reaper interval override (tests); 0 = inboxReapEvery (10m)
	enqueueFoundHook      func(sk string)                                                   // test seam: runs after Enqueue finds the inbox, before it pins it
	turnLockFetchedHook   func(sk string)                                                   // test seam: runs after lockTurn fetches the mutex, before it locks it
	inboxStarted          bool                                                              // true once StartInbox has been called
	reloginDrainOnce      sync.Once                                                         // registers DrainReloginDeferred with relogin.G once, on the first paused injection (#2384)
	inboxCtx              context.Context                                                   //nolint:containedctx // parent ctx for session worker goroutines
	inboxCancel           context.CancelFunc                                                // ends inboxCtx (StopInbox, #2787)
	inboxSteerMode        bool                                                              // urgent-steer dispatch enabled
	inboxBackend          func(ctx context.Context, sk string) (delegator.Delegator, error) // test seam; nil = use DelegatedManager
	turnObserver          func(sk string, batch []Envelope)                                 // test seam — see SetTurnObserver / TODO #746 Stage C
	turnSlotGrantObserver func()                                                            // test seam — see SetTurnSlotGrantObserver (the #2281 grant-vs-/stop settle window)
	enqueueObserver       func(Envelope)                                                    // test seam — see SetEnqueueObserver (observes every envelope entering the inbox)

	// Session-lifecycle hooks fired at the turn boundary in HandleMessage, so
	// EVERY backend turn (platform-driven AND system-injected) fires them —
	// unlike the platform Driver's WrapTurn, which injections bypass. Wired by
	// the gateway to the keepalive Runner: onTurnEnd → NotifyTurnEnd (flush
	// warnings deferred during the turn). onTurnComplete is currently unused
	// (cache warmth is tracked via session_index.last_cache_touch) but kept as a
	// generic hook. Any may be nil.
	onTurnComplete func()
	onTurnEnd      func()

	// onCacheExpiry, when set, is fired whenever a session's last_cache_touch
	// changes (turn entry, branch-warms-root, reset-clears). It carries the
	// truthful prompt-cache expiry (unix ms; 0 = cold) so the app client's
	// warmth indicator refreshes on ANY cache (re)warm — not only turns that
	// complete through a live app sink. Wired by the gateway to app.SetCacheExpiry.
	onCacheExpiry func(sessionKey string, expiryMs int64)
	// onPromptSuggestion, when set, receives the backend's prediction of the
	// user's next prompt (#2916). Wired by the gateway to
	// app.DeliverPromptSuggestion.
	onPromptSuggestion func(sessionKey, text string) bool
	// onPromptSuggestionStale, when set, hears that the user's input reached a
	// session's backend, so its shown prediction is out of date (#2924). Wired by
	// the gateway to app.ClearPromptSuggestion.
	onPromptSuggestionStale func(sessionKey string)
	// onSubagentActivity, when set, hears each step of a session's subagents:
	// kind "text" (a text block), "tool" (a tool call: text is the tool name
	// and its input) or "end" (the run ended, text empty). The app's Stop view
	// summarises the latest steps (#2960). Wired by the gateway to
	// app.NoteSubagentActivity.
	onSubagentActivity func(sessionKey, groupKey, kind, text string)
	// promptSuggestionSurface reports whether a session has anywhere to show a
	// prediction (#2922). Wired to app.CanShowPromptSuggestion; nil means no.
	promptSuggestionSurface func(sessionKey string) bool
	// silentTurnEnded holds the sessions whose last turn gave no reply, whose
	// prediction must not replace the chip (#2922).
	silentTurnEnded sync.Map // sessionKey → struct{}

	// onAliasChanged, when set, is fired after a backend-generated thread name
	// (e.g. Codex) is persisted as a chat's alias, so the app can show the new
	// name without reconnecting (#2157). Wired by the gateway to
	// app.NotifyAliasChanged.
	onAliasChanged func(agentID, platform string, chatID int64)

	// Per-session delivery routers (#1068 Phase 1). Built ONCE per session key,
	// shared by every turn on that session (platform turns register their
	// streaming sink; system turns register NopSink/BufferSink post-accept; an
	// autonomous run — no registration — falls through to the router's
	// late-delivery fallback). SessionEvents binds to this router once, so a
	// system turn can never rebind the session to NopSink (the #1068 poison).
	routers   map[string]*sessionRouter
	routersMu sync.Mutex
	// sessionEvents caches the session-scoped delivery callbacks (SessionEvents)
	// per session key, bound ONCE around that session's router. Attached to the
	// backend at acquisition (setBackendCallbacks → AttachDelivery), not rebuilt
	// per turn — that per-turn rebuild from the ctx sink was the #1068 poison.
	// Guarded by routersMu (same per-session lock as routers).
	sessionEvents map[string]*delegator.SessionEvents
	// thinkingBufs accumulates a session's streamed thinking deltas between
	// turns. Session-scoped because SessionEvents (which writes to it) now lives
	// for the backend's lifetime; DrainThinking reads-and-resets it at each turn's
	// completion to log that turn's thinking. Guarded by routersMu.
	thinkingBufs map[string]*strings.Builder
	// toolTallies counts each session's tool calls for the current delegated
	// turn, fed by the same SessionEvents (OnToolStart/OnToolEnd) and reset per
	// turn by buildTurnEvents (#2193). Guarded by routersMu.
	toolTallies map[string]*toolTally
	// bgOrigins records, per session key, the tool calls made by non-delivered
	// turns, keyed by tool_use id (= the group key of any background task the
	// call became), so the task's later output and CC's reaction to its result
	// stay out of the chat (#2093, bg_origin.go). Guarded by bgOriginsMu.
	bgOrigins   map[string]map[string]*bgOrigin
	bgOriginsMu sync.Mutex
	// ResolveLateConn resolves the current delivering connection for a session
	// key, for the router's late-delivery fallback. Called at Emit time so it
	// tracks connect/disconnect (a session that reconnects starts delivering
	// again with no router rebuild). Wired from cmd/foci-gw via route.ConnFor;
	// nil (tests / no connection manager) → the fallback discards-and-warns.
	ResolveLateConn func(sk string) platform.Connection
	// DurableTurnSink resolves a sink that persists a turn's events durably
	// against the session's platform binding (e.g. the app's conv_id via
	// chat_metadata) WITHOUT requiring a live connection right now — mirrors
	// internal/app/hub.go's convBinding.send, which appends every frame to its
	// durable buffer/store unconditionally, before it even looks at which
	// clients are currently connected, so a client that reconnects later
	// replays what it missed. autonomousTurnSink consults this as the
	// fallback BEFORE NopSink when ResolveLateConn's normal, ownership-
	// respecting cascade (route.ConnFor) resolves nothing at all — for the
	// app platform this is narrower than "no live socket" (a live socket
	// isn't required for the app's own delivery path either — see
	// app.DurableConnFor's doc comment for why); it fires on genuine
	// resolution failures such as the session's agent having no app
	// registration, not routine reconnects (clutch #1350 follow-up:
	// previously any such adoption discarded the turn's entire output with
	// zero durable record, unlike every other delivery path in the app).
	// Returns nil if the session has no platform binding to persist against
	// at all (e.g. a pure-API session with no chat identity). Nil (tests /
	// not wired) → autonomousTurnSink falls all the way to NopSink, as today.
	DurableTurnSink func(sk string) turnevent.Sink
	// SpawnBranchTurn claims an autonomous run on a background spawn's branch
	// (#2729): ok means sk is one, and the run's events go to sink, which
	// reports it to the spawn's spawner and never to a chat. stop interrupts
	// the run. Wired from cmd/foci-gw to tools.SpawnBranchTurn; nil (tests)
	// → no session is a spawn's.
	SpawnBranchTurn func(sk string, stop func()) (sink turnevent.Sink, ok bool)
}

func (a *Agent) streaming() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.Streaming
	}
	return a.Streaming
}

// reflection returns the session's live reflection config (agent+global merged),
// re-read from the resolved snapshot so [reflection] edits apply without a
// restart. Falls back to the startup-resolved a.Reflection in direct-constructed
// agents (tests) where LiveConfigFn is nil.
func (a *Agent) reflection() config.ResolvedReflection {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Reflection
	}
	return a.Reflection
}

// keepalive returns the live keepalive config (agent+global merged), re-read
// from the resolved snapshot so [keepalive] edits apply without a restart.
// Falls back to the startup-resolved a.Keepalive in direct-constructed agents
// (tests) where LiveConfigFn is nil.
func (a *Agent) keepalive() config.ResolvedKeepalive {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Keepalive
	}
	return a.Keepalive
}

// backgroundConfig returns the live background-work config (agent+global
// merged), re-read from the resolved snapshot so [background] edits apply
// without a restart. Falls back to the startup-resolved a.Background in
// direct-constructed agents (tests) where LiveConfigFn is nil. Named
// backgroundConfig (not background) to avoid clashing with any future
// receiver named after the section; canRunBackground already reads a single
// field the same way.
func (a *Agent) backgroundConfig() config.ResolvedBackground {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Background
	}
	return a.Background
}

// maintenance returns the live maintenance config (agent+global merged),
// re-read from the resolved snapshot so [maintenance] edits apply without a
// restart. Falls back to the startup-resolved a.Maintenance in
// direct-constructed agents (tests) where LiveConfigFn is nil.
func (a *Agent) maintenance() config.ResolvedMaintenance {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Maintenance
	}
	return a.Maintenance
}

func (a *Agent) maxToolLoops() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.MaxToolLoops
	}
	return a.MaxToolLoops
}

func (a *Agent) cacheBustDetect() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Debug.CacheBustDetect
	}
	return a.CacheBustDetect
}

func (a *Agent) cacheBustIdleThreshold(sessionKey string) time.Duration {
	return time.Duration(float64(a.cacheTTLFor(sessionKey)) * 0.95)
}

func (a *Agent) canRunBackground() string {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Background.CanRunBackground
	}
	return a.CanRunBackground
}

func (a *Agent) maxResultChars() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.MaxResultChars
	}
	return a.MaxResultChars
}

func (a *Agent) maxSummaryInputChars() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.MaxSummaryInputChars
	}
	return a.MaxSummaryInputChars
}

func (a *Agent) steerMode() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Behavior.SteerMode
	}
	return a.inboxSteerMode
}

func (a *Agent) maxOutputTokens() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.MaxOutputTokens
	}
	return a.MaxOutputTokens
}

func (a *Agent) duplicateMessages() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.DuplicateMessages
	}
	return a.DuplicateMessages
}

func (a *Agent) batchPartialAssistantMessages() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.BatchPartialAssistantMessages
	}
	return a.BatchPartialAssistantMessages
}

func (a *Agent) batchPartialJoiner() string {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Loop.BatchPartialJoiner
	}
	return a.BatchPartialJoiner
}

func (a *Agent) autoSummarise() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.AutoSummarise
	}
	return a.AutoSummarise
}

func (a *Agent) maxSummaryChars() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.MaxSummaryChars
	}
	return a.MaxSummaryChars
}

func (a *Agent) summaryContextTurns() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.SummaryContextTurns
	}
	return a.SummaryContextTurns
}

func (a *Agent) summaryContextChars() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.SummaryContextChars
	}
	return a.SummaryContextChars
}

func (a *Agent) maxImagePixels() int {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Summary.MaxImagePixels
	}
	return a.MaxImagePixels
}

// statusline is the header template for a turn arriving via platform; see
// config.ResolvedAgentConfig.StatuslineFor for the precedence.
func (a *Agent) statusline(platform string) string {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().StatuslineFor(platform)
	}
	return a.Statusline
}

func (a *Agent) showToolCalls() string {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Display.ShowToolCalls
	}
	return a.ShowToolCalls
}

// compactionSummaryPrompt resolves the compaction summary prompt per-agent via
// the standard file-override path (agent workspace/prompts → shared/prompts →
// embedded default), the SAME mechanism as the nudge framing prompts. Override
// by dropping a compaction-summary.md in the agent's prompts dir; an empty
// override file resolves to "" and disables the delegated /compact prompt.
func (a *Agent) compactionSummaryPrompt() string {
	return prompts.ResolvePrompt("", "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
}

func (a *Agent) compactionHandoffMsg() string {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Compaction.CompactionHandoffMsg
	}
	return a.CompactionHandoffMsg
}

func (a *Agent) reloadOnCompact() bool {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Compaction.ReloadOnCompact
	}
	return a.ReloadOnCompact
}

// turnLockWarnThreshold reads the live resolved string (e.g. "3m") and parses
// it per call; the baked a.TurnLockWarnThreshold is the pre-parsed fallback for
// direct-constructed agents (nil LiveConfigFn) and for an unparseable value.
func (a *Agent) turnLockWarnThreshold() time.Duration {
	if a.LiveConfigFn != nil {
		if d, err := time.ParseDuration(a.LiveConfigFn().Behavior.TurnLockWarnThreshold); err == nil {
			return d
		}
	}
	return a.TurnLockWarnThreshold
}

// TransformMessage applies compiled message transforms to the text.
// Returns the original text unchanged if no transforms are configured.
func (a *Agent) TransformMessage(text string) string {
	if len(a.MessageTransforms) == 0 {
		return text
	}
	return ApplyTransforms(a.MessageTransforms, text)
}

func (a *Agent) Warnings() *warnings.Queue {
	return a.WarningQueue
}

func (a *Agent) ChatWarnings() *warnings.Queue {
	return a.ChatWarningQueue
}

// defaultAgentLog is returned by logger() when a.Log is nil. Production
// always sets Log via the constructor in cmd/foci-gw/agents_shared.go;
// this fallback exists for tests that bare-literal &Agent{} without
// wiring logging. Read-only after package init — no race possible.
var defaultAgentLog = log.NewComponentLogger("agent")

// logger returns a.Log, or a package-level default if nil. Pure read —
// no side effects, no race. Use this everywhere instead of touching
// a.Log directly so the nil case is always handled centrally.
func (a *Agent) logger() *log.ComponentLogger {
	if a.Log != nil {
		return a.Log
	}
	return defaultAgentLog
}

// taggedLog returns a component logger scoped to this agent under a sub-tag,
// producing a component of the form "<tag>:<agentID>" (e.g. "reset:clutch").
// Use it for log lines tied to a specific agent that carry a distinct sub-tag;
// use logger() when the component is exactly "agent".
func (a *Agent) taggedLog(tag string) *log.ComponentLogger {
	return log.NewComponentLogger(tag + ":" + a.AgentID)
}

// ResolveCallSite resolves a call site to a (client, model, format) triple.
// Ungrouped call sites (and a nil GroupResolver) get the session's whole
// effective tuple — ONE SessionModelTuple snapshot, so the session's own
// format always accompanies its own client, never the agent default format.
// Grouped call sites resolve via GroupResolver and fetch the client from
// ClientProvider for the group's endpoint/format.
func (a *Agent) ResolveCallSite(callSite, sessionKey string) (provider.Client, string, string) {
	if a.GroupResolver != nil {
		if resolved := a.GroupResolver.ResolveCall(callSite); resolved != nil {
			client := a.SessionClient(sessionKey)
			if a.ClientProvider != nil {
				if c := a.ClientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
					client = c
				}
			}
			return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
		}
	}
	tuple := a.SessionModelTuple(sessionKey)
	return tuple.Client, tuple.Model, tuple.Format
}

// TurnDetail describes one in-flight turn for shutdown diagnostics.
type TurnDetail struct {
	SessionKey string
	Trigger    string // "user", "keepalive", "branch", "scheduled_wake", "telegram", "async_notify"
	ToolName   string // currently executing tool, or empty
	StartTime  time.Time
	// DispatchedAt is when the turn actually began on its backend — later than
	// StartTime when it first waited behind an in-flight turn. Zero while it
	// is still waiting. Set via markTurnDispatched.
	DispatchedAt time.Time
}

// ProcessingDetails returns detail for every in-flight turn.
func (a *Agent) ProcessingDetails() []TurnDetail {
	a.turnDetailsMu.Lock()
	defer a.turnDetailsMu.Unlock()
	out := make([]TurnDetail, 0, len(a.turnDetails))
	for _, d := range a.turnDetails {
		out = append(out, *d)
	}
	return out
}

// turnLock returns the map's per-session mutex that serializes HandleMessage
// calls. This prevents concurrent turns on the same session from interleaving
// messages in the session file, which would invalidate Anthropic's
// prefix-matched prompt cache. The entry can be removed while nobody holds it
// (idle eviction, ClearSessionState: dropTurnLock), so never lock its result
// directly: lockTurn locks it and retries when it was removed meanwhile.
func (a *Agent) turnLock(sessionKey string) *sync.Mutex {
	a.turnLocksMu.Lock()
	defer a.turnLocksMu.Unlock()
	if a.turnLocks == nil {
		a.turnLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := a.turnLocks[sessionKey]
	if !ok {
		mu = &sync.Mutex{}
		a.turnLocks[sessionKey] = mu
	}
	return mu
}

// HandleMessage processes one or more user messages with optional attachments
// (images, PDFs, or convertible documents like docx/xlsx/pptx/HTML/CSV). It
// routes to either the API tool-loop path (APITransport) or the delegated
// transport path (DelegatedTransport) via the TurnContract interface.
//
// Text delivery (intermediate and final), thinking, tool call visibility,
// retries, and typing-indicator lifecycle flow through the turnevent.Sink
// attached to ctx (see internal/turnevent). Callers that want the final
// text wire a BufferSink; callers that want streaming UI wire a StreamingSink.
//
// TurnStart fires at entry; TurnComplete fires via defer, carrying the
// accumulated FinalText, Usage, Cost, Model, and any error returned by the
// turn orchestrator — except for a turn whose input joined a turn already in
// flight (TurnState.foldedFollowUp) through the session router, which
// completes only its trace: the turn it joined owns the router's sink and
// completes it (#2366).
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (err error) {
	if err := a.validateSessionOwnership(sessionKey); err != nil {
		return err
	}

	// Tracing rides the same event stream every transport already emits:
	// wrapping the sink here (and in OpenAutonomousTurn, the one other turn
	// entry) is what makes every turn — platform, injected, cron, memory —
	// a trace without per-backend hooks. No-op wrap when tracing is off.
	sink, tspan := telemetry.NewTurnSink(turnevent.SinkFromContext(ctx))
	ctx = telemetry.WithTurn(turnevent.WithSink(ctx, sink), tspan)
	sink.Emit(ctx, turnevent.TurnStart{})

	var tc TurnContract
	if a.DelegatedManager != nil {
		tc = &DelegatedTransport{sharedTurnOps{agent: a}}
	} else {
		tc = &APITransport{sharedTurnOps{agent: a}}
	}
	ts := NewTurnState(ctx, sessionKey, texts, attachments)

	defer func() {
		// Diagnostic for delivery gaps (TODO #726): if FinalText is
		// empty here despite a turn that produced output tokens,
		// something dropped the text on the way from CC's stream
		// through to the renderer. Surface length so it's easy to
		// correlate with the usage line.
		a.logger().Debugf("deferred TurnComplete: session=%s final_text_len=%d err=%v folded=%v",
			sessionKey, len(ts.FinalText), err, ts.foldedFollowUp)
		complete := turnevent.TurnComplete{
			FinalText:   ts.FinalText,
			FinalNotice: ts.FinalNotice,
			Usage:       ts.FinalUsage,
			Cost:        ts.FinalCost,
			Model:       ts.FinalModel,
			Err:         err,
			TurnID:      ts.RowID(),
			Trigger:     ts.Trigger,
			Via:         triggerToPlatform(ts.Trigger),
			Effort:      a.turnEffort(ts),

			DispatchedAt: a.turnDispatchedAt(ts),
		}
		if ts.foldedFollowUp && err == nil && a.sessionRouter(sessionKey).routesTo(sink) {
			// The input joined a turn already in flight, which owns the
			// session's delivery and ends with its own TurnComplete. On a
			// platform turn the ctx sink is the session router, so this empty
			// completion would reach THAT turn's sink and finalize it mid-run:
			// the reply streamed so far went out as a message, and the full
			// reply went out again when the run delivered it (#2366). Close
			// only this turn's trace.
			//
			// A caller's OWN sink (an ask answer's SessionSink/appSink, a
			// BufferSink) still gets it: it pairs that sink's TurnStart (typing
			// off, activity idle) and reaches no other turn.
			tspan.Complete(complete.FinalText, complete.Model, complete.Usage, complete.Cost, nil)
		} else {
			sink.Emit(ctx, complete)
		}
		// Session-lifecycle hooks — fired at the ONE boundary every backend
		// turn passes through (platform turns via turn.RunTurn→HandleMessage,
		// injections via HandleMessage directly), so system injections warm
		// the keepalive cache clock and flush deferred warnings just like
		// platform turns. Fired regardless of turn error (mirrors the platform
		// Driver's WrapTurn) — but only for a turn that RAN: a delegated turn
		// whose input never reached the backend (a skipped keepalive, a system
		// turn abandoned in the begin loop, a failed write, a stale redelivery)
		// ended no turn, so it fires neither hook (#2446). TurnComplete above
		// still fires (unless folded): it pairs the sink's TurnStart and closes the trace.
		if a.DelegatedManager == nil || ts.inputDelivered {
			if a.onTurnComplete != nil {
				a.onTurnComplete()
			}
			if a.onTurnEnd != nil {
				a.onTurnEnd()
			}
		}
	}()

	_, err = a.OrchestrateFullTurn(ctx, tc, ts)
	return err
}

// validateSessionOwnership enforces the invariant that an Agent processes
// only its own sessions. Returns an error if sessionKey parses to a structured
// key whose AgentID differs from this Agent's own AgentID. This catches
// cross-agent routing bugs where a foreign session ends up in the wrong
// workdir / backend / permission scope (e.g. via send_to_session's
// reply_to=caller path mis-dispatching to the caller's Agent).
//
// Exemptions:
//   - Agents without an AgentID set (test mode) skip the check.
//   - Legacy/unparseable session keys (test-only formats like "test/s")
//     pass through — production code uses structured keys, but the agent
//     test suite has many legacy keys we must not break.
func (a *Agent) validateSessionOwnership(sessionKey string) error {
	if a.AgentID == "" {
		return nil
	}
	sk, parseErr := session.ParseSessionKey(sessionKey)
	if parseErr != nil {
		return nil
	}
	if sk.AgentID != a.AgentID {
		return fmt.Errorf("HandleMessage invariant violation: agent %q received session key %q owned by agent %q", a.AgentID, sessionKey, sk.AgentID)
	}
	return nil
}

// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage provider.Usage
}

// SkillScope returns what this agent's read tool calls are judged against
// when deciding whether they loaded a skill file (#2392): its live skill
// directories and its workspace. Read at call time, so a live extra_dirs edit
// (#2657) applies to the next chip and fetch. Nil-safe on SkillDirs: an agent
// without skill dirs gets a scope with no roots.
func (a *Agent) SkillScope() skills.ReadScope {
	return skills.ReadScope{WorkDir: a.Workspace, Dirs: a.SkillDirs.Get()}
}

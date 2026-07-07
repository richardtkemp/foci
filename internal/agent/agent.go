package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/memory"
	"foci/internal/modelinfo"
	"foci/internal/nudge"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/warnings"
	"foci/internal/workspace"
)

// NoResponseSentinel is the marker that prompts instruct the model to emit
// when it has nothing to say. The agent strips it before delivery so the
// user never sees it (and the platform treats it as an empty response).
const NoResponseSentinel = "[[NO_RESPONSE]]"

// nudgeHeader prefixes automatic nudge messages so the agent understands
// their origin and treats them as background guidance, not user input.
const nudgeHeader = "[Background nudge — a private note to yourself, not from the user. Apply it only if it genuinely fits what you're already doing; if it doesn't, ignore it and don't bend your reply to accommodate it. Don't refer to the nudge directly in what you send.]\n"

// nudgeFooter is appended to every nudge so the agent has explicit guidance
// on when to stay silent vs reply. If the nudge calls for an action or the
// agent has other pending work, it should respond; otherwise it emits
// NoResponseSentinel so the platform delivers nothing.
const nudgeFooter = "\n\nReply only if the nudge warrants it — if it calls for action or you have other pending work, send that reply; otherwise respond with `" + NoResponseSentinel + "` and nothing else."

// nudgeEndMarker closes the nudge region when one or more nudges are
// bundled with a real user message in the same turn. It lets the agent
// see exactly where the background nudge ends and the user's text begins.
// Emitted once, after the last nudge, at the bundling join point — never
// per-nudge, and never on the standalone mid-loop paths (after-tools,
// pre-answer) where there is no following user text.
const nudgeEndMarker = "[End of background nudge — the user's message follows below.]"

// wrapNudge composes a full nudge message: header + reminder body + footer.
// Used by the standalone mid-loop delivery paths (after-tools, pre-answer)
// where the nudge is its own injected user message and the NO_RESPONSE
// footer is the correct silence-vs-reply guidance.
func wrapNudge(reminder string) string {
	return nudgeHeader + reminder + nudgeFooter
}

// wrapBundledNudge wraps a nudge that is prepended to a real user message
// in the same turn (the start-of-turn interval/regex injection path). It
// omits nudgeFooter because a reply to the user is always required on that
// path — the [[NO_RESPONSE]] guidance would contradict the user's message.
// The closing nudgeEndMarker is emitted separately at the join point so a
// single delimiter appears after all bundled nudges, not one per nudge.
func wrapBundledNudge(reminder string) string {
	return nudgeHeader + reminder
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
	Compactor       *compaction.Compactor // nil disables auto-compaction
	AsyncNotifier   *tools.AsyncNotifier  // nil disables async-pending compaction guard
	AskRouter       *tools.AskRouter      // nil disables typed-answer routing for the foci-native `ask` tool
	Reminders       *memory.ReminderStore // nil disables reminder injection
	TaskListStore   *memory.TaskListStore // nil disables task list in state dashboard
	TodoStore       *memory.TodoStore     // nil disables todo count in state dashboard
	ScratchpadStore *memory.Scratchpad    // nil disables scratchpad count in state dashboard
	AgentID         string                // unique agent identifier (for per-agent DB queries)
	Model           string                // "developer/model_id" format (e.g. "anthropic/claude-opus-4-6")
	Format          string                // wire format resolved at startup (e.g. "anthropic", "gemini", "openai")
	Endpoint        string                // agent's default endpoint (sessions inherit this unless overridden)
	Log             *log.ComponentLogger  // structured logger for this agent

	EnvironmentBlock                        string                                  // static environment block (fallback; tests). Prod uses EnvironmentBlockFunc.
	EnvironmentBlockFunc                    func(sessionKey string) string          // per-session environment block (platform-aware); preferred over EnvironmentBlock
	ExtraSystemBlocks                       []provider.SystemBlock                  // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy                           string                                  // "auto" (top-level) or "explicit" (manual breakpoints) — from primary model config
	CacheBustDetect                         bool                                    // detect cache busts (cache_read drop >50%)
	CacheBustIdleThreshold                  time.Duration                           // suppress cache bust alert if session idle > this (default 10m)
	CacheBustAlert                          HookList[CacheBustFunc]                 // callbacks for cache bust alerts
	DuplicateMessages                       bool                                    // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages           bool                                    // accumulate mid-turn text; send concatenated on turn end (default false = send immediately)
	BatchPartialJoiner                      string                                  // separator between batched partial messages (default "")
	MaxResultChars                          int                                     // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir                       string                                  // where to write large tool results
	SummaryContextTurns                     int                                     // recent conversation turns for summary context
	SummaryContextChars                     int                                     // max chars of context to send to cheap model
	MaxSummaryChars                         int                                     // max chars to auto-summarise (skip cheap model above this)
	MaxSummaryInputChars                    int                                     // max chars of tool result embedded in summary prompt (0 = no limit)
	GroupResolver                           *config.GroupResolver                   // resolves call sites to model groups
	FallbackFunc                            provider.FallbackFunc                   // nil disables automatic model fallback on transient errors
	MaxImagePixels                          int                                     // max pixels (w*h) for images before downscaling; 0 disables
	AutoSummarise                           bool                                    // enable auto-summarise of oversized tool results (default true)
	WarningQueue                            *warnings.Queue                         // nil disables warning injection into session
	ChatWarningQueue                        *warnings.Queue                         // nil disables chat warning notifications
	MaxTokensWarnFunc                       HookList[func(string)]                  // callbacks when stop_reason=max_tokens (response truncated)
	RateLimitFunc                           HookList[func(resetTime time.Time)]     // callbacks when API returns 429 (rate limited)
	ReloadOnCompact                         bool                                    // delegated: after compaction, bounce the CC session (resume) so character/skill files reload from disk (#828)
	TaskListNotifyFunc                      HookList[func(string, string)]          // callbacks for task list changes (session key, message)
	CompactionMemoryFunc                    HookList[func(string)]                  // fires before compaction to save memories (session key)
	CompactionStartFunc                     HookList[func(string, string)]          // callbacks for compaction start (session key, message) — sent immediately, not buffered
	CompactionNotifyFunc                    HookList[func(string, string)]          // callbacks for compaction notifications (session key, message)
	CompactionDebugFunc                     HookList[func(string, string)]          // callbacks for compaction debug (session key, summary text)
	OnActivity                              HookList[func(string)]                  // callbacks when a session has activity (session key)
	Redact                                  func(string) string                     // redact secrets from tool output; nil disables
	SessionIndex                            *session.SessionIndex                   // nil disables state persistence
	MessageTransforms                       []CompiledTransform                     // compiled regex rules for inbound message transformation
	CompactionSummaryPromptPath             string                                  // file path; read at compaction time via prompts.ResolvePrompt
	CompactionHandoffMsg                    string                                  // inline handoff message; empty resolves from search dirs or embedded default
	PromptSearchDirs                        []string                                // directories to search for prompt files (agent workspace, shared)
	MaxToolLoops                            int                                     // max tool iterations per turn (default 25)
	MaxOutputTokens                         int                                     // max tokens in model response (default 16384)
	Nudger                                  *nudge.Scheduler                        // nil disables nudge reminders
	NudgePreAnswerGate                      bool                                    // enable pre-answer verification gate
	NudgePreAnswerMinTools                  int                                     // min tool calls before gate fires (default 2)
	NudgeReloadFunc                         func()                                  // called after bootstrap reload to refresh nudge rules; nil disables
	FirstRunMessage                         atomic.Value                            // string; prepended as separate content block on first HandleMessage, then cleared
	OnFirstRunConsumed                      func()                                  // fired once when FirstRunMessage is actually consumed into a delivered turn (nil disables); marks onboarding complete
	TurnLockWarnThreshold                   time.Duration                           // warn if turn lock wait exceeds this (default 3m)
	ShowToolCalls                           string                                  // agent-level default: "off"/"preview"/"full" (per-session overrides via /display)
	Statusline                              string                                  // per-agent [meta]/[state] header template; "" = DefaultStatuslineTemplate (#831)
	Streaming                               bool                                    // use streaming API when provider supports it
	ModelMetaFn                             func(model string) modelinfo.ModelMeta  // per-model meta from config (context window)
	ModelDefaultsFn                         func(model string) config.ModelDefaults // returns per-model defaults from [models.*] config; nil = no model defaults
	CanRunBackground                        string                                  // path to an executable gating background work; exit 0 = allowed, non-zero = skip; "" = always allowed
	ServerTools                             []provider.ToolDef                      // server-side tools (web_search, web_fetch) — executed by Anthropic, not client
	DelegatedManager                        *DelegatedManager                       // nil = traditional agent loop; non-nil = lazy per-session delegated transport management
	ReloginTrigger                          func(reason, sessionKey string) bool    // nil unless an ccstream backend is wired; starts the #843 re-login flow, returns false if one is already in flight. sessionKey (may be "") targets the chat that gets the login URL; "" falls back to the agent's default chat.
	Reflection                              config.ResolvedReflection               // resolved reflection config (agent+global merged)
	DefaultPlatform                         string                                  // configured default_platform (per-agent, else global); preferred for default-session resolution and delivery fallback
	ResetOrientTemplateFn                   func() string                           // resolves orientation template for session reset; nil = no orientation
	ReloadSystemFn                          func() ([]provider.SystemBlock, int)    // reloads skills/extra blocks; returns new blocks + count; nil = no-op

	platforms  map[string]platform.Sender // per-agent platforms (telegram, discord, etc.); key = platform name
	platformMu sync.RWMutex               // protects platforms map access

	rateLimitGates     map[string]*RateLimitGate // per-endpoint gates; key = endpoint name, lazy-init
	rateLimitGatesMu   sync.RWMutex              // protects rateLimitGates map access
	inFlightMu         sync.Mutex                // protects inFlight + inFlightDelivering + inFlightChanged map access
	inFlight           map[string]int32          // per-session count of in-flight OrchestrateFullTurn calls — see IsTurnInFlight
	inFlightDelivering map[string]int32          // per-session-base count of in-flight turns whose sink reports DeliversToPlatform=true — see IsInFlightDelivering
	inFlightChanged    map[string]chan struct{}  // per-session-base close-and-replace channel; closes on each state change so waiters wake — see InFlightWaitCh
	turnDetailsMu      sync.Mutex
	turnDetails        map[uint64]*TurnDetail // keyed by unique turn ID
	turnIDCounter      uint64                 // atomic: monotonic turn ID
	turnLocksMu        sync.Mutex
	turnLocks          map[string]*sync.Mutex // per-session turn serialization
	metaMu             sync.Mutex
	meta               map[string]*sessionMeta // per-session metadata

	// compacting latches compaction-in-flight per session so /status reports
	// "compacting" rather than "idle" while a summary turn runs. Keyed by
	// session key → time.Time deadline for self-heal. Set/cleared around the
	// compaction brackets in compaction.go; read via IsCompacting (#725).
	compacting sync.Map

	// Inbox subsystem (Phase 6 — TODO #739): per-session message queue
	// + worker. Bot calls Enqueue(envelope) after filtering; agent owns
	// queueing, batching, steer dispatch, and turn execution via the
	// platform-supplied Driver. See inbox.go.
	inboxes        map[string]*sessionInbox                                          // per-session inbox registry, lazy-created
	inboxesMu      sync.Mutex                                                        // protects inboxes map + StartInbox idempotency
	inboxStarted   bool                                                              // true once StartInbox has been called
	inboxCtx       context.Context                                                   //nolint:containedctx // parent ctx for session worker goroutines
	inboxSteerMode bool                                                              // urgent-steer dispatch enabled
	inboxBackend   func(ctx context.Context, sk string) (delegator.Delegator, error) // test seam; nil = use DelegatedManager
	turnObserver   func(sk string, batch []Envelope)                                 // test seam — see SetTurnObserver / TODO #746 Stage C

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
	// ResolveLateConn resolves the current delivering connection for a session
	// key, for the router's late-delivery fallback. Called at Emit time so it
	// tracks connect/disconnect (a session that reconnects starts delivering
	// again with no router rebuild). Wired from cmd/foci-gw via route.ConnFor;
	// nil (tests / no connection manager) → the fallback discards-and-warns.
	ResolveLateConn func(sk string) platform.Connection
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

// ResolveCallSite resolves a call site to a (client, model, format) triple.
// For ungrouped calls or nil GroupResolver, returns the session's client/model/format.
// Otherwise resolves via GroupResolver and gets the appropriate client.
func (a *Agent) ResolveCallSite(callSite, sessionKey string) (provider.Client, string, string) {
	if a.GroupResolver == nil {
		return a.SessionClient(sessionKey), a.SessionModel(sessionKey), a.Format
	}
	resolved := a.GroupResolver.ResolveCall(callSite)
	if resolved == nil {
		return a.SessionClient(sessionKey), a.SessionModel(sessionKey), a.Format
	}
	client := a.SessionClient(sessionKey)
	if a.ClientProvider != nil {
		if c := a.ClientProvider.GetClient(resolved.Endpoint, resolved.Format); c != nil {
			client = c
		}
	}
	return client, resolved.Developer + "/" + resolved.ModelID, resolved.Format
}

// TurnDetail describes one in-flight turn for shutdown diagnostics.
type TurnDetail struct {
	SessionKey string
	Trigger    string // "user", "keepalive", "wake", "scheduled_wake", "telegram", "async_notify"
	ToolName   string // currently executing tool, or empty
	StartTime  time.Time
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

// turnLock returns a per-session mutex that serializes HandleMessage calls.
// This prevents concurrent turns on the same session from interleaving messages
// in the session file, which would invalidate Anthropic's prefix-matched prompt cache.
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
// attached to ctx (see internal/agent/turnevent). Callers that want the final
// text wire a BufferSink; callers that want streaming UI wire a StreamingSink.
//
// TurnStart fires at entry; TurnComplete always fires via defer, carrying the
// accumulated FinalText, Usage, Cost, Model, and any error returned by the
// turn orchestrator.
func (a *Agent) HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) (err error) {
	if err := a.validateSessionOwnership(sessionKey); err != nil {
		return err
	}

	sink := turnevent.SinkFromContext(ctx)
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
		log.Debugf("agent", "deferred TurnComplete: session=%s final_text_len=%d err=%v",
			sessionKey, len(ts.FinalText), err)
		sink.Emit(ctx, turnevent.TurnComplete{
			FinalText: ts.FinalText,
			Usage:     ts.DisplayUsage(), // header chips: in/out/cw summed, cache_read last-call (size from FinalUsage, billing from ledger)
			Cost:      ts.FinalCost,
			Model:     ts.FinalModel,
			Err:       err,
		})
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

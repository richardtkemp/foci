package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/mana"
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
const nudgeHeader = "[system: automatic nudge — incorporate this guidance naturally without mentioning this nudge to the user.]\n"

// CacheBustFunc is called when a cache bust is detected (cache_read drops
// significantly compared to the previous request).
// session is the session key, prevRead is what we had, curRead is what we got.
type CacheBustFunc func(session string, prevRead, curRead int)

// Agent is the core agent loop.
type Agent struct {
	Client         provider.Client
	ClientProvider provider.ClientProvider // provides access to API clients for different endpoint:format pairs
	Sessions       *session.Store
	Tools          *tools.Registry
	Bootstrap      *workspace.Bootstrap
	Compactor      *compaction.Compactor // nil disables auto-compaction
	AsyncNotifier  *tools.AsyncNotifier  // nil disables async-pending compaction guard
	Reminders       *memory.ReminderStore  // nil disables reminder injection
	TaskListStore   *memory.TaskListStore  // nil disables task list in state dashboard
	TodoStore       *memory.TodoStore      // nil disables todo count in state dashboard
	ScratchpadStore *memory.Scratchpad     // nil disables scratchpad count in state dashboard
	AgentID         string                 // unique agent identifier (for per-agent DB queries)
	Model          string                // "developer/model_id" format (e.g. "anthropic/claude-opus-4-6")
	Format         string                // wire format resolved at startup (e.g. "anthropic", "gemini", "openai")
	Endpoint       string                // agent's default endpoint (sessions inherit this unless overridden)
	Log            *log.ComponentLogger  // structured logger for this agent

	EnvironmentBlock              string                       // pre-built environment context block (prepended first in system prompt)
	ExtraSystemBlocks             []provider.SystemBlock       // additional system blocks (e.g. skills list), injected before cache marker
	CacheStrategy                 string                       // "auto" (top-level) or "explicit" (manual breakpoints) — from primary model config
	CacheBustDetect               bool                         // detect cache busts (cache_read drop >50%)
	CacheBustIdleThreshold        time.Duration                // suppress cache bust alert if session idle > this (default 10m)
	CacheBustAlert                HookList[CacheBustFunc]      // callbacks for cache bust alerts
	DuplicateMessages             bool                         // send user text twice per API call (improves instruction following)
	BatchPartialAssistantMessages bool                         // accumulate mid-turn text; send concatenated on turn end (default false = send immediately)
	BatchPartialJoiner            string                       // separator between batched partial messages (default "")
	MaxResultChars                int                          // max chars for tool result before writing to file (0 disables)
	ToolResultTempDir             string                       // where to write large tool results
	SummaryContextTurns           int                          // recent conversation turns for summary context
	SummaryContextChars           int                          // max chars of context to send to cheap model
	MaxSummaryChars               int                          // max chars to auto-summarise (skip cheap model above this)
	MaxSummaryInputChars          int                          // max chars of tool result embedded in summary prompt (0 = no limit)
	GroupResolver                 *config.GroupResolver         // resolves call sites to model groups
	FallbackFunc                  provider.FallbackFunc          // nil disables automatic model fallback on transient errors
	MaxImagePixels                int                          // max pixels (w*h) for images before downscaling; 0 disables
	AutoSummarise                 bool                         // enable auto-summarise of oversized tool results (default true)
	WarningQueue                  *warnings.Queue              // nil disables warning injection into session
	ChatWarningQueue              *warnings.Queue              // nil disables chat warning notifications
	ManaWatcher                   *ManaWatcher                 // nil disables mana threshold warnings
	ManaWarnFunc                  HookList[func(string)]                 // callbacks for mana threshold warnings (e.g. platform notification)
	MaxTokensWarnFunc             HookList[func(string)]                 // callbacks when stop_reason=max_tokens (response truncated)
	RateLimitFunc                 HookList[func(resetTime time.Time)]    // callbacks when API returns 429 (rate limited)
	AutocompactBeforeManaRefresh          bool                                   // master switch for mana-refresh compaction
	AutocompactBeforeManaRefreshThreshold string                                 // trigger mana-refresh when reset this soon (e.g. "5m")
	AutocompactBeforeManaRefreshFactor    float64                               // secondary threshold = main threshold × factor (e.g. 0.5)
	AutocompactBeforeManaRefreshPreserve    *int                                   // messages to preserve in refresh mode (nil = use percentage)
	AutocompactBeforeManaRefreshPreservePct float64                                // fraction of messages to preserve in refresh mode (0 = default 0.5)
	TaskListNotifyFunc            HookList[func(string, string)]         // callbacks for task list changes (session key, message)
	CompactionMemoryFunc          HookList[func(string)]                 // fires before compaction to save memories (session key)
	CompactionStartFunc           HookList[func(string, string)]         // callbacks for compaction start (session key, message) — sent immediately, not buffered
	CompactionNotifyFunc          HookList[func(string, string)]         // callbacks for compaction notifications (session key, message)
	CompactionDebugFunc           HookList[func(string, string)]         // callbacks for compaction debug (session key, summary text)
	SessionKeyRotatedFunc         HookList[func(string, string)]         // callbacks when session key rotates (oldKey, newKey)
	OnActivity                    HookList[func(string)]                 // callbacks when a session has activity (session key)
	Redact                        func(string) string          // redact secrets from tool output; nil disables
	SessionIndex                  *session.SessionIndex        // nil disables state persistence
	UsageClient                   mana.UsageClient             // nil disables mana metadata
	UsageClientProvider           mana.UsageClientProvider     // per-endpoint usage client resolution (nil = use default UsageClient)
	MessageTransforms             []CompiledTransform          // compiled regex rules for inbound message transformation
	CompactionSummaryPromptPath   string                       // file path; read at compaction time via prompts.ResolvePrompt
	CompactionHandoffMsg          string                       // inline handoff message; empty resolves from search dirs or embedded default
	PromptSearchDirs              []string                     // directories to search for prompt files (agent workspace, shared)
	MaxToolLoops                  int                          // max tool iterations per turn (default 25)
	MaxOutputTokens               int                          // max tokens in model response (default 16384)
	Nudger                        *nudge.Scheduler             // nil disables nudge reminders
	NudgePreAnswerGate            bool                         // enable pre-answer verification gate
	NudgePreAnswerMinTools        int                          // min tool calls before gate fires (default 2)
	NudgeReloadFunc               func()                       // called after bootstrap reload to refresh nudge rules; nil disables
	FirstRunMessage               atomic.Value                 // string; prepended as separate content block on first HandleMessage, then cleared
	TurnLockWarnThreshold         time.Duration                // warn if turn lock wait exceeds this (default 3m)
	ShowToolCalls                 string                       // agent-level default: "off"/"preview"/"full" (per-session overrides via /display)
	Streaming                     bool                         // use streaming API when provider supports it
	ModelMetaFn                   func(model string) modelinfo.ModelMeta  // per-model meta from config (context window)
	ModelDefaultsFn               func(model string) config.ModelDefaults // returns per-model defaults from [models.*] config; nil = no model defaults
	ManaInvestInterval            time.Duration                // invest interval for mana good/bad indicator; 0 = no indicator
	ServerTools                   []provider.ToolDef           // server-side tools (web_search, web_fetch) — executed by Anthropic, not client
	DelegatedManager              *DelegatedManager            // nil = traditional agent loop; non-nil = lazy per-session delegated transport management
	Reflection                    config.ResolvedReflection      // resolved reflection config (agent+global merged)
	ResetOrientTemplateFn         func() string                  // resolves orientation template for session reset; nil = no orientation
	ResetNotifyFunc               HookList[func(string, string)] // fires progress notifications during reset (session key, message)
	ReloadSystemFn                func() ([]provider.SystemBlock, int) // reloads skills/extra blocks; returns new blocks + count; nil = no-op

	platforms  map[string]platform.Sender // per-agent platforms (telegram, discord, etc.); key = platform name
	platformMu sync.RWMutex               // protects platforms map access

	rateLimitGates   map[string]*RateLimitGate // per-endpoint gates; key = endpoint name, lazy-init
	rateLimitGatesMu sync.RWMutex              // protects rateLimitGates map access
	processing       int32                     // atomic: number of in-flight HandleMessage calls
	turnDetailsMu    sync.Mutex
	turnDetails      map[uint64]*TurnDetail // keyed by unique turn ID
	turnIDCounter    uint64                 // atomic: monotonic turn ID
	turnLocksMu      sync.Mutex
	turnLocks        map[string]*sync.Mutex // per-session turn serialization
	metaMu           sync.Mutex
	meta             map[string]*sessionMeta // per-session metadata

	// Inbox subsystem (Phase 6 — TODO #739): per-session message queue
	// + worker. Bot calls Enqueue(envelope) after filtering; agent owns
	// queueing, batching, steer dispatch, and turn execution via the
	// platform-supplied Driver. See inbox.go.
	inboxes        map[string]*sessionInbox // per-session inbox registry, lazy-created
	inboxesMu      sync.Mutex               // protects inboxes map + StartInbox idempotency
	inboxStarted   bool                     // true once StartInbox has been called
	inboxCtx       context.Context          //nolint:containedctx // parent ctx for session worker goroutines
	inboxSteerMode bool                     // urgent-steer dispatch enabled
	inboxBackend   func(ctx context.Context, sk string) (delegator.Delegator, error) // test seam; nil = use DelegatedManager
	turnObserver   func(sk string, batch []Envelope)                                 // test seam — see SetTurnObserver / TODO #746 Stage C
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

// logger returns the agent's ComponentLogger, lazily creating a default if nil.
func (a *Agent) logger() *log.ComponentLogger {
	if a.Log != nil {
		return a.Log
	}
	a.Log = log.NewComponentLogger("agent")
	return a.Log
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

// IsProcessing returns true if the agent is currently handling a message.
func (a *Agent) IsProcessing() bool {
	return atomic.LoadInt32(&a.processing) > 0
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


// SetProcessingForTest sets the processing counter directly. Test-only.
func (a *Agent) SetProcessingForTest(n int32) {
	atomic.StoreInt32(&a.processing, n)
}

// isSystemMessage returns true if the message is from a system source
// (keepalive, scheduled wake, proactive warnings) rather than a human user.
func isSystemMessage(msg string) bool {
	return strings.HasPrefix(msg, "[KEEPALIVE]") ||
		strings.HasPrefix(msg, "[SCHEDULED WAKE]") ||
		strings.HasPrefix(msg, "[proactive system warnings]")
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
			Usage:     ts.FinalUsage,
			Cost:      ts.FinalCost,
			Model:     ts.FinalModel,
			Err:       err,
		})
	}()

	_, err = a.OrchestrateFullTurn(ctx, tc, ts)
	return err
}



// TurnResult holds the result of a single agent turn.
// (For compaction to use.)
type TurnResult struct {
	Text  string
	Usage provider.Usage
}

package agent

import (
	"context"

	"foci/internal/backend"
	"foci/internal/platform"
	"foci/internal/provider"
)

// TurnContract defines every concern of a turn. Both APITransport and
// BackendTransport implement every method. Adding a method here produces
// a compile error in both until implemented.
//
// Methods are grouped into four phases:
//
//	Phase 1  — Pre-lock gates and registration
//	Phase 2  — Turn preparation (prompt, session, model resolution)
//	Phase 3  — Core execution (API tool loop or backend SendTurn)
//	Phase 4  — Post-turn (save, metadata, compaction, logging)
type TurnContract interface {
	// --- Phase 1: Pre-lock gates and registration ---

	// RateLimitGate checks the per-endpoint rate limit. Returns
	// *RateLimitedError if the endpoint is gated.
	// Backend: no-op (CC has its own rate limiting).
	RateLimitGate(ts *TurnState) error

	// AcquireTurnLock serializes turns on the same session to preserve
	// prompt cache prefix ordering. Returns an unlock func.
	// Backend: no-op (CC serializes internally).
	AcquireTurnLock(ts *TurnState) (unlock func())

	// IncrementProcessing bumps the atomic processing counter.
	// Returns a decrement func.
	// Backend: no-op (backend turns are fire-and-forget from foci's view).
	IncrementProcessing(ts *TurnState) (decrement func())

	// RegisterTurn adds a TurnDetail for shutdown diagnostics.
	// Returns an unregister func.
	// Backend: no-op.
	RegisterTurn(ts *TurnState) (unregister func())

	// CheckStaleContext returns ctx.Err() if the context was cancelled
	// while waiting for the turn lock. Shared.
	CheckStaleContext(ts *TurnState) error

	// --- Phase 1b: Post-lock logging and tracking ---

	// RegisterSessionIndex ensures this session appears in the session
	// index for memory formation and state tracking. Shared.
	RegisterSessionIndex(ts *TurnState)

	// LogConversationRecv logs the inbound message to the conversation log. Shared.
	LogConversationRecv(ts *TurnState)

	// TouchActivity fires OnActivity callbacks for session liveness. Shared.
	TouchActivity(ts *TurnState)

	// --- Phase 2: Turn preparation ---

	// LoadSessionMeta loads or initialises per-session metadata. Shared.
	LoadSessionMeta(ts *TurnState)

	// ComposePrompt builds the user-facing prompt content.
	// API: rich content blocks via prepareUserMessage.
	// Backend: flat text via composeTurnText + JoinPrompt.
	ComposePrompt(ts *TurnState) error

	// LoadAndRepairSession loads session history and repairs corruption.
	// API: full load + 3 repair passes.
	// Backend: no-op (CC owns its session file).
	LoadAndRepairSession(ts *TurnState) error

	// ResolveModelEffort resolves model, effort, thinking, speed for this turn.
	// API: full resolution with per-model defaults.
	// Backend: reads agent-level model.
	ResolveModelEffort(ts *TurnState)

	// BuildSystemAndTools builds system prompt blocks and tool definitions.
	// API: per-turn rebuild from bootstrap.
	// Backend: no-op (system prompt set at Start time).
	BuildSystemAndTools(ts *TurnState)

	// InjectNudges prepends behavioral nudge reminders.
	// API: content blocks in the user message.
	// Backend: text prepended to the prompt string.
	InjectNudges(ts *TurnState)

	// --- Phase 3: Core execution ---

	// ExecuteTurn runs the core turn.
	// API: multi-iteration tool loop with streaming and fallback.
	// Backend: SendTurn to CC; watcher closes CompletionChan on end_turn.
	ExecuteTurn(ts *TurnState) error

	// --- Phase 4: Post-turn (fires after CompletionChan is closed) ---

	// SaveSession persists new messages to the session file.
	// API: AppendAll to session store.
	// Backend: no-op (CC owns its session file).
	SaveSession(ts *TurnState) error

	// UpdateSessionMeta updates per-session cost/token tracking.
	// API: from provider.MessageResponse usage.
	// Backend: from JSONL watcher usage.
	UpdateSessionMeta(ts *TurnState)

	// RunCompaction checks if compaction is needed and runs it.
	// API: direct maybeCompact call.
	// Backend: sends /compact command to CC.
	RunCompaction(ts *TurnState)

	// LogConversationSent logs the outbound response. Shared.
	LogConversationSent(ts *TurnState)

	// TouchActivityPost fires OnActivity after turn completion. Shared.
	TouchActivityPost(ts *TurnState)
}

// TurnState holds all per-turn data flowing through the TurnContract pipeline.
// Created at the start of each turn, populated progressively by each phase,
// consumed by post-turn methods.
type TurnState struct {
	// --- Inputs (set by caller) ---

	Ctx         context.Context
	SessionKey  string
	Texts       []string              // user message text(s); texts[0] is primary
	Attachments []platform.Attachment // optional file attachments

	// --- Derived from context (set by orchestrator) ---

	Meta    *TurnMetadata // user/chat metadata from context
	Trigger string        // trigger source ("telegram", "keepalive", etc.)

	// --- Phase 2 outputs ---

	SessionMeta *sessionMeta // per-session metadata (costs, timestamps, overrides)

	Messages    []provider.Message // loaded + repaired session history (API only)
	NewMessages []provider.Message // messages appended this turn (API only)
	UserMsg     provider.Message   // composed user message (API only)
	Prompt      string             // composed flat prompt (Backend only)

	TurnModel    string          // resolved model for this turn
	TurnClient   provider.Client // resolved client for this turn
	TurnEffort   string          // resolved effort level
	TurnThinking string          // resolved thinking mode
	TurnSpeed    string          // resolved speed setting

	EffectiveDuplicate bool // whether duplicate messages are active (API only)
	ConvChatID         int64 // chat ID for conversation logging

	System   []provider.SystemBlock // system prompt blocks (API only)
	ToolDefs []provider.ToolDef     // tool definitions (API only)

	// TurnDetail tracks the in-flight turn for shutdown diagnostics.
	TurnDetail *TurnDetail
	TurnID     uint64

	// --- Phase 3 outputs ---

	FinalText  string          // response text from the completed turn
	FinalUsage *provider.Usage // token usage from the completed turn

	// --- Async coordination ---

	// CompletionChan is closed when the turn actually completes.
	// API: closed before ExecuteTurn returns (synchronous).
	// Backend: closed by the watcher's OnTurnComplete callback (async).
	CompletionChan chan struct{}

	// --- Backend-specific ---

	Backend backend.Backend // backend instance for this session (nil for API)
}

// NewTurnState creates a TurnState with a properly initialised CompletionChan.
// Always use this instead of constructing TurnState directly — a nil
// CompletionChan panics on close.
func NewTurnState(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) *TurnState {
	return &TurnState{
		Ctx:            ctx,
		SessionKey:     sessionKey,
		Texts:          texts,
		Attachments:    attachments,
		CompletionChan: make(chan struct{}),
	}
}

// sharedTurnOps holds shared method implementations inherited by both
// APITransport and BackendTransport via embedding.
type sharedTurnOps struct {
	agent *Agent
}

// APITransport implements TurnContract for the traditional API code path
// (direct provider calls with client-side tool execution loop).
type APITransport struct {
	sharedTurnOps
}

// BackendTransport implements TurnContract for the coding agent backend path
// (Claude Code, etc. — the backend owns inference and tool execution).
type BackendTransport struct {
	sharedTurnOps
}

// Compile-time interface checks.
var _ TurnContract = (*APITransport)(nil)
var _ TurnContract = (*BackendTransport)(nil)

// ---------------------------------------------------------------------------
// Shared implementations on sharedTurnOps — inherited by both transports.
// Stage 2 will replace panic stubs with real logic extracted from
// HandleMessageWithAttachments / handleViaBackend.
// ---------------------------------------------------------------------------

func (s *sharedTurnOps) CheckStaleContext(ts *TurnState) error { panic("not implemented") }
func (s *sharedTurnOps) RegisterSessionIndex(ts *TurnState)    { panic("not implemented") }
func (s *sharedTurnOps) LogConversationRecv(ts *TurnState)     { panic("not implemented") }
func (s *sharedTurnOps) TouchActivity(ts *TurnState)           { panic("not implemented") }
func (s *sharedTurnOps) LoadSessionMeta(ts *TurnState)         { panic("not implemented") }
func (s *sharedTurnOps) LogConversationSent(ts *TurnState)     { panic("not implemented") }
func (s *sharedTurnOps) TouchActivityPost(ts *TurnState)       { panic("not implemented") }

// ---------------------------------------------------------------------------
// APITransport stubs — Stage 3 will replace with real implementations
// extracted from HandleMessageWithAttachments.
// ---------------------------------------------------------------------------

func (t *APITransport) RateLimitGate(ts *TurnState) error          { panic("not implemented") }
func (t *APITransport) AcquireTurnLock(ts *TurnState) func()       { panic("not implemented") }
func (t *APITransport) IncrementProcessing(ts *TurnState) func()   { panic("not implemented") }
func (t *APITransport) RegisterTurn(ts *TurnState) func()          { panic("not implemented") }
func (t *APITransport) ComposePrompt(ts *TurnState) error          { panic("not implemented") }
func (t *APITransport) LoadAndRepairSession(ts *TurnState) error   { panic("not implemented") }
func (t *APITransport) ResolveModelEffort(ts *TurnState)           { panic("not implemented") }
func (t *APITransport) BuildSystemAndTools(ts *TurnState)          { panic("not implemented") }
func (t *APITransport) InjectNudges(ts *TurnState)                 { panic("not implemented") }
func (t *APITransport) ExecuteTurn(ts *TurnState) error            { panic("not implemented") }
func (t *APITransport) SaveSession(ts *TurnState) error            { panic("not implemented") }
func (t *APITransport) UpdateSessionMeta(ts *TurnState)            { panic("not implemented") }
func (t *APITransport) RunCompaction(ts *TurnState)                { panic("not implemented") }

// ---------------------------------------------------------------------------
// BackendTransport stubs — Stage 4 will replace with real implementations.
// Methods that are genuinely no-ops for the backend path return zero values
// immediately; the backend explicitly opts out rather than silently skipping.
// ---------------------------------------------------------------------------

func (t *BackendTransport) RateLimitGate(ts *TurnState) error        { return nil }      // CC has its own rate limiting
func (t *BackendTransport) AcquireTurnLock(ts *TurnState) func()     { return func() {} } // CC serializes internally
func (t *BackendTransport) IncrementProcessing(ts *TurnState) func() { return func() {} } // fire-and-forget
func (t *BackendTransport) RegisterTurn(ts *TurnState) func()        { return func() {} } // not tracked externally
func (t *BackendTransport) ComposePrompt(ts *TurnState) error        { panic("not implemented") }
func (t *BackendTransport) LoadAndRepairSession(ts *TurnState) error { return nil }      // CC owns its session
func (t *BackendTransport) ResolveModelEffort(ts *TurnState)         { panic("not implemented") }
func (t *BackendTransport) BuildSystemAndTools(ts *TurnState)        {}                  // set at Start time
func (t *BackendTransport) InjectNudges(ts *TurnState)               { panic("not implemented") }
func (t *BackendTransport) ExecuteTurn(ts *TurnState) error          { panic("not implemented") }
func (t *BackendTransport) SaveSession(ts *TurnState) error          { return nil }      // CC owns its session
func (t *BackendTransport) UpdateSessionMeta(ts *TurnState)          { panic("not implemented") }
func (t *BackendTransport) RunCompaction(ts *TurnState)              { panic("not implemented") }

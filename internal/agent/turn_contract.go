package agent

import (
	"context"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
)

// TurnContract defines every concern of a turn. Both APITransport and
// DelegatedTransport implement every method. Adding a method here produces
// a compile error in both until implemented.
//
// Methods are grouped into four phases:
//
//	Phase 1  — Pre-lock gates and registration
//	Phase 2  — Turn preparation (prompt, session, model resolution)
//	Phase 3  — Core execution (API tool loop or delegated SendToPane)
//	Phase 4  — Post-turn (save, metadata, compaction, logging)
type TurnContract interface {
	// --- Phase 1: Pre-lock gates and registration ---

	// RateLimitGate checks the per-endpoint rate limit. Returns
	// *RateLimitedError if the endpoint is gated.
	// Delegated: no-op (CC has its own rate limiting).
	RateLimitGate(ts *TurnState) error

	// AcquireTurnLock serializes turns on the same session to preserve
	// prompt cache prefix ordering. Returns an unlock func.
	// Delegated: no-op (CC serializes internally).
	AcquireTurnLock(ts *TurnState) (unlock func())

	// IncrementProcessing bumps the atomic processing counter.
	// Returns a decrement func.
	// Delegated: no-op (delegated turns are fire-and-forget from foci's view).
	IncrementProcessing(ts *TurnState) (decrement func())

	// RegisterTurn adds a TurnDetail for shutdown diagnostics.
	// Returns an unregister func.
	// Delegated: no-op.
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
	// Delegated: flat text via composeTurnText + JoinPrompt.
	ComposePrompt(ts *TurnState) error

	// LoadAndRepairSession loads session history and repairs corruption.
	// API: full load + 3 repair passes.
	// Delegated: no-op (CC owns its session file).
	LoadAndRepairSession(ts *TurnState) error

	// ResolveModelEffort resolves model, effort, thinking, speed for this turn.
	// API: full resolution with per-model defaults.
	// Delegated: reads agent-level model.
	ResolveModelEffort(ts *TurnState)

	// BuildSystemAndTools builds system prompt blocks and tool definitions.
	// API: per-turn rebuild from bootstrap.
	// Delegated: no-op (system prompt set at Start time).
	BuildSystemAndTools(ts *TurnState)

	// InjectNudges prepends behavioral nudge reminders.
	// API: content blocks in the user message.
	// Delegated: text prepended to the prompt string.
	InjectNudges(ts *TurnState)

	// --- Phase 3: Core execution ---

	// RunInference runs the core turn.
	// API: multi-iteration tool loop with streaming and fallback.
	// Delegated: SendToPane to CC; watcher closes CompletionChan on end_turn.
	RunInference(ts *TurnState) error

	// --- Phase 4: Post-turn (fires after CompletionChan is closed) ---

	// SaveSession persists new messages to the session file.
	// API: AppendAll to session store.
	// Delegated: no-op (CC owns its session file).
	SaveSession(ts *TurnState) error

	// UpdateSessionMeta updates per-session cost/token tracking.
	// API: from provider.MessageResponse usage.
	// Delegated: from JSONL watcher usage.
	UpdateSessionMeta(ts *TurnState)

	// LogUsage records API usage to the usage database.
	// NOT called by the orchestrator — each transport invokes it at the
	// appropriate time. Present on the interface for compile-time enforcement.
	// API: per-call inside RunInference (via logAPIResponse; this method is a no-op).
	// Delegated: called from OnTurnComplete callback after FinalUsage is populated.
	LogUsage(ts *TurnState)

	// RunCompaction checks if compaction is needed and runs it.
	// API: direct maybeCompact call.
	// Delegated: sends /compact command to CC.
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
	Prompt      string             // composed flat prompt (delegated only)

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

	// StartedAt is when the turn began. Set once by the orchestrator.
	// Used for lastMessageTime, cache bust idle detection, etc.
	StartedAt time.Time

	// --- Phase 3 outputs ---

	FinalText  string          // response text from the completed turn
	FinalUsage *provider.Usage // token usage from the completed turn
	FinalCost  float64         // calculated cost from logAPIResponse
	FinalModel string          // model used (delegated: from JSONL; API: from TurnModel)

	// --- Async coordination ---

	// CompletionChan is closed when the turn actually completes.
	// API: closed before RunInference returns (synchronous).
	// Delegated: closed by the watcher's OnTurnComplete callback (async).
	CompletionChan chan struct{}

	// --- Delegated-specific ---

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
// APITransport and DelegatedTransport via embedding.
type sharedTurnOps struct {
	agent *Agent
}

// APITransport implements TurnContract for the traditional API code path
// (direct provider calls with client-side tool execution loop).
type APITransport struct {
	sharedTurnOps
}

// DelegatedTransport implements TurnContract for the delegated transport path
// (Claude Code, etc. — the backend owns inference and tool execution).
type DelegatedTransport struct {
	sharedTurnOps
}

// Compile-time interface checks.
var _ TurnContract = (*APITransport)(nil)
var _ TurnContract = (*DelegatedTransport)(nil)

// ---------------------------------------------------------------------------
// Shared implementations on sharedTurnOps — inherited by both transports.
// Stage 2 will replace panic stubs with real logic extracted from
// HandleMessageWithAttachments / the delegated transport.
// ---------------------------------------------------------------------------

// CheckStaleContext returns the context error if the context was cancelled
// (e.g. while waiting for the turn lock). Extracted from agent.go:330-332.
func (s *sharedTurnOps) CheckStaleContext(ts *TurnState) error {
	return ts.Ctx.Err()
}

// RegisterSessionIndex ensures the session exists in the session index.
// The API path gets this implicitly via store.Append → fireEvent → Upsert,
// but the delegated path never calls Append (CC owns its session file).
// Calling Upsert directly covers both paths uniformly.
// For delegated sessions, includes the CC JSONL file path so the pruner
// doesn't treat the entry as an orphan (no foci session file exists).
func (s *sharedTurnOps) RegisterSessionIndex(ts *TurnState) {
	if s.agent.SessionIndex == nil {
		return
	}
	now := time.Now()
	filePath := ""
	if s.agent.DelegatedManager != nil {
		filePath = s.agent.DelegatedManager.SessionFilePath(ts.SessionKey)
	}
	s.agent.SessionIndex.Upsert(session.SessionIndexEntry{
		SessionKey:     ts.SessionKey,
		FilePath:       filePath,
		CreatedAt:      now,
		LastActivityAt: now,
		SessionType:    session.ClassifySessionKey(ts.SessionKey),
		Status:         session.SessionStatusActive,
	})
}

// LogConversationRecv logs the inbound user message. Extracted from
// agent.go:335-350 and turn_delegated.go:25-32.
func (s *sharedTurnOps) LogConversationRecv(ts *TurnState) {
	chatID := ts.Meta.ChatID
	if chatID == 0 {
		chatID = session.ChatIDFromKey(ts.SessionKey)
	}
	ts.ConvChatID = chatID
	log.Conversation(log.ConversationEntry{
		Direction: "recv",
		UserID:    ts.Meta.UserID,
		Username:  ts.Meta.Username,
		ChatID:    chatID,
		Text:      strings.Join(ts.Texts, "\n"),
		Session:   ts.SessionKey,
	})
}

// TouchActivity fires OnActivity callbacks so session liveness is tracked.
// Extracted from agent.go:353-355 and turn_delegated.go:17-19.
func (s *sharedTurnOps) TouchActivity(ts *TurnState) {
	for _, fn := range s.agent.OnActivity {
		fn(ts.SessionKey)
	}
}

// LoadSessionMeta loads or initialises per-session metadata.
// Extracted from agent.go:440 and turn_delegated.go:36.
func (s *sharedTurnOps) LoadSessionMeta(ts *TurnState) {
	ts.SessionMeta = s.agent.getSessionMeta(ts.SessionKey)
}

// LogConversationSent logs the outbound response text. Extracted from
// agent.go:825-838. Skips empty text (no-response turns).
func (s *sharedTurnOps) LogConversationSent(ts *TurnState) {
	if ts.FinalText == "" {
		return
	}
	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    ts.Meta.UserID,
		Username:  ts.Meta.Username,
		ChatID:    ts.ConvChatID,
		Text:      ts.FinalText,
		Session:   ts.SessionKey,
	})
}

// TouchActivityPost fires OnActivity callbacks after the turn completes.
// Same as TouchActivity — separate method for contract clarity and because
// post-turn may run asynchronously (delegated path).
func (s *sharedTurnOps) TouchActivityPost(ts *TurnState) {
	for _, fn := range s.agent.OnActivity {
		fn(ts.SessionKey)
	}
}

// APITransport method implementations live in turn_api.go (Stage 3).

// DelegatedTransport method implementations live in turn_delegated.go (Stage 4).

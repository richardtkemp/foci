// Package backend defines the interface for coding agent backends
// (Claude Code, Codex, OpenCode, etc.) that handle entire agent turns
// including inference and tool execution. This is fundamentally different
// from provider.Client, which handles only the inference call while Foci
// executes tools.
package delegator

import (
	"context"
	"time"
)

// Backend is the interface that all coding agent backends implement.
// A Backend owns the entire turn: inference, tool execution, and context
// management. Foci sends composed prompts (with metadata, nudges, reminders)
// via Inject and receives streaming events back.
type Delegator interface {
	// Start launches the coding agent subprocess.
	// Called once during agent setup. The backend should be ready to
	// accept turns after Start returns.
	Start(ctx context.Context, opts StartOptions) error

	// Inject delivers a user-role event to the backend. Single funnel
	// for every path that produces input to the coding agent: primary
	// user turns, urgent steers, queued follow-ups, slash commands.
	//
	// The backend dispatches based on inj.Source and the current
	// IsTurnInFlight() state:
	//
	//   Source   | Turn state | Action
	//   ---------|------------|--------------------------------------------
	//   User     | idle       | begin turn (with attachments if provided)
	//   User     | in-flight  | send follow-up; CC's mid-turn drain folds it
	//   Steer    | in-flight  | send at priority "now"; CC folds at next tool boundary
	//   Steer    | idle       | begin turn — degrades to User-idle
	//   Compact  | any        | send slash command (fire-and-forget)
	//   Pass     | any        | send slash command (fire-and-forget)
	//
	// Returns whatever error the underlying writer/protocol returns. The
	// turn result (when applicable) flows through inj.Turn.OnTurnComplete;
	// delivery (text, tool events) flows through the SessionEvents
	// previously installed via AttachSessionEvents — independent of Turn.
	Inject(ctx context.Context, inj Inject) error

	// WaitForTurn blocks until the next turn completion (stop_reason
	// "end_turn" in the session output). Returns immediately if no turn
	// is in progress. Respects context cancellation/deadline.
	WaitForTurn(ctx context.Context) error

	// IsTurnInFlight reports whether a turn callback is registered but
	// hasn't fired yet. Inject consults this to decide between begin-turn
	// and follow-up routing.
	IsTurnInFlight() bool

	// IsRunning reports whether the agent subprocess is alive.
	IsRunning() bool

	// Restart kills and relaunches the agent subprocess.
	Restart(ctx context.Context) error

	// SetPermissionPromptFunc sets the function used to send permission
	// prompts with inline keyboard choices. Optional — if not set, the
	// backend logs and drops undeliverable prompts.
	SetPermissionPromptFunc(fn PermissionPromptFunc)

	// SetOnPromptsCleared sets a callback fired when the last outstanding
	// user-input prompt (permission, AskUserQuestion sequence, or MCP
	// elicitation) is resolved or cancelled. Used by DelegatedManager to
	// unblock WaitForPermission.
	SetOnPromptsCleared(fn func())

	// RegisterPromptCancelListener appends a callback fired when the prompt
	// with requestID is cancelled by a non-user path (e.g. CC's
	// control_cancel_request after a follow-up message aborted the in-flight
	// tool). The listener does NOT fire on normal user responses — use it to
	// clean up per-prompt UI state (e.g. disable the orphaned inline keyboard
	// so the user can't click an already-resolved button). Multiple listeners
	// may be registered for the same requestID; they fire in registration
	// order. If no prompt with requestID is registered (or the backend
	// doesn't track prompts — e.g. the legacy tmux backend), the call is a
	// silent no-op.
	RegisterPromptCancelListener(requestID string, fn func(reason string))

	// SetOnSessionReady sets a callback fired once when the backend
	// discovers its session ID. Used to persist the ID for resume-after-restart.
	SetOnSessionReady(fn func(sessionID string))

	// SetTypingFunc sets a callback to control the platform's typing indicator.
	// Called with true when the backend starts working (SendToPane), and false
	// when the turn completes (end_turn). Optional — nil means no typing.
	SetTypingFunc(fn func(typing bool))

	// AttachSessionEvents installs the session-scoped delivery sink. Called
	// once when the backend is acquired for a session, before the first
	// Inject. Subsequent calls replace the previous attachment. The events
	// live until the backend is closed — text/thinking/tool events flow
	// through them regardless of whether a per-turn TurnEvents bookkeeping
	// handler is currently armed. This is what divorces delivery (session
	// lifetime) from bookkeeping (turn lifetime), eliminating the
	// "text dropped: handler nil" failure mode at backend layer.
	AttachSessionEvents(events *SessionEvents)

	// SendKeystroke sends a single literal keypress to the agent's TUI.
	// Used for permission prompt responses where paste+Enter doesn't work.
	SendKeystroke(ctx context.Context, key string) error

	// SendSpecialKey sends a special key sequence (e.g. "Escape", "C-c", "C-u").
	// Unlike SendKeystroke, the key name is interpreted by tmux, not sent literally.
	SendSpecialKey(ctx context.Context, key string) error

	// Interrupt cancels any in-progress agent turn. The mechanism is
	// backend-specific: tmux sends Escape×2 + Ctrl-C; the stream backend
	// sends an interrupt control message over stdio.
	Interrupt(ctx context.Context) error

	// SessionID returns the coding agent's session identifier (e.g. CC's UUID).
	// Used to resume sessions after idle shutdown. Empty if unknown.
	SessionID() string

	// SessionFilePath returns the path to the coding agent's session JSONL file.
	// Empty if the session hasn't been discovered yet.
	SessionFilePath() string

	// WaitReady blocks until the coding agent is ready to accept prompts.
	// Implementations should detect the agent's UI/prompt indicator.
	// Respects context cancellation/deadline.
	WaitReady(ctx context.Context) error

	// Close shuts down the agent subprocess gracefully.
	Close() error
}

// Attachment is a file attachment to include as a structured content block.
// Mirrors platform.Attachment but lives in the delegator package to avoid
// a dependency on platform from backends.
type Attachment struct {
	MimeType string // "image/jpeg", "image/png", "application/pdf", etc.
	Data     []byte // raw binary data (will be base64-encoded for the wire)
}

// ControlSender is optionally implemented by backends that support
// runtime control requests (model switch, effort change, etc.).
// The Agent layer constructs backend-agnostic ControlRequest values;
// the backend translates them to its own wire format.
type ControlSender interface {
	SendControl(ctx context.Context, req ControlRequest) error
}

// CompactionWaiter is optionally implemented by backends that can signal
// when CC-initiated compaction has completed. ArmCompactionWait must be
// called before the /compact command is sent so that the compact_boundary
// stream event is never missed. WaitForCompaction then blocks until that
// event arrives (or ctx expires).
type CompactionWaiter interface {
	ArmCompactionWait()
	WaitForCompaction(ctx context.Context) error
}

// CompactionStartWaiter is optionally implemented by backends that can
// signal when CC has confirmed compaction is underway (status="compacting").
// Used to defer the ⏳ notification until compaction actually starts,
// avoiding a race where the notification overtakes buffered content.
type CompactionStartWaiter interface {
	ArmCompactionStartWait()
	WaitForCompactionStart(ctx context.Context) error
}

// ActivityChecker is optionally implemented by backends that track stream
// activity. Used by the orchestrator to replace fixed timeouts with
// activity-based detection: alive (events arriving) vs dead (stream silent).
type ActivityChecker interface {
	// LastActivity returns the time of the most recent stream event from
	// the backend. Zero time means no events have been received.
	LastActivity() time.Time
}

// CommandOutputCapturer is optionally implemented by backends that can
// capture local command output from the agent's TUI by polling for stable
// pane content. The tmux backend implements this; the stream backend
// doesn't need it (local commands produce system messages on stdout).
type CommandOutputCapturer interface {
	// CaptureCommandOutput polls the agent's display until it stabilises
	// (content unchanged for stableFor), checking every pollInterval.
	// Returns the raw display content, or error on timeout/context cancel.
	CaptureCommandOutput(ctx context.Context, stableFor, pollInterval time.Duration) (string, error)
}

// PromptChoice represents a choice in a permission prompt.
type PromptChoice struct {
	Label string // button text (e.g. "Yes", "No")
	Data  string // value to send to the pane (e.g. "1", "2")
}

// PermissionPromptFunc sends an interactive prompt to the user with keyboard
// choices. Used for both permission requests and AskUserQuestion prompts.
// requestID is the CC protocol request ID (empty for tmux backends).
// summary is a short description for post-action display (e.g.
// "Edit memory/2026-03-27.md"). If nil, the backend falls back to plain text.
type PermissionPromptFunc func(requestID, text, summary string, choices []PromptChoice)

// QuestionResponder is optionally implemented by backends that support
// the AskUserQuestion tool. It allows the agent layer to route user
// answers (button clicks or typed text) back to the backend.
type QuestionResponder interface {
	// RespondToQuestion handles a user's answer. choice is either
	// "qa:<index>" (button click), or raw text (custom typed answer).
	RespondToQuestion(requestID, choice string) error

	// CancelQuestion cancels a pending question (sends PermissionDeny).
	CancelQuestion(requestID string) error

	// HasPendingQuestion returns the request ID of a pending
	// AskUserQuestion, or empty string if none.
	HasPendingQuestion() string
}

// ElicitationResponder is optionally implemented by backends that support
// MCP elicitation control requests — requests from MCP servers for
// structured user input via form fields or a URL visit. ccstream implements
// this; other backends (cctmux) don't.
type ElicitationResponder interface {
	// RespondToElicitation handles one user action on a pending
	// elicitation. For form-mode flows, the backend advances through schema
	// fields on each call; the control_response is sent only when all
	// fields have been answered (or when the user declines/cancels).
	//
	// choice is one of:
	//   - "elic:accept"          URL-mode Done button
	//   - "elic:decline"         user declines
	//   - "elic:cancel"          user cancels
	//   - "elic:enum:<i>"        enum button click for current form field
	//   - "elic:bool:true|false" boolean button for current form field
	//   - any other string       free-text answer for current form field
	RespondToElicitation(requestID, choice string) error

	// HasPendingElicitation returns the request ID of a pending elicitation
	// currently awaiting a free-text field answer, or "" if none. Used by
	// the agent layer to intercept typed user messages as form input.
	HasPendingElicitation() string
}

// ContextUsage holds context window usage data returned by a backend's
// get_context_usage control request. Zero-cost (no API call).
type ContextUsage struct {
	TotalTokens          int              // tokens currently consumed
	MaxTokens            int              // total context window size
	Percentage           int              // usage percentage (0–100)
	AutoCompactThreshold int              // CC's autocompact trigger threshold
	Model                string           // model reported by CC
	Categories           []ContextCategory // per-category token breakdown
}

// ContextCategory is a single category in the context usage breakdown.
type ContextCategory struct {
	Name   string // e.g. "System prompt", "Messages", "Free space"
	Tokens int
}

// ContextUsageQuerier is optionally implemented by backends that support
// on-demand context window queries. The response is computed locally by CC
// (no API call), so it's cheap and fast (~650ms on a persistent session).
type ContextUsageQuerier interface {
	GetContextUsage(ctx context.Context) (*ContextUsage, error)
}

// StartOptions configures the backend at launch time.
type StartOptions struct {
	WorkDir         string // agent workspace directory (becomes cwd)
	SystemPrompt    string // concatenated character/system files
	Model           string // initial model (e.g. "opus", "sonnet")
	AgentID         string // foci agent ID
	Label           string // unique label for this instance (used for tmux window naming); falls back to AgentID
	ResumeSessionID string // resume a previous CC session (e.g. --resume <uuid>); empty = new session
	SessionKey      string // foci session key — used by exec bridge tools for routing (e.g. send_to_chat)
	ExecRegistry    any    // *tools.Registry — if set, used by DelegatedManager to create exec bridges
	Env             map[string]string // extra environment variables to inject (e.g. BASH_ENV, FOCI_SOCK from exec bridge)
	TmuxCols        int      // tmux window width (0 = use tools.tmux_cols default)
	TmuxRows        int      // tmux window height (0 = use tools.tmux_rows default)
	AutoApproveRules []string // foci-level auto-approve patterns (e.g. "Bash:git *", "Read")
}

// EventHandler is the legacy per-turn callback bundle: delivery callbacks
// (OnText, OnTextDelta, OnThinkingDelta, OnToolStart, OnToolEnd) and
// bookkeeping callbacks (OnTurnComplete, PostToolNudgeFunc, PreAnswerNudgeFunc)
// in one struct. Used by cctmux's JSONL watcher path. ccstream has migrated
// to the SessionEvents + TurnEvents split (see below) which divorces session-
// lifetime delivery from per-turn bookkeeping.
//
// Deprecated: prefer SessionEvents (delivery) + TurnEvents (bookkeeping)
// for new backends.
type EventHandler struct {
	OnText          func(text string)                           // complete text block from the agent
	OnTextDelta     func(delta string)                          // streaming text delta
	OnThinkingDelta func(delta string)                          // streaming thinking delta
	OnToolStart     func(id, name, input string)                // tool execution began
	OnToolEnd       func(id, name, output string, isError bool) // tool execution finished
	OnTurnComplete  func(result *TurnResult)                    // turn finished

	// PostToolNudgeFunc is called after each tool's completion signal.
	// See TurnEvents.PostToolNudgeFunc for full semantics.
	PostToolNudgeFunc func(toolName, toolInput string, isError bool) []string

	// PreAnswerNudgeFunc is called when the backend signals end_turn.
	// See TurnEvents.PreAnswerNudgeFunc for full semantics.
	PreAnswerNudgeFunc func(result *TurnResult) string
}

// SessionEvents are the session-scoped, always-callable delivery callbacks.
// Set once on a Backend via AttachSessionEvents and live for the session's
// lifetime. Never nil after attachment; the backend's text/tool emission
// path always reads through them, so per-turn handler nilling never causes
// a drop.
//
// Tool events carry the tool_use ID (from the JSONL/stream source) so
// consumers can correlate OnToolEnd with the originating OnToolStart
// without having to match by name or rely on ordering.
type SessionEvents struct {
	OnText          func(text string)                           // complete text block from the agent
	OnTextDelta     func(delta string)                          // streaming text delta (content_block_delta)
	OnThinkingDelta func(delta string)                          // streaming thinking delta (content_block_delta)
	OnToolStart     func(id, name, input string)                // tool execution began
	OnToolEnd       func(id, name, output string, isError bool) // tool execution finished
}

// TurnEvents are the per-turn bookkeeping callbacks. Set when a turn begins
// via Inject, cleared on OnResult. May be nil between turns; backend must
// tolerate that. These are bookkeeping only — delivery (text, tool events)
// flows through SessionEvents regardless.
type TurnEvents struct {
	// OnTurnComplete fires once when the turn finishes. The backend
	// captures-then-nils TurnEvents under turnMu in OnResult so this
	// invariant holds by construction (no counters needed).
	OnTurnComplete func(result *TurnResult)

	// PostToolNudgeFunc is called after each tool's completion signal
	// (PostToolUse hook dispatch). The caller returns any nudge reminders
	// that should be injected mid-turn as "now"-priority user messages,
	// matching the API transport's CheckAfterTools path. Non-blocking.
	// Used by the delegated transport to fire every_n_tools, after_error,
	// and tool_pattern nudge rules during long CC turns.
	//
	// toolInput is the raw tool_input JSON forwarded by the CC hook helper
	// (truncated at 64KB). Empty when the hook envelope omits it. Tool-
	// pattern rules use this to match on specific Bash commands, file
	// paths, etc. — see internal/nudge/scheduler.go.
	PostToolNudgeFunc func(toolName, toolInput string, isError bool) []string

	// PreAnswerNudgeFunc is called when CC signals end_turn on a result
	// message. The caller returns a follow-up prompt (or "" to let the
	// turn finalise as-is). When non-empty, the backend sends the
	// returned text as a new user message and treats its result as the
	// authoritative one. The TurnEvents stays bound across both rounds;
	// only the round-2 OnResult clears it.
	PreAnswerNudgeFunc func(result *TurnResult) string
}

// TurnResult is the outcome of a completed turn.
type TurnResult struct {
	Text      string     // final response text
	ToolCalls int        // number of tool calls executed during the turn
	Usage     *TurnUsage // token usage (nil if unavailable)
	Model     string     // model used (e.g. "claude-opus-4-6")
}

// InjectSource identifies the semantic origin of an injected user-role
// event. The backend's Inject method routes based on Source + the
// current IsTurnInFlight() state — see Delegator.Inject for the
// full routing matrix.
type InjectSource int

const (
	// SourceUser is user-originated text. Begins a new turn at idle, or
	// queues as a follow-up at default priority "next" behind the in-flight
	// turn. CC's mid-turn drain at the next tool boundary folds the
	// message as an attachment to the current ask() — the response
	// reaches the original handler in the same turn.
	SourceUser InjectSource = iota

	// SourceSteer is an urgent platform-side dispatch (Telegram/Discord
	// message arriving during an in-flight CC turn). Inject queues the
	// text at queue priority "now" — CC's mid-turn drain folds it ahead
	// of any other queued items at the next tool boundary, without
	// aborting the in-flight tool. The running tool finishes and the
	// model responds in the same turn. Steer no longer interrupts; for
	// "stop right now" semantics use /reset hard. At idle, degrades to
	// SourceUser-idle (begin turn).
	SourceSteer

	// SourceCompact is a /compact slash command sent to CC. Fire-and-forget:
	// CC processes the compaction internally. Caller is responsible for
	// arming compaction-completion waiters (CompactionWaiter) before
	// Inject if it wants to block on completion.
	SourceCompact

	// SourcePass is a passthrough slash command (/context, /model, etc.).
	// Fire-and-forget: response (if any) flows through the agent's normal
	// stream events.
	SourcePass
)

// String returns a stable lower-case label for logging.
func (s InjectSource) String() string {
	switch s {
	case SourceUser:
		return "user"
	case SourceSteer:
		return "steer"
	case SourceCompact:
		return "compact"
	case SourcePass:
		return "pass"
	default:
		return "unknown"
	}
}

// Inject describes a user-role event delivered to the backend via
// Delegator.Inject. See InjectSource for source-specific routing.
type Inject struct {
	// Source identifies the semantic origin of the event. Drives routing
	// and rearm policy.
	Source InjectSource

	// Text is the message body sent to CC. For SourceCompact / SourcePass
	// this is the slash command including its leading "/" (e.g.
	// "/compact summarise...").
	Text string

	// Attachments are optional structured content blocks (images, PDFs).
	// Only honored when the inject begins a new turn (idle state with
	// SourceUser); ignored otherwise. Backends that don't support
	// attachments silently drop them.
	Attachments []Attachment

	// Turn is the per-turn TurnEvents installed for the turn this inject
	// begins. Required for SourceUser / SourceSteer at idle when the
	// caller needs OnTurnComplete fired; ignored for in-flight injections
	// (the existing TurnEvents persists) and for slash commands.
	//
	// Delivery (text, tool events) does NOT route through Turn — it
	// routes through the SessionEvents installed on the backend via
	// AttachSessionEvents, which lives for the session's lifetime. Turn
	// is strictly bookkeeping: turn completion, post-tool nudges,
	// pre-answer gate.
	//
	// Used by ccstream. cctmux still uses the legacy Handler field; the
	// two are mutually exclusive within a single Inject call.
	Turn *TurnEvents

	// Handler is the legacy combined per-turn EventHandler. Used by
	// cctmux which still threads delivery + bookkeeping through one
	// pointer via its session JSONL watcher. New code should use Turn
	// (per-turn bookkeeping) plus AttachSessionEvents (session-scoped
	// delivery). Will be removed once cctmux is migrated.
	//
	// Deprecated: use Turn + AttachSessionEvents.
	Handler *EventHandler
}

// TurnUsage holds token counts from a completed backend turn,
// extracted from the session JSONL's usage payload.
type TurnUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

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
// via SendToPane and receives streaming events back.
type Delegator interface {
	// Start launches the coding agent subprocess.
	// Called once during agent setup. The backend should be ready to
	// accept turns after Start returns.
	Start(ctx context.Context, opts StartOptions) error

	// SendToPane sends a composed prompt to the coding agent and streams
	// events back via the handler. May return before the turn completes
	// (implementation-dependent). Use WaitForTurn to block until the
	// turn finishes.
	SendToPane(ctx context.Context, prompt string, handler *EventHandler) (*TurnResult, error)

	// WaitForTurn blocks until the next turn completion (stop_reason
	// "end_turn" in the session output). Returns immediately if no turn
	// is in progress. Respects context cancellation/deadline.
	WaitForTurn(ctx context.Context) error

	// IsTurnInFlight reports whether a turn callback is registered but
	// hasn't fired yet. Used by RunInference to detect steered follow-up
	// messages that should be pasted into the pane without creating a
	// new turn pipeline (CC treats them as part of the same turn).
	IsTurnInFlight() bool

	// SendCommand sends a slash command or steered message directly to the
	// agent (e.g. "/compact ...", "/model opus", or a user redirect).
	// These bypass Foci's prompt composition — they're raw commands sent
	// verbatim.
	//
	// priority controls CC's processing order: "now" interrupts the
	// current operation (tool execution), "next" queues after the current
	// turn, "later" defers. Empty string omits priority (CC defaults to
	// "next"). Backends that don't support priority (e.g. tmux) ignore it.
	SendCommand(ctx context.Context, command string, priority string) error

	// IsRunning reports whether the agent subprocess is alive.
	IsRunning() bool

	// Restart kills and relaunches the agent subprocess.
	Restart(ctx context.Context) error

	// SetPermissionPromptFunc sets the function used to send permission
	// prompts with inline keyboard choices. Optional — if not set, the
	// backend logs and drops undeliverable prompts.
	SetPermissionPromptFunc(fn PermissionPromptFunc)

	// SetOnPermissionCleared sets a callback fired when a permission prompt
	// is resolved (user responded, CC timed out, or cancelled).
	// Used by DelegatedManager to unblock WaitForPermission.
	SetOnPermissionCleared(fn func())

	// SetOnSessionReady sets a callback fired once when the backend
	// discovers its session ID. Used to persist the ID for resume-after-restart.
	SetOnSessionReady(fn func(sessionID string))

	// SetTypingFunc sets a callback to control the platform's typing indicator.
	// Called with true when the backend starts working (SendToPane), and false
	// when the turn completes (end_turn). Optional — nil means no typing.
	SetTypingFunc(fn func(typing bool))

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

// AttachmentSender is optionally implemented by backends that support
// structured content blocks (images, documents) alongside text prompts.
// When the delegated transport has attachments, it checks for this interface
// and uses it instead of the text-only SendToPane path.
type AttachmentSender interface {
	// SendToPaneWithAttachments sends a composed prompt with file attachments
	// as structured content blocks. Each attachment becomes an image or document
	// ContentBlock alongside the text prompt.
	SendToPaneWithAttachments(ctx context.Context, prompt string, attachments []Attachment, handler *EventHandler) (*TurnResult, error)
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

// EventHandler receives streaming events during a turn.
// All callbacks are optional — nil callbacks are silently skipped.
//
// Tool events carry the tool_use ID (from the JSONL/stream source) so
// consumers can correlate OnToolEnd with the originating OnToolStart without
// having to match by name or rely on ordering.
type EventHandler struct {
	OnText           func(text string)                             // complete text block from the agent
	OnTextDelta      func(delta string)                            // streaming text delta (content_block_delta)
	OnThinkingDelta  func(delta string)                            // streaming thinking delta (content_block_delta)
	OnToolStart      func(id, name, input string)                  // tool execution began
	OnToolEnd        func(id, name, output string, isError bool)   // tool execution finished
	OnTurnComplete   func(result *TurnResult)                      // turn finished

	// SteerCheckFunc is called by the backend at tool execution boundaries
	// to check for pending steer messages. Non-blocking; returns nil if no
	// steer is pending. If non-nil text is returned, the backend injects
	// it as a "now"-priority user message to interrupt the current tool
	// execution. Used by the delegated transport to drain steer messages
	// buffered by the platform's MessageQueue.
	SteerCheckFunc func() []string

	// PostToolNudgeFunc is called after each tool's completion signal
	// (PostToolUse hook dispatch). The caller returns any nudge reminders
	// that should be injected mid-turn as "now"-priority user messages,
	// matching the API transport's CheckAfterTools path. Non-blocking.
	// Used by the delegated transport to fire every_n_tools and
	// after_error nudge rules during long CC turns.
	PostToolNudgeFunc func(toolName string, isError bool) []string

	// PreAnswerNudgeFunc is called when CC signals end_turn on a result
	// message. The caller returns a follow-up prompt (or "" to let the
	// turn finalise as-is). When non-empty, the backend re-arms a second
	// round under the SAME EventHandler — sends the returned text as a
	// new user message and treats its result as the authoritative one.
	// This wires every nudge rule of type pre_answer into delegated turns,
	// at the cost of the original answer streaming to the user before the
	// revised answer arrives (delegated can't retract a committed reply).
	PreAnswerNudgeFunc func(result *TurnResult) string
}

// TurnResult is the outcome of a completed turn.
type TurnResult struct {
	Text      string     // final response text
	ToolCalls int        // number of tool calls executed during the turn
	Usage     *TurnUsage // token usage (nil if unavailable)
	Model     string     // model used (e.g. "claude-opus-4-6")
}

// TurnUsage holds token counts from a completed backend turn,
// extracted from the session JSONL's usage payload.
type TurnUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

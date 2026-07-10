// Package backend defines the interface for coding agent backends
// (Claude Code, Codex, OpenCode, etc.) that handle entire agent turns
// including inference and tool execution. This is fundamentally different
// from provider.Client, which handles only the inference call while Foci
// executes tools.
package delegator

import (
	"context"
	"errors"
	"time"
)

// ErrTurnNotInFlight is returned by Inject(SourceSteer) when the steer arrives
// after the turn it meant to interrupt has already completed and the inject
// carries no Turn of its own to track a fresh turn. Rather than silently
// beginning an untracked turn (nil TurnEvents → no OnTurnComplete, lost
// usage/compaction), the backend declines and the caller re-routes the message
// through the normal idle path. See the Steer/idle row in Delegator.ImmediateInject.
var ErrTurnNotInFlight = errors.New("delegator: turn not in flight")

// ErrTurnInFlight is returned by Inject(SourceSystem) when a turn is already
// in flight. System-initiated input (foci send, cron, notifications, error and
// restart injections) must never fold into a running turn — only real user
// input may steer. The backend rejects atomically (the idle check and turn
// begin happen under one lock) and the caller waits for turn completion
// (WaitForTurn) before retrying. See the System row in Delegator.ImmediateInject.
var ErrTurnInFlight = errors.New("delegator: turn in flight")

// Backend is the interface that all coding agent backends implement.
// A Backend owns the entire turn: inference, tool execution, and context
// management. Foci sends composed prompts (with metadata, nudges, reminders)
// via ImmediateInject and receives streaming events back.
type Delegator interface {
	// Start launches the coding agent subprocess.
	// Called once during agent setup. The backend should be ready to
	// accept turns after Start returns.
	Start(ctx context.Context, opts StartOptions) error

	// ImmediateInject delivers a user-role event to the backend RIGHT NOW —
	// it writes to the coding agent's input channel immediately, with no
	// serialisation against in-flight turns beyond the per-source routing
	// below. It is the low-level delivery primitive, and the counterpart of
	// the session queue, Agent.Enqueue (internal/agent/inbox.go): Enqueue
	// decides WHEN input may reach the backend (serialised with the
	// session's turns, deferred behind pending asks, held through
	// compaction); ImmediateInject is HOW it gets there once that decision
	// is made.
	//
	// Most code should enqueue, not call this. The queue is the right
	// choice for anything that can wait for the current turn — which is
	// nearly everything, and everything system-initiated. Legitimate direct
	// callers are the turn transport (RunInference: begin-turn and
	// follow-up fold), the inbox's own urgent-steer dispatch, and the
	// slash-command paths (compaction, passthrough). Calling it from
	// anywhere else bypasses the queue's guarantees and re-introduces the
	// mid-turn steering/racing bugs it exists to prevent.
	//
	// The backend dispatches based on inj.Source and the current
	// IsTurnInFlight() state:
	//
	//   Source   | Turn state | Action
	//   ---------|------------|--------------------------------------------
	//   User     | idle       | begin turn (with attachments if provided)
	//   User     | in-flight  | send follow-up; CC's mid-turn drain folds it
	//   Steer    | in-flight  | send at priority "next"; CC folds at next tool boundary
	//   Steer    | idle       | with inj.Turn: begin turn; without: ErrTurnNotInFlight (caller re-routes)
	//   System   | idle       | begin turn, atomically (idle check + begin under one lock)
	//   System   | in-flight  | ErrTurnInFlight — never folds; caller waits and retries
	//   Compact  | any        | send slash command (fire-and-forget)
	//   Pass     | any        | send slash command (fire-and-forget)
	//
	// Returns whatever error the underlying writer/protocol returns. The
	// turn result (when applicable) flows through inj.Turn.OnTurnComplete;
	// delivery (text, tool events) flows through the SessionEvents
	// previously installed via AttachSessionEvents — independent of Turn.
	ImmediateInject(ctx context.Context, inj Inject) error

	// WaitForTurn blocks until the next turn completion (stop_reason
	// "end_turn" in the session output). Returns immediately if no turn
	// is in progress. Respects context cancellation/deadline.
	WaitForTurn(ctx context.Context) error

	// IsTurnInFlight reports whether a turn callback is registered but
	// hasn't fired yet. ImmediateInject consults this to decide between begin-turn
	// and follow-up routing.
	IsTurnInFlight() bool

	// IsRunning reports whether the agent subprocess is alive.
	IsRunning() bool

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

	// CheckReady verifies the backend is functionally ready to run turns and
	// self-heals if it is not. This is distinct from WaitReady, which only
	// waits for an already-started subprocess to present its input prompt.
	// CheckReady is called once at delegated-agent startup, before any turn,
	// and must not assume a running subprocess (it may be called on a freshly
	// constructed, un-Started backend).
	//
	// For the ccstream (Claude Code) backend this means verifying the CLI is
	// authenticated (`claude auth status` → loggedIn); if it is not, CheckReady
	// triggers the interactive re-login flow (posting a login URL to the
	// agent's default chat) and reports ready=false. Backends with no
	// readiness gate (cctmux, whose TUI handles its own login out of band)
	// return (true, nil).
	//
	// ready=false with err==nil means "not ready, recovery has been initiated"
	// (e.g. re-login is now in flight). A non-nil err means the readiness check
	// itself could not be performed (e.g. the auth-status probe failed to run);
	// callers should log it but must not treat an indeterminate probe as
	// not-authenticated (avoids spuriously launching a login flow).
	CheckReady(ctx context.Context) (ready bool, err error)

	// StatusDetail returns backend-specific status text for /status display
	// (e.g. CC's permission mode). Appended to the common backend info
	// (liveness, last-event, session) by DelegatedManager.BackendInfo.
	// Empty string means no additional detail.
	StatusDetail() string

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

// AutonomousRunAwaiter is optionally implemented by backends that track
// background work (Agent-tool subagents, run_in_background Bash) and autonomous
// runs. The inbox consults it to hold system injects across the whole
// background-work lifetime — from the spawn until the resulting autonomous run
// completes — not just while a run is visibly active (spec §4). Backends that
// don't track this (cctmux, opencode) don't implement it and are treated as
// never awaiting.
type AutonomousRunAwaiter interface {
	// AwaitingAutonomousRun reports whether a delivering autonomous run is
	// active, pending (a spawned background task not yet completed), or
	// imminently expected (within the post-run chain grace).
	AwaitingAutonomousRun() bool
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

	// Toggle, when non-nil, makes this a NON-TERMINAL button: pressing it
	// toggles ExtraBody in/out of the message (e.g. show/hide a proposed
	// diff) and re-renders the prompt in place instead of resolving it. The
	// Allow/Deny buttons stay live. Platforms that can't re-render in place
	// (currently the native app) omit toggle buttons entirely. See
	// platform.ButtonToggle and platform.HandleInteractiveCallback.
	Toggle *PromptToggle
}

// PromptToggle carries the collapsible content and the two labels for a
// non-terminal toggle button (see PromptChoice.Toggle).
type PromptToggle struct {
	ExtraBody string // appended to the prompt body when shown
	ShowLabel string // button label while hidden (e.g. "Show diff")
	HideLabel string // button label while shown (e.g. "Hide diff")
}

// PermissionPromptFunc sends an interactive prompt to the user with keyboard
// choices. Used for both permission requests and AskUserQuestion prompts.
// requestID is the CC protocol request ID (empty for tmux backends).
// summary is a short description for post-action display (e.g.
// "Edit memory/2026-03-27.md"). If nil, the backend falls back to plain text.
// attachmentPath, when non-empty, is a file the platform layer should send as
// a document before drawing the keyboard (e.g. the full plan markdown for an
// ExitPlanMode prompt — see ccstream handleToolRequest). Empty for the common
// case; the platform layer ignores it then.
type PermissionPromptFunc func(requestID, text, summary, attachmentPath string, choices []PromptChoice)

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

// PlanResponder is optionally implemented by backends that support cancelling a
// pending ExitPlanMode (plan-approval) permission in favour of typed revision
// feedback. Unlike a normal tool permission — which a follow-up message queues
// behind — a plan request is cancelled by the message: the text is delivered to
// the model as rejection feedback so it revises the plan and re-presents it.
// ccstream implements this; other backends don't.
type PlanResponder interface {
	// HasPendingPlanPermission returns the request ID of a pending ExitPlanMode
	// permission, or "" if none.
	HasPendingPlanPermission() string

	// CancelPlanWithFeedback denies the plan permission, passing feedback to the
	// backend as the rejection message and editing the prompt's buttons away.
	CancelPlanWithFeedback(requestID, feedback string) error
}

// ContextWindow holds the model's context window size and, when the backend
// reports one, its own self-computed usage breakdown. Returned by backends
// that can look up the real limit (e.g. opencode's /config/providers, CC's
// get_context_usage control request).
//
// The HEADER token total shown by /context comes from api.db (QuerySessionStats),
// which is durable and survives a restart — NOT from TotalTokens here, which is
// in-memory backend state cleared at turn boundaries. Categories is the only
// source of the per-section breakdown though (CC computes it locally, exactly),
// so it drives /context's breakdown block; empty means the backend can't break
// it down (e.g. opencode) and /context degrades to "data unavailable".
type ContextWindow struct {
	MaxTokens   int               // total context window size for the current model
	Model       string            // model name (e.g. "claude-sonnet-4-6")
	TotalTokens int               // backend's in-memory used-token count (0 after restart)
	Categories  []ContextCategory // per-section breakdown, when the backend reports one
}

// ContextCategory is a single row in a backend's context-usage breakdown, e.g.
// "System prompt" / "Messages" / "Free space" with its token count.
type ContextCategory struct {
	Name   string
	Tokens int
}

// ContextWindowQuerier is optionally implemented by backends that can look up
// the current model's real context window size. Cheap and fast — no API call.
type ContextWindowQuerier interface {
	GetContextWindow(ctx context.Context) (*ContextWindow, error)
}

// BackendCapabilities is optionally implemented by backends to advertise
// which nudge delivery mechanisms they support. Backends that don't
// implement this interface are assumed to support everything (backward
// compat for cctmux or any hypothetical backend).
type BackendCapabilities interface {
	Capabilities() Capabilities
}

// Capabilities describes which mid-turn injection mechanisms the backend
// supports. Turn-start nudges (every_n_turns, regex) work on all backends
// because they're prepended to the prompt before the turn begins; the
// fields here gate only the mid-turn mechanisms.
type Capabilities struct {
	// PostToolNudge indicates the backend can inject messages at tool
	// boundaries mid-turn. Required for every_n_tools, after_error, and
	// tool_pattern trigger types.
	PostToolNudge bool

	// PreAnswerNudge indicates the backend can inject a message before
	// the model returns its final answer. Required for pre_answer trigger.
	PreAnswerNudge bool
}

// BackendBrancher is optionally implemented by backends that can fork their
// underlying conversation session into a new, independent backend session —
// i.e. the backend "can branch". A foci branch key backed by such a fork
// starts its first delegated turn with the PARENT's full backend context,
// instead of an empty session (BranchIndependent) or a shared in-place turn.
//
// Backends that don't implement this interface are treated as unable to
// branch; the branch machinery falls back to the existing behaviour. The
// method is a pure fork operation and MUST NOT require a started/running
// backend — callers may invoke it on a freshly-constructed (unstarted)
// backend instance, since the fork is performed against on-disk session
// state, not a live process.
type BackendBrancher interface {
	ForkSession(ctx context.Context, req ForkRequest) (ForkResult, error)
}

// ForkRequest identifies the backend conversation to fork.
type ForkRequest struct {
	// ParentSessionID is the backend session id to fork (for CC, its UUID).
	ParentSessionID string
	// WorkDir is the agent workspace (cwd); backends that key session storage
	// by cwd (CC's ~/.claude/projects/<slug>/) need it to locate the session.
	WorkDir string
	// TruncateAfter, when >0, forks only the first N messages of the parent
	// conversation (a mid-conversation branch). 0 forks the whole
	// conversation. v1 CC support treats any >0 value as unsupported —
	// reserved; see the backend-branch plan's "Deferred" section.
	TruncateAfter int
}

// ForkResult carries the new backend session id, ready to be resumed.
type ForkResult struct {
	// SessionID is the new backend session id (for CC, a fresh UUID whose
	// on-disk session is a copy of the parent's, ready for --resume).
	SessionID string
}

// SkipPermissions reports whether the backend config disables the permission
// prompt flow entirely (CC's --dangerously-skip-permissions). The single
// accessor for every reader — backend launch args (ccstream/cctmux) and the
// environment block's Command Approval gate — so the system prompt can never
// describe an approval regime the backend isn't actually enforcing.
func SkipPermissions(cfg map[string]any) bool {
	v, ok := cfg["skip_permissions"].(bool)
	return ok && v
}

// StartOptions configures the backend at launch time.
type StartOptions struct {
	WorkDir          string            // agent workspace directory (becomes cwd)
	SystemPrompt     string            // concatenated character/system files (static fallback; see SystemPromptFunc)
	Model            string            // initial model (e.g. "opus", "sonnet")
	AgentID          string            // foci agent ID
	Label            string            // unique label for this instance (used for tmux window naming); falls back to AgentID
	ResumeSessionID  string            // resume a previous CC session (e.g. --resume <uuid>); empty = new session
	SessionKey       string            // foci session key — used by exec bridge tools for routing (e.g. send_to_chat)
	ExecRegistry     any               // *tools.Registry — if set, used by DelegatedManager to create exec bridges
	Env              map[string]string // extra environment variables to inject (e.g. BASH_ENV, FOCI_SOCK from exec bridge)
	TmuxCols         int               // tmux window width (0 = use tools.tmux_cols default)
	TmuxRows         int               // tmux window height (0 = use tools.tmux_rows default)
	AutoApproveRules []string          // foci-level auto-approve patterns (e.g. "Bash:git *", "Read")
	SubagentMaxAge   time.Duration     // prune threshold for tracked background tasks (0 = tracker default 30m); from [cc_backend].background_task_max_age

	// ClaudeBinary overrides the path to the `claude` executable that
	// delegated/RunOnce launches. Empty = use "claude" (resolved via
	// $PATH). Folded from [cc_backend].claude_binary by
	// cmd/foci-gw/agents_delegated.go. Used by integration tests to
	// point at bin/cc-stub.
	ClaudeBinary string

	// SystemPromptFunc, when non-nil, is the single per-session prompt
	// generator: called at each session Start with the session key to produce
	// a fresh system prompt from disk (character/skill edits) AND resolve the
	// session's platform block. The manager resolves it once into SystemPrompt
	// before Start, so every backend consumes SystemPrompt identically — none
	// re-resolves. Its result (when non-empty) takes precedence over the static
	// SystemPrompt. See #828 / #706.
	SystemPromptFunc func(sessionKey string) string

	// Effort is the effort level to apply at launch (e.g. "high", "max").
	// Backends that support it (ccstream → `claude --effort <level>`) inject
	// it so the level survives a session bounce — apply_flag_settings is
	// runtime-only and resets to the model default on relaunch. Empty or
	// "off" means inject nothing (CC uses the model default). (#840)
	Effort string

	// EffortFunc, when non-nil, is called at each session Start with the
	// session key to resolve the launch effort fresh — mirroring
	// SystemPromptFunc but parameterized by session. Reading it per-start
	// (rather than freezing Effort at setup) keeps a post-/effort bounce on
	// the latest level and lets each session carry its own. Its result
	// populates Effort for that Start. (#840)
	EffortFunc func(sessionKey string) string

	// CompactionPromptFunc, when non-nil, is called at each session Start to
	// resolve the compaction summary prompt fresh from disk (mirroring
	// SystemPromptFunc). Backends that drive their OWN compaction (opencode's
	// internal /summarize) use it to make that summary follow foci's
	// compaction-summary.md instead of the backend's built-in template — via
	// the blank-system plugin's session.compacting hook. Empty result / nil =
	// leave the backend's default compaction prompt untouched.
	CompactionPromptFunc func(sessionKey string) string
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
	OnSubagentStart func(groupKey, label string)                // a subagent run began (PreToolUse hook for the Agent tool); label = agent description
	OnSubagentText  func(groupKey, text string)                 // raw text block from a subagent (Task tool); groupKey = parent tool_use id
	OnSubagentEnd   func(groupKey string)                       // a subagent run (groupKey) completed (its Agent tool_use resolved)
	OnTextDelta     func(delta string)                          // streaming text delta (content_block_delta)
	OnThinkingDelta func(delta string)                          // streaming thinking delta (content_block_delta)
	OnToolStart     func(id, name, input string)                // tool execution began
	OnToolEnd       func(id, name, output string, isError bool) // tool execution finished
}

// TurnEvents are the per-turn bookkeeping callbacks. Set when a turn begins
// via ImmediateInject, cleared on OnResult. May be nil between turns; backend must
// tolerate that. These are bookkeeping only — delivery (text, tool events)
// flows through SessionEvents regardless.
type TurnEvents struct {
	// OnTurnComplete fires once when the turn finishes. The backend
	// captures-then-nils TurnEvents under turnMu in OnResult so this
	// invariant holds by construction (no counters needed).
	OnTurnComplete func(result *TurnResult)

	// PostToolNudgeFunc is called after each tool's completion signal
	// (PostToolUse hook dispatch). The caller returns any nudge reminders
	// that should be injected mid-turn as default-priority user messages,
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
// current IsTurnInFlight() state — see Delegator.ImmediateInject for the
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
	// text at queue priority "next" — CC's mid-turn drain folds it into
	// the current ask at the next tool boundary; the running tool
	// finishes and the model responds in the same turn. Steer does not
	// interrupt: priority "now" (which aborts the in-flight ask) is
	// reserved for a future per-message steer tag or aggressive-steer
	// config mode (both NYI); "stop right now" semantics live in /reset
	// hard. At idle, degrades to SourceUser-idle (begin turn).
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

	// SourceSystem is text that must never fold into an in-flight turn:
	// system-initiated input (foci send / HTTP /send, cron keepalives,
	// webhooks, inter-session notifies, error and restart notifications) and
	// user messages the sender explicitly marked "queue" (agent.SteerNever).
	// At idle it begins a new turn exactly like SourceUser (the idle check
	// and turn begin are atomic, so two racing injects cannot clobber each
	// other's turn bookkeeping); in flight it returns ErrTurnInFlight and
	// the caller waits gracefully for turn completion before retrying.
	SourceSystem
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
	case SourceSystem:
		return "system"
	default:
		return "unknown"
	}
}

// Inject describes a user-role event delivered to the backend via
// Delegator.ImmediateInject. See InjectSource for source-specific routing.
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
	// begins. Required for SourceUser / SourceSteer / SourceSystem at idle
	// when the caller needs OnTurnComplete fired; ignored for in-flight
	// injections (the existing TurnEvents persists) and for slash commands.
	//
	// Delivery (text, tool events) does NOT route through Turn — it
	// routes through the SessionEvents installed on the backend via
	// AttachSessionEvents, which lives for the session's lifetime. Turn
	// is strictly bookkeeping: turn completion, post-tool nudges,
	// pre-answer gate.
	//
	// Used by both ccstream and cctmux. Delivery (text, tool events) does NOT
	// route through Turn — it routes through the SessionEvents installed via
	// AttachSessionEvents, which lives for the session's lifetime.
	Turn *TurnEvents
}

// TurnUsage holds token counts from a completed backend turn,
// extracted from the session JSONL's usage payload.
type TurnUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

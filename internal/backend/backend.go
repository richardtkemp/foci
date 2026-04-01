// Package backend defines the interface for coding agent backends
// (Claude Code, Codex, OpenCode, etc.) that handle entire agent turns
// including inference and tool execution. This is fundamentally different
// from provider.Client, which handles only the inference call while Foci
// executes tools.
package backend

import (
	"context"
	"time"
)

// Backend is the interface that all coding agent backends implement.
// A Backend owns the entire turn: inference, tool execution, and context
// management. Foci sends composed prompts (with metadata, nudges, reminders)
// via SendToPane and receives streaming events back.
type Backend interface {
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

	// SendCommand sends a slash command directly to the agent
	// (e.g. "/compact ...", "/model opus"). These bypass Foci's prompt
	// composition — they're raw commands sent verbatim.
	SendCommand(ctx context.Context, command string) error

	// IsRunning reports whether the agent subprocess is alive.
	IsRunning() bool

	// Restart kills and relaunches the agent subprocess.
	Restart(ctx context.Context) error

	// SetReplyFunc sets the function used to deliver text to the user's
	// platform chat. Called when the session/connection is known. The backend
	// uses this for all asynchronous output (streamed responses, etc.).
	SetReplyFunc(fn ReplyFunc)

	// SetPermissionPromptFunc sets the function used to send permission
	// prompts with inline keyboard choices. Optional — if not set, the
	// backend falls back to plain text via ReplyFunc.
	SetPermissionPromptFunc(fn PermissionPromptFunc)

	// SetOnPermissionCleared sets a callback fired when a permission prompt
	// disappears from the TUI (user responded, CC timed out, or Escape).
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

// ReplyFunc sends text to the user's platform chat. Set by the agent layer
// so the backend can deliver asynchronous output (session file watcher events,
// permission prompts, etc.) without depending on per-turn context.
type ReplyFunc func(text string)

// PromptChoice represents a choice in a permission prompt.
type PromptChoice struct {
	Label string // button text (e.g. "Yes", "No")
	Data  string // value to send to the pane (e.g. "1", "2")
}

// PermissionPromptFunc sends a permission prompt to the user with keyboard
// choices. requestID is the CC protocol request ID (empty for tmux backends).
// summary is a short description for post-approval display (e.g.
// "Edit memory/2026-03-27.md"). If nil, the backend falls back to plain text.
type PermissionPromptFunc func(requestID, text, summary string, choices []PromptChoice)

// StartOptions configures the backend at launch time.
type StartOptions struct {
	WorkDir         string // agent workspace directory (becomes cwd)
	SystemPrompt    string // concatenated character/system files
	Model           string // initial model (e.g. "opus", "sonnet")
	AgentID         string // foci agent ID
	Label           string // unique label for this instance (used for tmux window naming); falls back to AgentID
	ResumeSessionID string // resume a previous CC session (e.g. --resume <uuid>); empty = new session
	SessionKey      string // foci session key — used by exec bridge tools for routing (e.g. send_to_chat)
	ExecRegistry    any    // *tools.Registry — if set, creates a persistent exec bridge for foci shell commands
	TmuxCols        int    // tmux window width (0 = use tools.tmux_cols default)
	TmuxRows        int    // tmux window height (0 = use tools.tmux_rows default)
}

// EventHandler receives streaming events during a turn.
// All callbacks are optional — nil callbacks are silently skipped.
type EventHandler struct {
	OnText         func(text string)                                    // new text content from the agent
	OnToolStart    func(name string, input string)                      // tool execution began
	OnToolEnd      func(name string, output string, isError bool)       // tool execution finished
	OnTurnComplete func(result *TurnResult)                             // turn finished
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

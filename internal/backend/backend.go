// Package backend defines the interface for coding agent backends
// (Claude Code, Codex, OpenCode, etc.) that handle entire agent turns
// including inference and tool execution. This is fundamentally different
// from provider.Client, which handles only the inference call while Foci
// executes tools.
package backend

import "context"

// Backend is the interface that all coding agent backends implement.
// A Backend owns the entire turn: inference, tool execution, and context
// management. Foci sends composed prompts (with metadata, nudges, reminders)
// via SendTurn and receives streaming events back.
type Backend interface {
	// Start launches the coding agent subprocess.
	// Called once during agent setup. The backend should be ready to
	// accept turns after Start returns.
	Start(ctx context.Context, opts StartOptions) error

	// SendTurn sends a composed prompt to the coding agent and streams
	// events back via the handler. Blocks until the turn completes.
	// The prompt includes Foci's metadata, nudges, reminders, etc.
	SendTurn(ctx context.Context, prompt string, handler *EventHandler) (*TurnResult, error)

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

	// SendKeystroke sends a single keypress to the agent's TUI.
	// Used for permission prompt responses where paste+Enter doesn't work.
	SendKeystroke(ctx context.Context, key string) error

	// SessionID returns the coding agent's session identifier (e.g. CC's UUID).
	// Used to resume sessions after idle shutdown. Empty if unknown.
	SessionID() string

	// Close shuts down the agent subprocess gracefully.
	Close() error
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
// choices. If nil, the backend falls back to sending plain text via ReplyFunc.
type PermissionPromptFunc func(text string, choices []PromptChoice)

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
	Text      string // final response text
	ToolCalls int    // number of tool calls executed during the turn
}

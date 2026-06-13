// Package cctmux implements the delegator.Delegator interface using
// Claude Code running as an interactive process in a tmux pane.
// Input is sent via tmux send-keys; output is read by tailing
// Claude Code's session JSONL file.
package cctmux

import (
	"context"
	"sync"
	"sync/atomic"

	"foci/internal/delegator"
)

func init() {
	delegator.Register("claude-code-tmux", newFromConfig)
}

func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{cfg: cfg, preSendOffset: -1}
	if v, ok := cfg["socket_path"].(string); ok {
		b.socketPath = v
	}
	return b, nil
}

// Backend drives Claude Code as a subprocess in a tmux pane.
type Backend struct {
	cfg        map[string]any
	socketPath string       // tmux socket override (empty = default)
	tmuxExec   tmuxExecFunc // tmux subprocess runner injected into panes (nil = real tmux)

	mu              sync.Mutex
	pane            *tmuxPane
	watcher         *sessionWatcher
	watcherStarting bool // true while ensureWatcher is running (prevents concurrent discovery)
	watchCtx        context.Context
	watchStop       context.CancelFunc
	sessionID       string // CC session UUID
	agentID         string // foci agent ID
	workDir         string // workspace directory

	// sessionEvents holds the session-scoped delivery callbacks installed via
	// AttachSessionEvents. Stored in atomic.Pointer so the watcher goroutine
	// reads them lock-free; set once per session (idempotent re-attach), never
	// nil after the first attach so delivery never drops between turns.
	sessionEvents atomic.Pointer[delegator.SessionEvents]

	// replyMu guards the callback setters below.
	replyMu        sync.Mutex
	permPromptFunc delegator.PermissionPromptFunc
	onSessionReady func(string) // called once when session ID is discovered
	typingFunc     func(bool)   // typing indicator: true=start, false=stop

	// lastPrompt tracks the last permission prompt sent to avoid duplicates.
	// permissionActive tracks whether a prompt is currently displayed, so we
	// can detect when it disappears (CC timeout, user response, Escape).
	lastPromptMu     sync.Mutex
	lastPrompt       string
	permissionActive bool
	onPermCleared    func() // called when permission prompt disappears

	// waitMu guards waitCh. WaitForTurn creates a channel; the watcher's
	// OnTurnComplete callback signals it. One waiter at a time.
	waitMu sync.Mutex
	waitCh chan struct{}

	// turnMu guards turnEvents — the current turn's per-turn bookkeeping
	// (OnTurnComplete, nudges). Installed by sendToPane when a turn begins,
	// captured-and-nil'd by fireTurnComplete on end_turn (one-shot). Nil
	// between turns; the backend tolerates that.
	turnMu     sync.Mutex
	turnEvents *delegator.TurnEvents

	// preSendOffset records the JSONL file size before sendText, so the
	// watcher starts from there and catches responses written before it starts.
	// -1 means "use end of file" (default, for resumed sessions with existing watcher).
	preSendOffset int64
}

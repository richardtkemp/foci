// Package claudecode implements the backend.Backend interface using
// Claude Code running as an interactive process in a tmux pane.
// Input is sent via tmux send-keys; output is read by tailing
// Claude Code's session JSONL file.
package claudecode

import (
	"context"
	"sync"

	"foci/internal/backend"
	"foci/internal/tools"
)

func init() {
	backend.Register("claude-code", newFromConfig)
}

func newFromConfig(cfg map[string]any) (backend.Backend, error) {
	b := &Backend{cfg: cfg, preSendOffset: -1}
	if v, ok := cfg["socket_path"].(string); ok {
		b.socketPath = v
	}
	return b, nil
}

// Backend drives Claude Code as a subprocess in a tmux pane.
type Backend struct {
	cfg        map[string]any
	socketPath string // tmux socket override (empty = default)

	mu             sync.Mutex
	pane           *tmuxPane
	watcher        *sessionWatcher
	watchCtx       context.Context
	watchStop      context.CancelFunc
	sessionID string // CC session UUID
	agentID        string                // foci agent ID
	workDir        string                // workspace directory
	bridge         *tools.ExecBridge     // persistent exec bridge for foci shell commands; nil if not configured

	// replyFunc delivers text to the user's platform chat.
	replyMu            sync.Mutex
	replyFunc          func(string)
	permPromptFunc     func(string, string, []backend.PromptChoice)
	onSessionReady     func(string)     // called once when session ID is discovered
	typingFunc         func(bool)       // typing indicator: true=start, false=stop

	// lastPrompt tracks the last permission prompt sent to avoid duplicates.
	lastPromptMu sync.Mutex
	lastPrompt   string

	// waitMu guards waitCh. WaitForTurn creates a channel; the watcher's
	// OnTurnComplete callback signals it. One waiter at a time.
	waitMu sync.Mutex
	waitCh chan struct{}

	// turnCompleteMu guards turnCompleteFn. Set by SendTurn from the
	// per-turn EventHandler; fired once by the watcher on end_turn, then nil'd.
	turnCompleteMu sync.Mutex
	turnCompleteFn func(*backend.TurnResult)

	// preSendOffset records the JSONL file size before sendText, so the
	// watcher starts from there and catches responses written before it starts.
	// -1 means "use end of file" (default, for resumed sessions with existing watcher).
	preSendOffset int64
}

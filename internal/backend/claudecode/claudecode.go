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
	b := &Backend{cfg: cfg}
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
	replyMu        sync.Mutex
	replyFunc      func(string)
	permPromptFunc func(string, string, []backend.PromptChoice)

	// lastPrompt tracks the last permission prompt sent to avoid duplicates.
	lastPromptMu sync.Mutex
	lastPrompt   string
}

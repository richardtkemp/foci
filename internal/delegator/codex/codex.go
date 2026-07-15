// Package codex implements an OpenAI Codex CLI backend using the
// `codex app-server` JSON-RPC 2.0 protocol over stdio.
//
// The app-server is a persistent subprocess: foci launches it once, performs
// an initialize handshake, starts a thread, and drives turns via turn/start.
// Events (agent messages, tool calls, approvals) arrive as JSON-RPC
// notifications and server-initiated requests on the same stdout stream.
//
// This is architecturally closest to ccstream (persistent subprocess +
// structured stdin/stdout protocol) rather than opencode (HTTP server).
package codex

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

func init() {
	delegator.Register("codex", newFromConfig, true)
}

func newFromConfig(cfg map[string]any) (delegator.Delegator, error) {
	b := &Backend{}
	b.cfg = cfg
	b.lg = log.NewComponentLogger("codex")
	return b, nil
}

// Backend implements delegator.Delegator using OpenAI Codex's app-server
// JSON-RPC 2.0 protocol over stdio.
type Backend struct {
	cfg     map[string]any
	workDir string
	agentID string
	label   string

	lg *log.ComponentLogger

	// Process
	cmd     *exec.Cmd
	writer  *Writer
	cancel  context.CancelFunc
	done    chan struct{}

	// State
	mu         sync.Mutex
	running    bool
	closing    bool
	threadID   string // Codex thread ID; from thread/start response
	readyCh    chan struct{}
	readyOnce  sync.Once
	startOpts  delegator.StartOptions

	// JSON-RPC request/response correlation
	rpcMu       sync.Mutex
	rpcSeq      int64
	pendingRPC  map[int64]chan json.RawMessage

	// Session-scoped delivery callbacks
	sessionEvents atomic.Pointer[delegator.SessionEvents]

	// Turn state
	turnMu       sync.Mutex
	turnActive   bool
	turnEvents   *delegator.TurnEvents
	turnResultCh chan *delegator.TurnResult
	turnText     strings.Builder
	turnTools    int
	stashedUsage *delegator.TurnUsage

	// Activity tracking
	lastActivity atomic.Int64

	// Callbacks (set before Start, read-only after)
	permPromptFn     delegator.PermissionPromptFunc
	onSessionReady   func(sessionID string)
	typingFunc       func(typing bool)
	onPromptsCleared func()
	onWarning        func(detail string) // fired for configWarning / runtime warnings → delivered to chat

	// Approvals
	permMu        sync.Mutex
	pendingPerms  map[int64]*pendingApproval

	// Compaction
	compactMu       sync.Mutex
	compactDoneCh   chan struct{}

	// Pending control overrides (set by ControlSender, applied on next turn/start)
	pendingModel   string
	pendingApproval string
	contextWindow   int
}

// pendingApproval tracks an outstanding server-initiated approval request.
type pendingApproval struct {
	rpcID   int64
	itemID  string
	command string
}

// IsRunning reports whether the Codex app-server subprocess is alive.
func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// IsTurnInFlight reports whether a turn callback is registered but hasn't
// fired yet.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnActive
}

// SessionID returns the Codex thread ID.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.threadID
}

// SessionFilePath returns the on-disk path of this session's transcript.
func (b *Backend) SessionFilePath() string {
	b.mu.Lock()
	tid := b.threadID
	b.mu.Unlock()
	if tid == "" {
		return ""
	}
	home, err := userHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.codex/sessions/"
}

// touchActivity records the current time as the most recent event.
func (b *Backend) touchActivity() {
	b.lastActivity.Store(time.Now().UnixNano())
}

// LastActivity returns the time of the most recent event from Codex.
func (b *Backend) LastActivity() time.Time {
	ns := b.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (b *Backend) StatusDetail() string {
	return "sandbox=" + b.sandboxMode()
}

func (b *Backend) sandboxMode() string {
	if v, ok := b.cfg["sandbox"].(string); ok && v != "" {
		return v
	}
	return "workspace-write"
}

func (b *Backend) codexBinary() string {
	if v, ok := b.cfg["binary"].(string); ok && v != "" {
		return v
	}
	return "codex"
}

func (b *Backend) WaitForTurn(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}


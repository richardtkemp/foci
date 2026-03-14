package tools

import (
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/state"
)

var tmuxCounter uint64

// tmuxSocketPath overrides the tmux socket (via -S). Empty = default.
// Tests set this to isolate from the user's tmux server.
var tmuxSocketPath string

// watchedSession tracks a tmux session being monitored for inactivity
type watchedSession struct {
	session         string
	window          int
	threshold       time.Duration
	lastContent     [16]byte // md5 hash
	lastActivity    time.Time
	notifier        *AsyncNotifier
	agentSessionKey string // agent session that started the watch (for async delivery)
	autopilot       bool   // auto-unwatch after inactivity notification
	conditional     bool   // wait for activity before starting inactivity timer
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
}

// persistedWatch is the JSON-serializable form of a watch for state persistence.
type persistedWatch struct {
	Session         string `json:"session"`
	Window          int    `json:"window"`
	ThresholdSecs   int    `json:"threshold_secs"`
	AgentSessionKey string `json:"agent_session_key"`
	Conditional     bool   `json:"conditional,omitempty"`
}

// sendMinGap is the minimum time between consecutive sends to the same session.
const sendMinGap = 2 * time.Second

// tmuxInstance holds per-tool-instance state so each agent gets isolated tmux sessions.
type tmuxInstance struct {
	mu                sync.Mutex
	watched           map[string]*watchedSession // key: "session:window"
	owned             map[string]string          // tmux session name → owning agent session key
	notifier          *AsyncNotifier
	cols              int
	rows              int
	autopilot         bool // auto-unwatch on inactivity, auto-watch on send
	watchThresholdSec int  // default watch threshold in seconds from config
	stateStore        *state.Store // nil = no persistence
	stateKey          string       // key prefix for persisted owned sessions
	watchStateKey     string       // key for persisted watches (stateKey + ":watches")
	sendMu            sync.Mutex
	lastSend          map[string]time.Time // session name → last send timestamp
	lastAccess        map[string]time.Time // tmux session name → last interaction time
	sessionTTL        time.Duration        // auto-kill idle tmux sessions (0 = disabled)
	reaperCancel      context.CancelFunc   // cancels the TTL reaper goroutine
	socketPath        string               // tmux socket path (empty = default)
}

// NewTmuxTool creates a tmux tool. cols and rows set the default window size
// applied via resize-window after session creation. notifier delivers messages
// when a watched session exceeds its inactivity threshold (nil disables).
// Each call returns an independent tool instance with its own session tracking.
// stateStore and stateKey enable persistence of owned sessions across restarts.
// autopilot enables auto-unwatch on inactivity and auto-watch on send.
// watchThresholdSec sets the default watch threshold in seconds.
// sessionTTL sets the auto-kill TTL for idle tmux sessions (0 disables).
// The returned cleanup function clears all watches and owned sessions (used by
// the tmux memory monitor after kill-server).
func NewTmuxTool(cols, rows int, notifier *AsyncNotifier, stateStore *state.Store, stateKey string, autopilot bool, watchThresholdSec int, sessionTTL time.Duration) (func() int, *Tool, func()) {
	if watchThresholdSec < 1 {
		watchThresholdSec = 30
	}
	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]string),
		notifier:          notifier,
		cols:              cols,
		rows:              rows,
		autopilot:         autopilot,
		watchThresholdSec: watchThresholdSec,
		stateStore:        stateStore,
		stateKey:          stateKey,
		watchStateKey:     stateKey + ":watches",
		lastSend:          make(map[string]time.Time),
		lastAccess:        make(map[string]time.Time),
		sessionTTL:        sessionTTL,
		socketPath:        tmuxSocketPath,
	}

	// Restore owned sessions from persistent state
	if stateStore != nil {
		var ownedMap map[string]string
		if stateStore.Get(stateKey, &ownedMap) {
			for name, sk := range ownedMap {
				inst.owned[name] = sk
				inst.lastAccess[name] = time.Now() // conservative: full TTL window after restart
			}
			if len(ownedMap) > 0 {
				log.Debugf("tmux", "restored %d owned session(s) from state", len(ownedMap))
			}
		}

		// Restore watches from persistent state
		var watches []persistedWatch
		if stateStore.Get(inst.watchStateKey, &watches) && len(watches) > 0 && notifier != nil {
			var restored int
			for _, pw := range watches {
				// Verify the tmux session still exists
				_, err := inst.runTmux(context.Background(), "has-session", "-t", pw.Session)
				if err != nil {
					continue // stale — session no longer exists
				}

				key := fmt.Sprintf("%s:%d", pw.Session, pw.Window)

				// Capture initial content hash to avoid false activity reset on first poll.
				var initialHash [md5.Size]byte
				if initOut, initErr := inst.runTmux(context.Background(), "capture-pane", "-t",
					fmt.Sprintf("%s:%d", pw.Session, pw.Window), "-p"); initErr == nil {
					initialHash = md5.Sum([]byte(normalizePaneContent(initOut))) // #nosec G401
				}

				monCtx, cancel := context.WithCancel(context.Background())
				ws := &watchedSession{
					session:         pw.Session,
					window:          pw.Window,
					threshold:       time.Duration(pw.ThresholdSecs) * time.Second,
					lastActivity:    time.Now(),
					lastContent:     initialHash,
					notifier:        notifier,
					agentSessionKey: pw.AgentSessionKey,
					autopilot:       autopilot,
					conditional:     pw.Conditional,
					ctx:             monCtx,
					cancel:          cancel,
					done:            make(chan struct{}),
				}
				inst.watched[key] = ws
				go tmuxWatchMonitor(ws, inst, key)
				restored++
			}
			// Re-persist to remove stale entries
			if restored != len(watches) {
				inst.persistWatches()
			}
			if restored > 0 {
				log.Debugf("tmux", "restored %d watch(es) from state", restored)
			}
		}
	}

	// Start TTL reaper goroutine if sessionTTL > 0
	if sessionTTL > 0 {
		reaperCtx, reaperCancel := context.WithCancel(context.Background())
		inst.reaperCancel = reaperCancel
		go inst.ttlReaper(reaperCtx)
	}

	return inst.WatchCount, &Tool{
		Name:        "tmux",
		ExecExport:  true,
		Description: "Manage tmux sessions — start, send keys, read pane output, list, kill, watch for inactivity. Sessions persist across agent turns. Sessions are automatically watched when you send to them and unwatched after inactivity is reported. Before killing: is follow-up likely? Loaded context is expensive to rebuild. Ask before killing coding agent sessions.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"operation": {
					"type": "string",
					"enum": ["start", "send", "read", "list", "kill", "watch", "unwatch"],
					"description": "Operation to perform"
				},
				"name": {
					"type": "string",
					"description": "Session name (start, send, read, kill)"
				},
				"command": {
					"type": "string",
					"description": "Command to run in new session (start)"
				},
				"workdir": {
					"type": "string",
					"description": "Working directory (start, optional)"
				},
				"watch": {
					"type": "boolean",
					"description": "Auto-watch for inactivity after start (start, default true). Requires notifier."
				},
				"keys": {
					"type": "string",
					"description": "Keystrokes to send (send). Optional if enter=true (sends bare Enter). For start operation: keys to send after command finishes loading."
				},
				"enter": {
					"type": "boolean",
					"description": "Append Enter after keys (send, default true)"
				},
				"lines": {
					"type": "integer",
					"description": "Lines to capture (read, default 50)"
				},
				"window": {
					"type": "integer",
					"description": "Window index (watch/unwatch, default 0)"
				},
				"threshold_seconds": {
					"type": "integer",
					"description": "Inactivity threshold in seconds (watch, default 30)"
				},
				"raw": {
					"type": "boolean",
					"description": "Return unfiltered output (read, default false). When false, TUI chrome from Claude Code / OpenCode is stripped."
				}
			},
			"required": ["operation"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return inst.execute(ctx, params)
		},
	}, inst.ClearAll
}

// WatchCount returns the number of actively watched tmux sessions.
func (inst *tmuxInstance) WatchCount() int {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return len(inst.watched)
}

// ptrBool dereferences a *bool pointer, returning the value or defaultValue if nil.
func ptrBool(ptr *bool, defaultValue bool) bool {
	if ptr != nil {
		return *ptr
	}
	return defaultValue
}

// intDefault returns the value if greater than zero, otherwise defaultValue.
func intDefault(value, defaultValue int) int {
	if value > 0 {
		return value
	}
	return defaultValue
}

func (inst *tmuxInstance) execute(ctx context.Context, params json.RawMessage) (ToolResult, error) {
	var p struct {
		Operation        string `json:"operation"`
		Name             string `json:"name"`
		Command          string `json:"command"`
		Workdir          string `json:"workdir"`
		Watch            *bool  `json:"watch"`
		Keys             string `json:"keys"`
		Enter            *bool  `json:"enter"`
		Lines            int    `json:"lines"`
		Window           int    `json:"window"`
		ThresholdSeconds int    `json:"threshold_seconds"`
		Raw              bool   `json:"raw"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	switch p.Operation {
	case "start":
		return inst.start(ctx, p.Name, p.Command, p.Workdir, p.Keys, ptrBool(p.Watch, true))
	case "send":
		return inst.send(ctx, p.Name, p.Keys, ptrBool(p.Enter, true))
	case "read":
		return inst.read(ctx, p.Name, intDefault(p.Lines, 50), p.Raw)
	case "list":
		return inst.list(ctx)
	case "kill":
		return inst.kill(ctx, p.Name)
	case "watch":
		return inst.watch(ctx, p.Name, intDefault(p.Window, 0), intDefault(p.ThresholdSeconds, 30), false)
	case "unwatch":
		return inst.unwatch(ctx, p.Name)
	default:
		return ToolResult{}, fmt.Errorf("unknown operation: %q (valid: start, send, read, list, kill, watch, unwatch)", p.Operation)
	}
}

func (inst *tmuxInstance) owns(name, sessionKey string) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	storedKey, ok := inst.owned[name]
	if !ok {
		return false
	}
	// Both empty = backwards compat (context.Background() in tests)
	if storedKey == "" && sessionKey == "" {
		return true
	}
	return storedKey == sessionKey
}

// persistOwned saves the owned sessions map to the state store.
// Must be called with inst.mu held.
func (inst *tmuxInstance) persistOwned() {
	if inst.stateStore == nil {
		return
	}
	owned := make(map[string]string, len(inst.owned))
	for name, sk := range inst.owned {
		owned[name] = sk
	}
	if err := inst.stateStore.Set(inst.stateKey, owned); err != nil {
		log.Warnf("tmux", "persist owned sessions: %v", err)
	}
}

// clearStaleOwned clears all owned sessions and their lastAccess entries.
// Used when no tmux sessions exist at all (e.g. "no server running").
func (inst *tmuxInstance) clearStaleOwned() {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if len(inst.owned) == 0 {
		return
	}
	inst.owned = make(map[string]string)
	inst.lastAccess = make(map[string]time.Time)
	inst.persistOwned()
}

// persistWatches saves the watched sessions map to the state store.
// Must be called with inst.mu held.
func (inst *tmuxInstance) persistWatches() {
	if inst.stateStore == nil {
		return
	}
	watches := make([]persistedWatch, 0, len(inst.watched))
	for _, ws := range inst.watched {
		watches = append(watches, persistedWatch{
			Session:         ws.session,
			Window:          ws.window,
			ThresholdSecs:   int(ws.threshold / time.Second),
			AgentSessionKey: ws.agentSessionKey,
			Conditional:     ws.conditional,
		})
	}
	if err := inst.stateStore.Set(inst.watchStateKey, watches); err != nil {
		log.Warnf("tmux", "persist watches: %v", err)
	}
}

// cancelWatchesForSession cancels and removes all watches whose key starts with
// "name:". Acquires inst.mu internally, persists state if anything changed,
// and waits for each monitor goroutine to exit.
func (inst *tmuxInstance) cancelWatchesForSession(name string) {
	inst.mu.Lock()
	prefix := name + ":"
	var toCancel []*watchedSession
	for key, ws := range inst.watched {
		if strings.HasPrefix(key, prefix) {
			toCancel = append(toCancel, ws)
			delete(inst.watched, key)
		}
	}
	if len(toCancel) > 0 {
		inst.persistWatches()
	}
	inst.mu.Unlock()

	for _, ws := range toCancel {
		ws.cancel()
	}
}

// runTmuxWithSocket executes a tmux command using the given socket path.
func runTmuxWithSocket(ctx context.Context, socket string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if socket != "" {
		args = append([]string{"-S", socket}, args...)
	}
	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	// Setsid puts the tmux process in its own session so it (and the tmux
	// server it may spawn) won't be killed when the parent process group
	// is cleaned up. Also drops supplementary groups (foci-secrets).
	cmd.SysProcAttr = ChildSysProcAttrSetsid()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runTmux executes a tmux command using the global socket path.
// Used by free functions (memory monitor) and test helpers.
func runTmux(ctx context.Context, args ...string) (string, error) {
	return runTmuxWithSocket(ctx, tmuxSocketPath, args...)
}

// runTmux executes a tmux command using the instance's socket path.
func (inst *tmuxInstance) runTmux(ctx context.Context, args ...string) (string, error) {
	return runTmuxWithSocket(ctx, inst.socketPath, args...)
}

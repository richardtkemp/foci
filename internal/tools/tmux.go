package tools

import (
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"foci/internal/log"
	"foci/prompts"
	"foci/internal/state"
	"foci/internal/display"
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
				_, err := runTmux(context.Background(), "has-session", "-t", pw.Session)
				if err != nil {
					continue // stale — session no longer exists
				}

				key := fmt.Sprintf("%s:%d", pw.Session, pw.Window)

				// Capture initial content hash to avoid false activity reset on first poll.
				var initialHash [md5.Size]byte
				if initOut, initErr := runTmux(context.Background(), "capture-pane", "-t",
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
		return inst.watch(ctx, p.Name, intDefault(p.Window, 0), intDefault(p.ThresholdSeconds, 30))
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

// killSessionWithChildren kills a tmux session and terminates any child
// processes that survive SIGHUP. Returns the number of child processes killed.
func killSessionWithChildren(ctx context.Context, name string) (childrenKilled int, err error) {
	pids := tmuxSessionPIDs(name)
	children := collectDescendants(pids)
	allPIDs := append(pids, children...)

	out, err := runTmux(ctx, "kill-session", "-t", name)
	if err != nil {
		return 0, fmt.Errorf("tmux kill-session: %s %w", strings.TrimSpace(out), err)
	}

	return terminateProcesses(allPIDs), nil
}

// ClearAll stops all watches, clears the owned sessions map, and stops the
// TTL reaper. Called by the tmux memory monitor after kill-server to reset
// tool instance state.
func (inst *tmuxInstance) ClearAll() {
	if inst.reaperCancel != nil {
		inst.reaperCancel()
	}

	inst.mu.Lock()
	// Cancel all watches
	for key, ws := range inst.watched {
		ws.cancel()
		delete(inst.watched, key)
	}
	inst.persistWatches()
	// Clear owned set
	inst.owned = make(map[string]string)
	inst.lastAccess = make(map[string]time.Time)
	inst.persistOwned()
	inst.mu.Unlock()

	inst.sendMu.Lock()
	inst.lastSend = make(map[string]time.Time)
	inst.sendMu.Unlock()

	log.Debugf("tmux", "ClearAll: cleared all watches and owned sessions")
}

// ttlReaper periodically checks for idle tmux sessions and kills them.
func (inst *tmuxInstance) ttlReaper(ctx context.Context) {
	// Tick at sessionTTL/4, minimum 1 minute
	interval := inst.sessionTTL / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			inst.reapExpiredSessions()
		case <-ctx.Done():
			return
		}
	}
}

// reapExpiredSessions kills tmux sessions that haven't been accessed within the TTL.
func (inst *tmuxInstance) reapExpiredSessions() {
	inst.mu.Lock()
	var expired []string
	now := time.Now()
	for name, lastAccess := range inst.lastAccess {
		if now.Sub(lastAccess) > inst.sessionTTL {
			expired = append(expired, name)
		}
	}
	if len(expired) == 0 {
		inst.mu.Unlock()
		return
	}
	for _, name := range expired {
		delete(inst.owned, name)
		delete(inst.lastAccess, name)
	}
	inst.persistOwned()
	inst.mu.Unlock()

	for _, name := range expired {
		inst.cancelWatchesForSession(name)

		killed, err := killSessionWithChildren(context.Background(), name)
		if err != nil {
			log.Debugf("tmux", "ttl reaper: %s: %v", name, err)
			continue
		}
		log.Infof("tmux", "ttl reaper: killed idle session %s (TTL %v exceeded)", name, inst.sessionTTL)
		if killed > 0 {
			log.Debugf("tmux", "ttl reaper: terminated %d child process(es) for %s", killed, name)
		}
	}

	inst.sendMu.Lock()
	for _, name := range expired {
		delete(inst.lastSend, name)
	}
	inst.sendMu.Unlock()

	maybeKillTmuxServer(context.Background())
}

func (inst *tmuxInstance) start(ctx context.Context, name, command, workdir, keys string, watch bool) (ToolResult, error) {
	if name == "" {
		n := atomic.AddUint64(&tmuxCounter, 1)
		name = fmt.Sprintf("foci-%d", n)
	}

	log.Debugf("tmux", "start: name=%s command=%q workdir=%q keys=%q cols=%d rows=%d watch=%v", name, command, workdir, keys, inst.cols, inst.rows, watch)

	// Cancel any stale watches for this session name (e.g. from a prior
	// session that exited naturally before the monitor noticed).
	inst.cancelWatchesForSession(name)

	// Create the tmux session
	args := []string{"new-session", "-d", "-s", name}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}

	// If keys are provided, append them as a shell-quoted argument to the command
	finalCommand := command
	if keys != "" && command != "" {
		finalCommand = command + " " + fmt.Sprintf("%q", keys)
	}
	if finalCommand != "" {
		args = append(args, finalCommand)
	}

	out, err := runTmux(ctx, args...)
	if err != nil {
		return ToolResult{}, fmt.Errorf("tmux new-session: %s %w", strings.TrimSpace(out), err)
	}

	// Resize window so output isn't truncated to a small default terminal size.
	if inst.cols > 0 && inst.rows > 0 {
		out, err = runTmux(ctx, "resize-window", "-t", name, "-x", fmt.Sprintf("%d", inst.cols), "-y", fmt.Sprintf("%d", inst.rows))
		if err != nil {
			log.Warnf("tmux", "resize-window: %s %v", strings.TrimSpace(out), err)
		}
	}

	sessionKey := SessionKeyFromContext(ctx)
	inst.mu.Lock()
	inst.owned[name] = sessionKey
	inst.lastAccess[name] = time.Now()
	inst.persistOwned()
	inst.mu.Unlock()

	result := fmt.Sprintf("Session started: %s", name)

	// Auto-watch for inactivity if requested and notifier is available
	if watch && inst.notifier != nil {
		watchRes, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
		if watchErr != nil {
			log.Warnf("tmux", "auto-watch failed for %s: %v", name, watchErr)
		} else {
			result += "\n" + watchRes.Text
		}
	}

	return TextResult(result), nil
}


func (inst *tmuxInstance) send(ctx context.Context, name, keys string, enter bool) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for send")
	}
	if keys == "" && !enter {
		return ToolResult{}, fmt.Errorf("keys is required for send (or set enter=true to send just Enter)")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	inst.mu.Unlock()

	log.Debugf("tmux", "send: name=%s keys=%q enter=%v", name, keys, enter)
	LogSendEntry(name, len(keys), enter)

	// Rate-limit: enforce minimum gap between consecutive sends to the same session.
	inst.sendMu.Lock()
	if last, ok := inst.lastSend[name]; ok {
		if gap := time.Since(last); gap < sendMinGap {
			wait := sendMinGap - gap
			log.Debugf("tmux", "send: rate-limiting %s, sleeping %v", name, wait)
			LogSendRateLimiting(gap, wait)
			time.Sleep(wait)
		}
	}
	inst.lastSend[name] = time.Now()
	inst.sendMu.Unlock()

	// Send keys first, then Enter as a separate send-keys call.
	// Combining them in one call is unreliable with certain key strings.
	// Use -l flag to send keys as literal string (prevents tmux from interpreting special characters).
	var out string
	var err error
	if keys != "" {
		LogSendSendKeys(len(keys))
		out, err = runTmux(ctx, "send-keys", "-t", name, "-l", keys)
		if err != nil {
			LogSendExit(false, err.Error())
			return ToolResult{}, fmt.Errorf("tmux send-keys: %s %w", strings.TrimSpace(out), err)
		}
	}
	if enter {
		// Brief pause so TUI apps (Claude Code, OpenCode) can process
		// the pasted input before receiving Enter (#26b).
		time.Sleep(200 * time.Millisecond)
		LogSendSendEnter()
		out, err = runTmux(ctx, "send-keys", "-t", name, "Enter")
		if err != nil {
			LogSendExit(false, err.Error())
			return ToolResult{}, fmt.Errorf("tmux send-keys Enter: %s %w", strings.TrimSpace(out), err)
		}
	}

	result := "Keys sent."

	// Best-effort verification: check if sent keys appeared in pane output
	if keys != "" {
		verified := inst.verifyKeysInPane(ctx, name, keys)
		if !verified {
			result += " Keys sent but not confirmed in pane output."
		}
	}

	// Autopilot: auto-watch after send if not already watched
	if inst.autopilot && inst.notifier != nil {
		inst.mu.Lock()
		alreadyWatched := false
		prefix := name + ":"
		for key := range inst.watched {
			if strings.HasPrefix(key, prefix) {
				alreadyWatched = true
				break
			}
		}
		inst.mu.Unlock()

		if !alreadyWatched {
			watchRes, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
			if watchErr != nil {
				log.Warnf("tmux", "autopilot: auto-watch failed for %s: %v", name, watchErr)
			} else {
				log.Debugf("tmux", "autopilot: auto-watching %s after send", name)
				result += "\n" + watchRes.Text
			}
		}
	}

	LogSendExit(true, "")
	return TextResult(result), nil
}

// lettersOnly strips everything except ASCII and Unicode letters from s.
func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// verifyKeysInPane checks if the sent keys appear in the pane output within a timeout.
// Returns true if keys were found, false if not found within the timeout.
func (inst *tmuxInstance) verifyKeysInPane(ctx context.Context, name, keys string) bool {
	// Strip to letters only and truncate to 100 chars so matching is resilient
	// to TUI chrome, special characters, and formatting differences.
	needle := lettersOnly(keys)
	if len(needle) > 100 {
		needle = needle[:100]
	}
	if needle == "" {
		return true // nothing meaningful to verify
	}

	log.Debugf("tmux", "verifyKeysInPane: name=%s needle=%q", name, needle)

	// Poll every 200ms for up to 2 seconds
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(2 * time.Second)

	for {
		select {
		case <-ticker.C:
			// Capture pane content
			out, err := runTmux(ctx, "capture-pane", "-t", name, "-p")
			if err != nil {
				log.Debugf("tmux", "verifyKeysInPane: capture failed: %v", err)
				return false
			}

			haystack := lettersOnly(out)
			if strings.Contains(haystack, needle) {
				log.Debugf("tmux", "verifyKeysInPane: keys confirmed in pane output")
				return true
			}

		case <-timeout:
			log.Debugf("tmux", "verifyKeysInPane: timeout, keys not found in pane output")
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (inst *tmuxInstance) read(ctx context.Context, name string, lines int, raw bool) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for read")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	inst.mu.Unlock()

	log.Debugf("tmux", "read: name=%s lines=%d raw=%v", name, lines, raw)

	out, err := runTmux(ctx, "capture-pane", "-t", name, "-p", fmt.Sprintf("-S-%d", lines))
	if err != nil {
		return ToolResult{}, fmt.Errorf("tmux capture-pane: %s %w", strings.TrimSpace(out), err)
	}
	content := strings.TrimRight(out, "\n")

	if raw {
		return TextResult(content), nil
	}

	agent := detectTUIAgent(content)
	if agent == "" {
		return TextResult(content), nil
	}
	return TextResult(cleanTUIOutput(content, agent)), nil
}

func (inst *tmuxInstance) list(ctx context.Context) (ToolResult, error) {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}|#{session_windows}|#{session_created}")
	if err != nil {
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current") {
			inst.clearStaleOwned()
			return TextResult("No tmux sessions."), nil
		}
		return ToolResult{}, fmt.Errorf("tmux list-sessions: %s %w", strings.TrimSpace(out), err)
	}

	inst.mu.Lock()
	ownedNames := make(map[string]string, len(inst.owned))
	for k, v := range inst.owned {
		ownedNames[k] = v
	}
	watched := make(map[string]*watchedSession, len(inst.watched))
	for k, v := range inst.watched {
		watched[k] = v
	}
	inst.mu.Unlock()

	var rows [][]string
	var ownedStillExist bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		name := parts[0]
		windows := parts[1]
		createdUnix, _ := strconv.ParseInt(parts[2], 10, 64)
		age := "?"
		if createdUnix != 0 {
			age = display.FormatDuration(time.Since(time.Unix(createdUnix, 0)))
		}

		storedKey, isOwned := ownedNames[name]
		if isOwned {
			ownedStillExist = true
		}

		// Owner: extract agent ID from session key.
		owner := "-"
		if isOwned {
			owner = extractOwner(storedKey)
		}

		watchInfo := "-"
		for _, ws := range watched {
			if ws.session == name {
				watchInfo = fmt.Sprintf("w%d: %s", ws.window, ws.threshold.Round(time.Second))
				break
			}
		}

		rows = append(rows, []string{name, windows, age, owner, watchInfo})
	}

	if len(rows) == 0 {
		return TextResult("No tmux sessions."), nil
	}

	// Clean up stale owned entries if none still exist in tmux
	if len(ownedNames) > 0 && !ownedStillExist {
		inst.clearStaleOwned()
	}

	cols := []display.Column{
		{Header: "SESSION"},
		{Header: "W", Align: display.AlignRight},
		{Header: "AGE", Align: display.AlignRight},
		{Header: "OWNER"},
		{Header: "WATCH"},
	}
	return TextResult(display.MarkdownTable(cols, rows)), nil
}

// extractOwner returns the agent ID from a session key, or "self" if unknown.
// Handles "agent:<id>:..." and "<id>/..." formats.
func extractOwner(sessionKey string) string {
	if sessionKey == "" {
		return "self"
	}
	if strings.HasPrefix(sessionKey, "agent:") {
		rest := sessionKey[len("agent:"):]
		if idx := strings.Index(rest, ":"); idx > 0 {
			return rest[:idx]
		}
		return rest
	}
	if idx := strings.Index(sessionKey, "/"); idx > 0 {
		return sessionKey[:idx]
	}
	return sessionKey
}

func (inst *tmuxInstance) kill(ctx context.Context, name string) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for kill")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	log.Debugf("tmux", "kill: name=%s", name)

	// Stop any watches first so the monitor goroutine doesn't fire during cleanup
	inst.cancelWatchesForSession(name)

	// Kill the tmux session and clean up child processes that survived SIGHUP
	killed, err := killSessionWithChildren(ctx, name)
	if err != nil {
		return ToolResult{}, err
	}

	// If no sessions remain, kill the server to avoid an orphaned tmux
	// server process. This is safe: we only kill when the server is empty.
	serverKilled := maybeKillTmuxServer(ctx)

	inst.mu.Lock()
	delete(inst.owned, name)
	delete(inst.lastAccess, name)
	inst.persistOwned()
	inst.mu.Unlock()

	inst.sendMu.Lock()
	delete(inst.lastSend, name)
	inst.sendMu.Unlock()

	result := fmt.Sprintf("Session killed: %s", name)
	if killed > 0 {
		result += fmt.Sprintf(" (%d child process(es) terminated)", killed)
		log.Infof("tmux", "kill %s: terminated %d orphaned child process(es)", name, killed)
	}
	if serverKilled {
		log.Infof("tmux", "kill %s: no sessions remain, killed tmux server", name)
	}

	return TextResult(result), nil
}

// maybeKillTmuxServer kills the tmux server if no sessions remain.
// Returns true if the server was killed.
func maybeKillTmuxServer(ctx context.Context) bool {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return false // server gone or unknown error — leave it alone
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	// No sessions remain — kill the server.
	if _, err := runTmux(ctx, "kill-server"); err == nil {
		return true
	}
	return false
}

// tmuxSessionPIDs returns the PID of each pane's shell in the given tmux session.
func tmuxSessionPIDs(session string) []int {
	out, err := runTmux(context.Background(), "list-panes", "-t", session, "-F", "#{pane_pid}")
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if pid, err := strconv.Atoi(line); err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// collectDescendants returns all descendant PIDs for the given parent PIDs
// by walking /proc/<pid>/task/*/children recursively.
func collectDescendants(pids []int) []int {
	seen := make(map[int]bool)
	var result []int

	var walk func(pid int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true

		// Read children from /proc/<pid>/task/<pid>/children
		childrenFile := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
		data, err := os.ReadFile(childrenFile)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(data)) {
			childPID, err := strconv.Atoi(field)
			if err != nil || childPID <= 1 {
				continue
			}
			result = append(result, childPID)
			walk(childPID)
		}
	}

	for _, pid := range pids {
		walk(pid)
	}
	return result
}

// terminateProcesses sends SIGTERM, waits up to 2 seconds, then SIGKILLs
// any survivors. Returns the number of processes that were signaled.
func terminateProcesses(pids []int) int {
	if len(pids) == 0 {
		return 0
	}

	// Send SIGTERM to all
	var alive []int
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			continue // already dead
		}
		alive = append(alive, pid)
	}

	if len(alive) == 0 {
		return 0
	}

	// Wait up to 2 seconds for graceful shutdown
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var stillAlive []int
		for _, pid := range alive {
			proc, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			// Signal 0 checks if process exists
			if proc.Signal(syscall.Signal(0)) == nil {
				stillAlive = append(stillAlive, pid)
			}
		}
		if len(stillAlive) == 0 {
			return len(alive)
		}
		alive = stillAlive
		time.Sleep(100 * time.Millisecond)
	}

	// SIGKILL survivors
	for _, pid := range alive {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGKILL); err == nil {
			log.Debugf("tmux", "SIGKILL pid %d (did not exit after SIGTERM)", pid)
		}
	}

	return len(pids)
}

func (inst *tmuxInstance) watch(ctx context.Context, name string, window, thresholdSeconds int) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for watch")
	}
	if thresholdSeconds < 1 {
		thresholdSeconds = 30
	}

	log.Debugf("tmux", "watch: name=%s window=%d threshold=%ds", name, window, thresholdSeconds)

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	key := fmt.Sprintf("%s:%d", name, window)
	if _, exists := inst.watched[key]; exists {
		inst.mu.Unlock()
		return ToolResult{}, fmt.Errorf("session %s is already being watched", key)
	}

	// Capture initial pane content so the first poll doesn't reset the
	// activity timer by seeing a "changed" hash (zero-value → real hash).
	var initialHash [md5.Size]byte
	if out, err := runTmux(context.Background(), "capture-pane", "-t",
		fmt.Sprintf("%s:%d", name, window), "-p"); err == nil {
		initialHash = md5.Sum([]byte(normalizePaneContent(out))) // #nosec G401
	}

	monCtx, cancel := context.WithCancel(context.Background())
	ws := &watchedSession{
		session:         name,
		window:          window,
		threshold:       time.Duration(thresholdSeconds) * time.Second,
		lastActivity:    time.Now(),
		lastContent:     initialHash,
		notifier:        inst.notifier,
		agentSessionKey: SessionKeyFromContext(ctx),
		autopilot:       inst.autopilot,
		ctx:             monCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
	}
	inst.watched[key] = ws
	inst.persistWatches()
	inst.mu.Unlock()

	// Start monitoring goroutine
	go tmuxWatchMonitor(ws, inst, key)

	return TextResult(fmt.Sprintf("Watching session %s (window %d) for inactivity (threshold: %ds)", name, window, thresholdSeconds)), nil
}

func (inst *tmuxInstance) unwatch(ctx context.Context, name string) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for unwatch")
	}

	log.Debugf("tmux", "unwatch: name=%s", name)

	sessionKey := SessionKeyFromContext(ctx)

	inst.mu.Lock()
	// Collect watches matching this session name and session key
	prefix := name + ":"
	var toCancel []*watchedSession
	for key, ws := range inst.watched {
		if strings.HasPrefix(key, prefix) {
			// Only unwatch if session keys match (both empty = backwards compat)
			if ws.agentSessionKey == sessionKey || (ws.agentSessionKey == "" && sessionKey == "") {
				toCancel = append(toCancel, ws)
				delete(inst.watched, key)
			}
		}
	}
	if len(toCancel) == 0 {
		inst.mu.Unlock()
		return ToolResult{}, fmt.Errorf("session %s is not being watched", name)
	}
	inst.persistWatches()
	inst.mu.Unlock()

	// Cancel goroutines outside the lock
	for _, ws := range toCancel {
		ws.cancel()
		<-ws.done
	}
	return TextResult(fmt.Sprintf("Stopped watching session %s", name)), nil
}

// detectTUIAgent inspects pane content for known TUI agent markers.
// Returns "cc" for Claude Code, "oc" for OpenCode, or "" if no TUI is detected.
func detectTUIAgent(content string) string {
	// Claude Code markers
	ccMarkers := []string{"Claude Code", "⏵⏵ bypass", "Cooked for", "Crunched for", "Baked for"}
	for _, m := range ccMarkers {
		if strings.Contains(content, m) {
			return "cc"
		}
	}
	// OpenCode markers
	ocMarkers := []string{"OpenCode", "GLM", "Build"}
	for _, m := range ocMarkers {
		if strings.Contains(content, m) {
			return "oc"
		}
	}
	return ""
}

// Compiled regex patterns for TUI cleanup — shared across calls.
var (
	// Common patterns
	reConsecutiveBlankLines = regexp.MustCompile(`\n{3,}`)
	reHorizontalSeparator   = regexp.MustCompile(`^[\s─╌═━╍]+$`)

	// Claude Code patterns
	reCCBoxDrawing    = regexp.MustCompile(`^[─╌━═╰╯╭╮▀▁─]+$`)
	reCCPipeBorder    = regexp.MustCompile(`^\s*│\s?`)
	reCCPipeTrail     = regexp.MustCompile(`\s*│\s*$`)
	reCCStatusHints   = regexp.MustCompile(`(?i)(shift\+tab|ctrl\+o|esc to interrupt|esc to undo|\/help for)`)
	reCCVersionLine   = regexp.MustCompile(`^Claude Code\b.*$`)
	reCCModeIndicator = regexp.MustCompile(`^[⏵⏸]+\s*(bypass|plan mode|auto mode)\s*$`)
	reCCDecoSymbols   = regexp.MustCompile(`^[✻✢\s]+$`)
	reCCLogoBlocks    = regexp.MustCompile(`[▟█▙▄▀▐▌░▒▓]+`)

	// OpenCode patterns
	reOCBorder      = regexp.MustCompile(`^[┃╹╻\s]+$`)
	reOCBoxDrawing  = regexp.MustCompile(`^[─┬┴┼├┤┌┐└┘╭╮╰╯━═╌]+$`)
	reOCStatusHints = regexp.MustCompile(`(?i)(esc to close|ctrl\+[a-z]|alt\+[a-z])`)
	reOCVersionLine = regexp.MustCompile(`^OpenCode\b.*$`)
	reOCSidebar     = regexp.MustCompile(`^(MCP|LSP)\s*[│┃]`)
	reOCBuildLine   = regexp.MustCompile(`^Build\s*[│┃]`)
	reOCErrorRetry  = regexp.MustCompile(`(?i)^(error|retrying)\b.*$`)
	reOCSectionHdr  = regexp.MustCompile(`^(Modified Files|Todo)\s*$`)
	reOCDiffSummary = regexp.MustCompile(`^\d+ files? changed`)
)

// cleanTUIOutput strips TUI chrome from pane content based on the detected agent type.
func cleanTUIOutput(content, agentType string) string {
	lines := strings.Split(content, "\n")
	var cleaned []string

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")

		switch agentType {
		case "cc":
			if shouldStripCC(trimmed) {
				continue
			}
			// Strip pipe borders from content lines
			trimmed = reCCPipeBorder.ReplaceAllString(trimmed, "")
			trimmed = reCCPipeTrail.ReplaceAllString(trimmed, "")
		case "oc":
			if shouldStripOC(trimmed) {
				continue
			}
		}

		// Truncate long horizontal separator lines to save tokens
		// (only for lines that weren't stripped by agent-specific logic)
		if reHorizontalSeparator.MatchString(trimmed) && len(trimmed) > 10 {
			trimmed = trimmed[:10]
		}

		cleaned = append(cleaned, trimmed)
	}

	result := strings.Join(cleaned, "\n")
	// Collapse runs of 3+ blank lines down to 2
	result = reConsecutiveBlankLines.ReplaceAllString(result, "\n\n")
	// Trim leading/trailing whitespace
	result = strings.TrimSpace(result)
	return result
}

// shouldStripCC returns true if the line is Claude Code TUI chrome that should be removed.
func shouldStripCC(line string) bool {
	if reCCBoxDrawing.MatchString(line) {
		return true
	}
	if reCCStatusHints.MatchString(line) {
		return true
	}
	if reCCVersionLine.MatchString(line) {
		return true
	}
	if reCCModeIndicator.MatchString(line) {
		return true
	}
	if reCCDecoSymbols.MatchString(line) {
		return true
	}
	if reCCLogoBlocks.MatchString(line) && len(strings.TrimSpace(line)) < 20 {
		return true
	}
	return false
}

// shouldStripOC returns true if the line is OpenCode TUI chrome that should be removed.
func shouldStripOC(line string) bool {
	if reOCBorder.MatchString(line) {
		return true
	}
	if reOCBoxDrawing.MatchString(line) {
		return true
	}
	if reOCStatusHints.MatchString(line) {
		return true
	}
	if reOCVersionLine.MatchString(line) {
		return true
	}
	if reOCSidebar.MatchString(line) {
		return true
	}
	if reOCBuildLine.MatchString(line) {
		return true
	}
	if reOCErrorRetry.MatchString(line) {
		return true
	}
	if reOCSectionHdr.MatchString(line) {
		return true
	}
	if reOCDiffSummary.MatchString(line) {
		return true
	}
	return false
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	// Use a fresh background context (not agent turn context) so tmux sessions persist.
	// Only apply a timeout for the command execution itself.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if tmuxSocketPath != "" {
		args = append([]string{"-S", tmuxSocketPath}, args...)
	}
	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	// Setsid puts the tmux process in its own session so it (and the tmux
	// server it may spawn) won't be killed when the parent process group
	// is cleaned up. Also drops supplementary groups (foci-secrets).
	cmd.SysProcAttr = ChildSysProcAttrSetsid()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tuiNoisePatterns matches dynamic TUI elements that change without indicating
// meaningful activity. Only clocks and elapsed timers are filtered — spinners,
// token counts, percentages, and cost changes ARE signals of active work.
var tuiNoisePatterns = regexp.MustCompile(strings.Join([]string{
	`\d+[hm]\s*\d+[ms]`,             // elapsed timers: "1m 3s", "2h 30m"
	`\d+:\d{2}(:\d{2})?(\s*[AP]M)?`, // clocks: "14:30", "2:30:00 PM"
	`\d+\.\d+s`,                     // durations: "3.2s", "0.5s"
}, "|"))

// normalizePaneContent strips TUI noise from pane output so that only
// meaningful content changes are detected by the watch monitor. Only strips
// clocks and timers — spinners, token counts, and percentages are kept as
// they indicate active work.
func normalizePaneContent(content string) string {
	return tuiNoisePatterns.ReplaceAllString(content, "")
}

func tmuxWatchMonitor(ws *watchedSession, inst *tmuxInstance, key string) {
	defer close(ws.done)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Read pane content
			out, err := runTmux(context.Background(), "capture-pane", "-t",
				fmt.Sprintf("%s:%d", ws.session, ws.window), "-p")
			if err != nil {
				// Session exited — clean up the watch (debug log is sufficient)
				log.Debugf("tmux", "watch: session %s exited, cleaning up watch", ws.session)
				inst.mu.Lock()
				delete(inst.watched, key)
				inst.persistWatches()
				inst.mu.Unlock()
				return
			}

			// Normalize pane content to filter out TUI noise (status bar
			// clocks, spinners, token counts, etc.) before hashing.
			normalized := normalizePaneContent(out)
			hash := md5.Sum([]byte(normalized)) // #nosec G401 - content change detection, not security

			// Check if content changed
			if hash != ws.lastContent {
				ws.lastContent = hash
				ws.lastActivity = time.Now()
			} else {
				// Content unchanged; check if threshold exceeded
				if time.Since(ws.lastActivity) > ws.threshold {
					log.Infof("tmux", "watch: inactivity detected on %s:%d (threshold %v exceeded)", ws.session, ws.window, ws.threshold)
					msg := prompts.FormatInjectedMessage("TMUX WATCH",
						time.Now(),
						fmt.Sprintf("Session %s:%d has been inactive for %v", ws.session, ws.window, ws.threshold))
					ws.notifier.Notify(ws.agentSessionKey, msg)

					if ws.autopilot {
						// Auto-unwatch: remove from watched map and exit goroutine
						log.Infof("tmux", "autopilot: auto-unwatched %s after inactivity", ws.session)
						inst.mu.Lock()
						delete(inst.watched, key)
						inst.persistWatches()
						inst.mu.Unlock()
						return
					}

					// Reset activity timer to avoid repeated alerts
					ws.lastActivity = time.Now()
				}
			}

		case <-ws.ctx.Done():
			return
		}
	}
}

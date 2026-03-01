package tools

import (
	"context"
	"crypto/md5"
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

	"foci/log"
	"foci/state"
)

var tmuxCounter uint64

// watchedSession tracks a tmux session being monitored for inactivity
type watchedSession struct {
	session         string
	window          int
	threshold       time.Duration
	lastContent     [16]byte // md5 hash
	lastActivity    time.Time
	notifier        *AsyncNotifier
	agentSessionKey string // agent session that started the watch (for async delivery)
	braindead       bool   // auto-unwatch after inactivity notification
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

// tmuxInstance holds per-tool-instance state so each agent gets isolated tmux sessions.
type tmuxInstance struct {
	mu                sync.Mutex
	watched           map[string]*watchedSession // key: "session:window"
	owned             map[string]struct{}        // sessions created by this instance
	notifier          *AsyncNotifier
	cols              int
	rows              int
	braindead         bool // auto-unwatch on inactivity, auto-watch on send
	watchThresholdSec int  // default watch threshold in seconds from config
	stateStore        *state.Store // nil = no persistence
	stateKey          string       // key prefix for persisted owned sessions
	watchStateKey     string       // key for persisted watches (stateKey + ":watches")
}

// NewTmuxTool creates a tmux tool. cols and rows set the default window size
// applied via resize-window after session creation. notifier delivers messages
// when a watched session exceeds its inactivity threshold (nil disables).
// Each call returns an independent tool instance with its own session tracking.
// stateStore and stateKey enable persistence of owned sessions across restarts.
// braindead enables auto-unwatch on inactivity and auto-watch on send.
// watchThresholdSec sets the default watch threshold in seconds.
// The returned cleanup function clears all watches and owned sessions (used by
// the tmux memory monitor after kill-server).
func NewTmuxTool(cols, rows int, notifier *AsyncNotifier, stateStore *state.Store, stateKey string, braindead bool, watchThresholdSec int) (*Tool, func()) {
	if watchThresholdSec < 1 {
		watchThresholdSec = 30
	}
	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]struct{}),
		notifier:          notifier,
		cols:              cols,
		rows:              rows,
		braindead:         braindead,
		watchThresholdSec: watchThresholdSec,
		stateStore:        stateStore,
		stateKey:          stateKey,
		watchStateKey:     stateKey + ":watches",
	}

	// Restore owned sessions from persistent state
	if stateStore != nil {
		var owned []string
		if stateStore.Get(stateKey, &owned) {
			for _, name := range owned {
				inst.owned[name] = struct{}{}
			}
			if len(owned) > 0 {
				log.Debugf("tmux", "restored %d owned session(s) from state", len(owned))
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
					initialHash = md5.Sum([]byte(normalizePaneContent(initOut)))
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
					braindead:       braindead,
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

	return &Tool{
		Name:        "tmux",
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
					"description": "Keystrokes to send (send). Optional if enter=true (sends bare Enter)"
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return inst.execute(ctx, params)
		},
	}, inst.ClearAll
}

func (inst *tmuxInstance) execute(ctx context.Context, params json.RawMessage) (string, error) {
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
		return "", fmt.Errorf("parse params: %w", err)
	}

	switch p.Operation {
	case "start":
		watch := true
		if p.Watch != nil {
			watch = *p.Watch
		}
		return inst.start(ctx, p.Name, p.Command, p.Workdir, watch)
	case "send":
		enter := true
		if p.Enter != nil {
			enter = *p.Enter
		}
		return inst.send(ctx, p.Name, p.Keys, enter)
	case "read":
		lines := 50
		if p.Lines > 0 {
			lines = p.Lines
		}
		return inst.read(ctx, p.Name, lines, p.Raw)
	case "list":
		return inst.list(ctx)
	case "kill":
		return inst.kill(ctx, p.Name)
	case "watch":
		window := 0
		if p.Window > 0 {
			window = p.Window
		}
		threshold := 30
		if p.ThresholdSeconds > 0 {
			threshold = p.ThresholdSeconds
		}
		return inst.watch(ctx, p.Name, window, threshold)
	case "unwatch":
		return inst.unwatch(ctx, p.Name)
	default:
		return "", fmt.Errorf("unknown operation: %q (valid: start, send, read, list, kill, watch, unwatch)", p.Operation)
	}
}

func (inst *tmuxInstance) owns(name string) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	_, ok := inst.owned[name]
	return ok
}

// persistOwned saves the owned sessions map to the state store.
// Must be called with inst.mu held.
func (inst *tmuxInstance) persistOwned() {
	if inst.stateStore == nil {
		return
	}
	owned := make([]string, 0, len(inst.owned))
	for name := range inst.owned {
		owned = append(owned, name)
	}
	if err := inst.stateStore.Set(inst.stateKey, owned); err != nil {
		log.Warnf("tmux", "persist owned sessions: %v", err)
	}
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

// ClearAll stops all watches and clears the owned sessions map. Called by the
// tmux memory monitor after kill-server to reset tool instance state.
func (inst *tmuxInstance) ClearAll() {
	inst.mu.Lock()
	// Cancel all watches
	for key, ws := range inst.watched {
		ws.cancel()
		delete(inst.watched, key)
	}
	inst.persistWatches()
	// Clear owned set
	inst.owned = make(map[string]struct{})
	inst.persistOwned()
	inst.mu.Unlock()

	log.Debugf("tmux", "ClearAll: cleared all watches and owned sessions")
}

func (inst *tmuxInstance) start(ctx context.Context, name, command, workdir string, watch bool) (string, error) {
	if name == "" {
		n := atomic.AddUint64(&tmuxCounter, 1)
		name = fmt.Sprintf("foci-%d", n)
	}

	args := []string{"new-session", "-d", "-s", name}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}
	if command != "" {
		args = append(args, command)
	}

	log.Debugf("tmux", "start: name=%s command=%q workdir=%q cols=%d rows=%d watch=%v", name, command, workdir, inst.cols, inst.rows, watch)

	out, err := runTmux(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("tmux new-session: %s %w", strings.TrimSpace(out), err)
	}

	// Resize window so output isn't truncated to a small default terminal size.
	if inst.cols > 0 && inst.rows > 0 {
		out, err = runTmux(ctx, "resize-window", "-t", name, "-x", fmt.Sprintf("%d", inst.cols), "-y", fmt.Sprintf("%d", inst.rows))
		if err != nil {
			log.Warnf("tmux", "resize-window: %s %v", strings.TrimSpace(out), err)
		}
	}

	inst.mu.Lock()
	inst.owned[name] = struct{}{}
	inst.persistOwned()
	inst.mu.Unlock()

	result := fmt.Sprintf("Session started: %s", name)

	// Auto-watch for inactivity if requested and notifier is available
	if watch && inst.notifier != nil {
		watchResult, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
		if watchErr != nil {
			log.Warnf("tmux", "auto-watch failed for %s: %v", name, watchErr)
		} else {
			result += "\n" + watchResult
		}
	}

	return result, nil
}

func (inst *tmuxInstance) send(ctx context.Context, name, keys string, enter bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for send")
	}
	if keys == "" && !enter {
		return "", fmt.Errorf("keys is required for send (or set enter=true to send just Enter)")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "send: name=%s keys=%q enter=%v", name, keys, enter)

	// Send keys first, then Enter as a separate send-keys call.
	// Combining them in one call is unreliable with certain key strings.
	var out string
	var err error
	if keys != "" {
		out, err = runTmux(ctx, "send-keys", "-t", name, keys)
		if err != nil {
			return "", fmt.Errorf("tmux send-keys: %s %w", strings.TrimSpace(out), err)
		}
	}
	if enter {
		// Brief pause so TUI apps (Claude Code, OpenCode) can process
		// the pasted input before receiving Enter (#26b).
		time.Sleep(200 * time.Millisecond)
		out, err = runTmux(ctx, "send-keys", "-t", name, "Enter")
		if err != nil {
			return "", fmt.Errorf("tmux send-keys Enter: %s %w", strings.TrimSpace(out), err)
		}
	}

	result := "Keys sent."

	// Braindead: auto-watch after send if not already watched
	if inst.braindead && inst.notifier != nil {
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
			watchResult, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
			if watchErr != nil {
				log.Warnf("tmux", "braindead: auto-watch failed for %s: %v", name, watchErr)
			} else {
				log.Debugf("tmux", "braindead: auto-watching %s after send", name)
				result += "\n" + watchResult
			}
		}
	}

	return result, nil
}

func (inst *tmuxInstance) read(ctx context.Context, name string, lines int, raw bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for read")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "read: name=%s lines=%d raw=%v", name, lines, raw)

	out, err := runTmux(ctx, "capture-pane", "-t", name, "-p", fmt.Sprintf("-S-%d", lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %s %w", strings.TrimSpace(out), err)
	}
	content := strings.TrimRight(out, "\n")

	if raw {
		return content, nil
	}

	agent := detectTUIAgent(content)
	if agent == "" {
		return content, nil
	}
	return cleanTUIOutput(content, agent), nil
}

func (inst *tmuxInstance) list(ctx context.Context) (string, error) {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}|#{session_windows}|#{session_created}")
	if err != nil {
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current") {
			return "No tmux sessions.", nil
		}
		return "", fmt.Errorf("tmux list-sessions: %s %w", strings.TrimSpace(out), err)
	}

	inst.mu.Lock()
	ownedNames := make(map[string]struct{}, len(inst.owned))
	for k, v := range inst.owned {
		ownedNames[k] = v
	}
	watched := make(map[string]*watchedSession, len(inst.watched))
	for k, v := range inst.watched {
		watched[k] = v
	}
	inst.mu.Unlock()

	var lines []string
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
		age := formatTmuxAge(createdUnix)

		_, isOwned := ownedNames[name]
		if isOwned {
			ownedStillExist = true
		}

		// Status: owned / watched / idle
		status := "idle"
		if isOwned {
			status = "owned"
		}

		watchInfo := "-"
		for _, ws := range watched {
			if ws.session == name {
				status = "watched"
				watchInfo = fmt.Sprintf("w%d: %s", ws.window, ws.threshold.Round(time.Second))
				break
			}
		}

		lines = append(lines, fmt.Sprintf("%-20s %sw  %6s  %-8s %s", name, windows, age, status, watchInfo))
	}

	if len(lines) == 0 {
		return "No tmux sessions.", nil
	}

	// Clean up stale owned entries if none still exist in tmux
	if len(ownedNames) > 0 && !ownedStillExist {
		inst.mu.Lock()
		inst.owned = make(map[string]struct{})
		inst.persistOwned()
		inst.mu.Unlock()
	}

	header := fmt.Sprintf("%-20s %s  %6s  %-8s %s", "SESSION", "W", "AGE", "STATUS", "WATCH")
	return header + "\n" + strings.Join(lines, "\n"), nil
}

// formatTmuxAge converts a Unix timestamp to a human-readable age string.
func formatTmuxAge(createdUnix int64) string {
	if createdUnix == 0 {
		return "?"
	}
	created := time.Unix(createdUnix, 0)
	age := time.Since(created)
	if age < time.Minute {
		return fmt.Sprintf("%ds", int(age.Seconds()))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(age.Hours()), int(age.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(age.Hours())/24, int(age.Hours())%24)
}

func (inst *tmuxInstance) kill(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for kill")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "kill: name=%s", name)

	// Stop any watches first so the monitor goroutine doesn't fire during cleanup
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

	// Collect child process trees before killing the session.
	// tmux kill-session sends SIGHUP, but many processes (e.g. OpenCode)
	// ignore it and get reparented to PID 1, running forever.
	pids := tmuxSessionPIDs(name)
	children := collectDescendants(pids)
	allPIDs := append(pids, children...)

	// Kill the tmux session
	out, err := runTmux(ctx, "kill-session", "-t", name)
	if err != nil {
		return "", fmt.Errorf("tmux kill-session: %s %w", strings.TrimSpace(out), err)
	}

	// Clean up child processes that survived SIGHUP
	killed := terminateProcesses(allPIDs)

	inst.mu.Lock()
	delete(inst.owned, name)
	inst.persistOwned()
	inst.mu.Unlock()

	result := fmt.Sprintf("Session killed: %s", name)
	if killed > 0 {
		result += fmt.Sprintf(" (%d child process(es) terminated)", killed)
		log.Infof("tmux", "kill %s: terminated %d orphaned child process(es)", name, killed)
	}

	return result, nil
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

func (inst *tmuxInstance) watch(ctx context.Context, name string, window, thresholdSeconds int) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for watch")
	}
	if thresholdSeconds < 1 {
		thresholdSeconds = 30
	}

	log.Debugf("tmux", "watch: name=%s window=%d threshold=%ds", name, window, thresholdSeconds)

	inst.mu.Lock()
	key := fmt.Sprintf("%s:%d", name, window)
	if _, exists := inst.watched[key]; exists {
		inst.mu.Unlock()
		return "", fmt.Errorf("session %s is already being watched", key)
	}

	// Capture initial pane content so the first poll doesn't reset the
	// activity timer by seeing a "changed" hash (zero-value → real hash).
	var initialHash [md5.Size]byte
	if out, err := runTmux(context.Background(), "capture-pane", "-t",
		fmt.Sprintf("%s:%d", name, window), "-p"); err == nil {
		initialHash = md5.Sum([]byte(normalizePaneContent(out)))
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
		braindead:       inst.braindead,
		ctx:             monCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
	}
	inst.watched[key] = ws
	inst.persistWatches()
	inst.mu.Unlock()

	// Start monitoring goroutine
	go tmuxWatchMonitor(ws, inst, key)

	return fmt.Sprintf("Watching session %s (window %d) for inactivity (threshold: %ds)", name, window, thresholdSeconds), nil
}

func (inst *tmuxInstance) unwatch(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for unwatch")
	}

	log.Debugf("tmux", "unwatch: name=%s", name)

	inst.mu.Lock()
	// Collect all watches matching this session name (any window)
	prefix := name + ":"
	var toCancel []*watchedSession
	for key, ws := range inst.watched {
		if strings.HasPrefix(key, prefix) {
			toCancel = append(toCancel, ws)
			delete(inst.watched, key)
		}
	}
	if len(toCancel) == 0 {
		inst.mu.Unlock()
		return "", fmt.Errorf("session %s is not being watched", name)
	}
	inst.persistWatches()
	inst.mu.Unlock()

	// Cancel goroutines outside the lock
	for _, ws := range toCancel {
		ws.cancel()
		<-ws.done
	}
	return fmt.Sprintf("Stopped watching session %s", name), nil
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
				// Session is dead — notify and clean up
				log.Infof("tmux", "watch: session %s no longer exists, auto-unwatching", ws.session)
				msg := fmt.Sprintf("[TMUX WATCH] Session %s no longer exists — auto-unwatched", ws.session)
				ws.notifier.Notify(ws.agentSessionKey, msg)

				inst.mu.Lock()
				delete(inst.watched, key)
				inst.persistWatches()
				inst.mu.Unlock()
				return
			}

			// Normalize pane content to filter out TUI noise (status bar
			// clocks, spinners, token counts, etc.) before hashing.
			normalized := normalizePaneContent(out)
			hash := md5.Sum([]byte(normalized))

			// Check if content changed
			if hash != ws.lastContent {
				ws.lastContent = hash
				ws.lastActivity = time.Now()
			} else {
				// Content unchanged; check if threshold exceeded
				if time.Since(ws.lastActivity) > ws.threshold {
					log.Infof("tmux", "watch: inactivity detected on %s:%d (threshold %v exceeded)", ws.session, ws.window, ws.threshold)
					msg := fmt.Sprintf("[TMUX WATCH] Session %s:%d has been inactive for %v", ws.session, ws.window, ws.threshold)
					ws.notifier.Notify(ws.agentSessionKey, msg)

					if ws.braindead {
						// Auto-unwatch: remove from watched map and exit goroutine
						log.Infof("tmux", "braindead: auto-unwatched %s after inactivity", ws.session)
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

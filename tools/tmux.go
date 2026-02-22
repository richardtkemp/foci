package tools

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"clod/log"
)

var tmuxCounter uint64

// watchedSession tracks a tmux session being monitored for inactivity
type watchedSession struct {
	session      string
	window       int
	threshold    time.Duration
	lastContent  [16]byte // md5 hash
	lastActivity time.Time
	notifier     *AsyncNotifier
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
}

// tmuxInstance holds per-tool-instance state so each agent gets isolated tmux sessions.
type tmuxInstance struct {
	mu       sync.Mutex
	watched  map[string]*watchedSession // key: "session:window"
	owned    map[string]struct{}        // sessions created by this instance
	notifier *AsyncNotifier
	cols     int
	rows     int
}

// NewTmuxTool creates a tmux tool. cols and rows set the default window size
// applied via resize-window after session creation. notifier delivers messages
// when a watched session exceeds its inactivity threshold (nil disables).
// Each call returns an independent tool instance with its own session tracking.
func NewTmuxTool(cols, rows int, notifier *AsyncNotifier) *Tool {
	inst := &tmuxInstance{
		watched:  make(map[string]*watchedSession),
		owned:    make(map[string]struct{}),
		notifier: notifier,
		cols:     cols,
		rows:     rows,
	}
	return &Tool{
		Name:        "tmux",
		Description: "Manage tmux sessions — start, send keys, read pane output, list, kill, watch for inactivity. Sessions persist across agent turns.",
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
				"keys": {
					"type": "string",
					"description": "Keystrokes to send (send)"
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
				}
			},
			"required": ["operation"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return inst.execute(ctx, params)
		},
	}
}

func (inst *tmuxInstance) execute(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Operation        string `json:"operation"`
		Name             string `json:"name"`
		Command          string `json:"command"`
		Workdir          string `json:"workdir"`
		Keys             string `json:"keys"`
		Enter            *bool  `json:"enter"`
		Lines            int    `json:"lines"`
		Window           int    `json:"window"`
		ThresholdSeconds int    `json:"threshold_seconds"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	switch p.Operation {
	case "start":
		return inst.start(ctx, p.Name, p.Command, p.Workdir)
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
		return inst.read(ctx, p.Name, lines)
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

func (inst *tmuxInstance) start(ctx context.Context, name, command, workdir string) (string, error) {
	if name == "" {
		n := atomic.AddUint64(&tmuxCounter, 1)
		name = fmt.Sprintf("clod-%d", n)
	}

	args := []string{"new-session", "-d", "-s", name}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}
	if command != "" {
		args = append(args, command)
	}

	log.Debugf("tmux", "start: name=%s command=%q workdir=%q cols=%d rows=%d", name, command, workdir, inst.cols, inst.rows)

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
	inst.mu.Unlock()

	return fmt.Sprintf("Session started: %s", name), nil
}

func (inst *tmuxInstance) send(ctx context.Context, name, keys string, enter bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for send")
	}
	if keys == "" {
		return "", fmt.Errorf("keys is required for send")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "send: name=%s keys=%q enter=%v", name, keys, enter)

	// Send keys first, then Enter as a separate send-keys call.
	// Combining them in one call is unreliable with certain key strings.
	out, err := runTmux(ctx, "send-keys", "-t", name, keys)
	if err != nil {
		return "", fmt.Errorf("tmux send-keys: %s %w", strings.TrimSpace(out), err)
	}
	if enter {
		out, err = runTmux(ctx, "send-keys", "-t", name, "Enter")
		if err != nil {
			return "", fmt.Errorf("tmux send-keys Enter: %s %w", strings.TrimSpace(out), err)
		}
	}
	return "Keys sent.", nil
}

func (inst *tmuxInstance) read(ctx context.Context, name string, lines int) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for read")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "read: name=%s lines=%d", name, lines)

	out, err := runTmux(ctx, "capture-pane", "-t", name, "-p", fmt.Sprintf("-S-%d", lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %s %w", strings.TrimSpace(out), err)
	}
	return strings.TrimRight(out, "\n"), nil
}

func (inst *tmuxInstance) list(ctx context.Context) (string, error) {
	inst.mu.Lock()
	if len(inst.owned) == 0 {
		inst.mu.Unlock()
		return "No tmux sessions.", nil
	}
	// Copy owned set under lock
	ownedNames := make(map[string]struct{}, len(inst.owned))
	for k, v := range inst.owned {
		ownedNames[k] = v
	}
	inst.mu.Unlock()

	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}: #{session_windows} windows (created #{session_created_string})")
	if err != nil {
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current") {
			return "No tmux sessions.", nil
		}
		return "", fmt.Errorf("tmux list-sessions: %s %w", strings.TrimSpace(out), err)
	}

	// Filter to only sessions owned by this instance
	var filtered []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		// Line format: "name: N windows (created ...)"
		name := strings.SplitN(line, ":", 2)[0]
		if _, ok := ownedNames[name]; ok {
			filtered = append(filtered, line)
		}
	}

	if len(filtered) == 0 {
		// Owned sessions no longer exist in tmux — clean up stale entries
		inst.mu.Lock()
		inst.owned = make(map[string]struct{})
		inst.mu.Unlock()
		return "No tmux sessions.", nil
	}
	return strings.Join(filtered, "\n"), nil
}

func (inst *tmuxInstance) kill(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for kill")
	}
	if !inst.owns(name) {
		return "", fmt.Errorf("session %q not owned by this agent", name)
	}

	log.Debugf("tmux", "kill: name=%s", name)

	out, err := runTmux(ctx, "kill-session", "-t", name)
	if err != nil {
		return "", fmt.Errorf("tmux kill-session: %s %w", strings.TrimSpace(out), err)
	}

	inst.mu.Lock()
	delete(inst.owned, name)
	inst.mu.Unlock()

	return fmt.Sprintf("Session killed: %s", name), nil
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

	monCtx, cancel := context.WithCancel(context.Background())
	ws := &watchedSession{
		session:      name,
		window:       window,
		threshold:    time.Duration(thresholdSeconds) * time.Second,
		lastActivity: time.Now(),
		notifier:     inst.notifier,
		ctx:          monCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	inst.watched[key] = ws
	inst.mu.Unlock()

	// Start monitoring goroutine
	go tmuxWatchMonitor(ws)

	return fmt.Sprintf("Watching session %s (window %d) for inactivity (threshold: %ds)", name, window, thresholdSeconds), nil
}

func (inst *tmuxInstance) unwatch(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for unwatch")
	}

	log.Debugf("tmux", "unwatch: name=%s", name)

	inst.mu.Lock()
	key := name + ":0" // unwatch without window removes all watches for this session
	ws, exists := inst.watched[key]
	if !exists {
		inst.mu.Unlock()
		return "", fmt.Errorf("session %s is not being watched", name)
	}
	delete(inst.watched, key)
	inst.mu.Unlock()

	ws.cancel()
	<-ws.done
	return fmt.Sprintf("Stopped watching session %s", name), nil
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	// Use a fresh background context (not agent turn context) so tmux sessions persist.
	// Only apply a timeout for the command execution itself.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	// Setsid puts the tmux process in its own session so it (and the tmux
	// server it may spawn) won't be killed when the parent process group
	// is cleaned up.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tuiNoisePatterns matches dynamic TUI elements that change constantly without
// indicating meaningful activity: elapsed timers, clocks, spinner characters,
// token/percentage counters, and progress indicators.
var tuiNoisePatterns = regexp.MustCompile(strings.Join([]string{
	`\d+[hm]\s*\d+[ms]`,                       // elapsed timers: "1m 3s", "2h 30m"
	`\d+:\d{2}(:\d{2})?(\s*[AP]M)?`,           // clocks: "14:30", "2:30:00 PM"
	`\d[\d,]*\s*tokens?`,                       // token counts: "88,447 tokens"
	`\d+\.?\d*%`,                               // percentages: "44%", "88.5%"
	`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏⣾⣽⣻⢿⡿⣟⣯⣷⡀⡁⡂⡃⡄⡅⡆⡇⡈⡉⡊⡋⡌⡍⡎⡏◐◓◑◒⣀⣤⣶⣿|/\-\\]`, // spinner chars
	`\$\d+\.\d+`,                               // cost displays: "$0.0430"
	`\d+\.\d+s`,                                // durations: "3.2s", "0.5s"
}, "|"))

// normalizePaneContent strips TUI noise from pane output so that only
// meaningful content changes are detected by the watch monitor. This prevents
// status bar clocks, elapsed timers, spinners, token counters, and progress
// indicators from resetting the inactivity timer.
func normalizePaneContent(content string) string {
	return tuiNoisePatterns.ReplaceAllString(content, "")
}

func tmuxWatchMonitor(ws *watchedSession) {
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
				log.Debugf("tmux", "watch monitor read error: %v", err)
				return // session probably doesn't exist, stop watching
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
					ws.notifier.Notify(msg)

					// Reset activity timer to avoid repeated alerts
					ws.lastActivity = time.Now()
				}
			}

		case <-ws.ctx.Done():
			return
		}
	}
}

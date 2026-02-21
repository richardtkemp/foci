package tools

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clod/log"
)

var tmuxCounter uint64

// watchedSession tracks a tmux session being monitored for inactivity
type watchedSession struct {
	session       string
	window        int
	threshold     time.Duration
	lastContent   [16]byte // md5 hash
	lastActivity  time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
}

var (
	watchedMu   sync.Mutex
	watchedSess = make(map[string]*watchedSession)
)

func NewTmuxTool() *Tool {
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
			return tmuxExecute(ctx, params)
		},
	}
}

func tmuxExecute(ctx context.Context, params json.RawMessage) (string, error) {
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
		return tmuxStart(ctx, p.Name, p.Command, p.Workdir)
	case "send":
		enter := true
		if p.Enter != nil {
			enter = *p.Enter
		}
		return tmuxSend(ctx, p.Name, p.Keys, enter)
	case "read":
		lines := 50
		if p.Lines > 0 {
			lines = p.Lines
		}
		return tmuxRead(ctx, p.Name, lines)
	case "list":
		return tmuxList(ctx)
	case "kill":
		return tmuxKill(ctx, p.Name)
	case "watch":
		window := 0
		if p.Window > 0 {
			window = p.Window
		}
		threshold := 30
		if p.ThresholdSeconds > 0 {
			threshold = p.ThresholdSeconds
		}
		return tmuxWatch(ctx, p.Name, window, threshold)
	case "unwatch":
		return tmuxUnwatch(ctx, p.Name)
	default:
		return "", fmt.Errorf("unknown operation: %q (valid: start, send, read, list, kill, watch, unwatch)", p.Operation)
	}
}

func tmuxStart(ctx context.Context, name, command, workdir string) (string, error) {
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

	log.Debugf("tmux", "start: name=%s command=%q workdir=%q", name, command, workdir)

	out, err := runTmux(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("tmux new-session: %s %w", strings.TrimSpace(out), err)
	}
	return fmt.Sprintf("Session started: %s", name), nil
}

func tmuxSend(ctx context.Context, name, keys string, enter bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for send")
	}
	if keys == "" {
		return "", fmt.Errorf("keys is required for send")
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

func tmuxRead(ctx context.Context, name string, lines int) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for read")
	}

	log.Debugf("tmux", "read: name=%s lines=%d", name, lines)

	out, err := runTmux(ctx, "capture-pane", "-t", name, "-p", fmt.Sprintf("-S-%d", lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %s %w", strings.TrimSpace(out), err)
	}
	return strings.TrimRight(out, "\n"), nil
}

func tmuxList(ctx context.Context) (string, error) {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}: #{session_windows} windows (created #{session_created_string})")
	if err != nil {
		// "no server running" means no sessions — not an error
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current") {
			return "No tmux sessions.", nil
		}
		return "", fmt.Errorf("tmux list-sessions: %s %w", strings.TrimSpace(out), err)
	}
	result := strings.TrimSpace(out)
	if result == "" {
		return "No tmux sessions.", nil
	}
	return result, nil
}

func tmuxKill(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for kill")
	}

	log.Debugf("tmux", "kill: name=%s", name)

	out, err := runTmux(ctx, "kill-session", "-t", name)
	if err != nil {
		return "", fmt.Errorf("tmux kill-session: %s %w", strings.TrimSpace(out), err)
	}
	return fmt.Sprintf("Session killed: %s", name), nil
}

func runTmux(ctx context.Context, args ...string) (string, error) {
	// Use a fresh background context (not agent turn context) so tmux sessions persist.
	// Only apply a timeout for the command execution itself.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tmuxWatch(ctx context.Context, name string, window, thresholdSeconds int) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for watch")
	}
	if thresholdSeconds < 1 {
		thresholdSeconds = 30
	}

	log.Debugf("tmux", "watch: name=%s window=%d threshold=%ds", name, window, thresholdSeconds)

	watchedMu.Lock()
	key := fmt.Sprintf("%s:%d", name, window)
	if _, exists := watchedSess[key]; exists {
		watchedMu.Unlock()
		return "", fmt.Errorf("session %s is already being watched", key)
	}

	monCtx, cancel := context.WithCancel(context.Background())
	ws := &watchedSession{
		session:      name,
		window:       window,
		threshold:    time.Duration(thresholdSeconds) * time.Second,
		lastActivity: time.Now(),
		ctx:          monCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	watchedSess[key] = ws
	watchedMu.Unlock()

	// Start monitoring goroutine
	go tmuxWatchMonitor(ws)

	return fmt.Sprintf("Watching session %s (window %d) for inactivity (threshold: %ds)", name, window, thresholdSeconds), nil
}

func tmuxUnwatch(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for unwatch")
	}

	log.Debugf("tmux", "unwatch: name=%s", name)

	watchedMu.Lock()
	key := name + ":0" // unwatch without window removes all watches for this session
	ws, exists := watchedSess[key]
	if !exists {
		watchedMu.Unlock()
		return "", fmt.Errorf("session %s is not being watched", name)
	}
	delete(watchedSess, key)
	watchedMu.Unlock()

	ws.cancel()
	<-ws.done
	return fmt.Sprintf("Stopped watching session %s", name), nil
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

			// Compute md5 hash of pane content
			hash := md5.Sum([]byte(out))

			// Check if content changed
			if hash != ws.lastContent {
				ws.lastContent = hash
				ws.lastActivity = time.Now()
			} else {
				// Content unchanged; check if threshold exceeded
				if time.Since(ws.lastActivity) > ws.threshold {
					// Send wake message to the session (just log for now)
					log.Infof("tmux", "watch: inactivity detected on %s:%d (threshold %v exceeded)", ws.session, ws.window, ws.threshold)

					// Reset activity timer to avoid repeated alerts
					ws.lastActivity = time.Now()
				}
			}

		case <-ws.ctx.Done():
			return
		}
	}
}

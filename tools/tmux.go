package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"clod/log"
)

var tmuxCounter uint64

func NewTmuxTool() *Tool {
	return &Tool{
		Name:        "tmux",
		Description: "Manage tmux sessions — start, send keys, read pane output, list, kill. Sessions persist across agent turns.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"operation": {
					"type": "string",
					"enum": ["start", "send", "read", "list", "kill"],
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
		Operation string `json:"operation"`
		Name      string `json:"name"`
		Command   string `json:"command"`
		Workdir   string `json:"workdir"`
		Keys      string `json:"keys"`
		Enter     *bool  `json:"enter"`
		Lines     int    `json:"lines"`
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
	default:
		return "", fmt.Errorf("unknown operation: %q (valid: start, send, read, list, kill)", p.Operation)
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
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

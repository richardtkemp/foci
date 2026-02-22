package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"clod/log"
	"clod/secrets"
)

// ExecWakeFunc is called when an auto-backgrounded command completes.
// It delivers the result back to the agent session.
type ExecWakeFunc func(command string, result string)

// NewExecTool creates an exec tool. If store is non-nil, commands get
// secret template resolution, output redaction, and blocked path checks.
// autoBackgroundSecs is the threshold after which a running command is
// auto-backgrounded (0 disables). onComplete is called when an auto-backgrounded
// command finishes.
func NewExecTool(store *secrets.Store, autoBackgroundSecs int, onComplete ExecWakeFunc) *Tool {
	return &Tool{
		Name:        "exec",
		Description: "Run a shell command and return its output. Use timeout to limit execution time. Reference secrets with {{secret:NAME}} syntax. Set background=true for commands that spawn persistent processes (tmux, daemons) — children will survive after the exec call.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "Shell command to execute. Use {{secret:NAME}} to reference secrets."
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default 30)"
				},
				"background": {
					"type": "boolean",
					"description": "If true, child processes survive after the command exits (for tmux, daemons, etc.)"
				}
			},
			"required": ["command"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return execCommand(ctx, params, store, autoBackgroundSecs, onComplete)
		},
	}
}

func execCommand(ctx context.Context, params json.RawMessage, store *secrets.Store, autoBackgroundSecs int, onComplete ExecWakeFunc) (string, error) {
	var p struct {
		Command    string `json:"command"`
		Timeout    int    `json:"timeout"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	// Check blocked paths
	if store != nil && store.IsBlockedCommand(p.Command) {
		return "", fmt.Errorf("command references a blocked path")
	}

	// Resolve secret templates
	cmd := p.Command
	if store != nil {
		resolved, err := store.Resolve(cmd)
		if err != nil {
			return "", fmt.Errorf("resolve secrets: %w", err)
		}
		cmd = resolved
	}

	timeout := 30 * time.Second
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
	}

	log.Debugf("exec", "running: %s (timeout=%s background=%v)", truncateCmd(p.Command, 200), timeout, p.Background)

	// For explicit background mode, use the original direct approach
	if p.Background {
		return execDirect(ctx, cmd, p.Command, timeout, true, store)
	}

	// Auto-background: if threshold is set and onComplete is available,
	// start the command and wait with a timer
	if autoBackgroundSecs > 0 && onComplete != nil {
		return execWithAutoBackground(ctx, cmd, p.Command, timeout, store, autoBackgroundSecs, onComplete)
	}

	return execDirect(ctx, cmd, p.Command, timeout, false, store)
}

// execDirect runs a command and waits for completion (original behavior).
func execDirect(ctx context.Context, cmd, displayCmd string, timeout time.Duration, background bool, store *secrets.Store) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc := exec.CommandContext(ctx, "sh", "-c", cmd)

	if background {
		proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		proc.WaitDelay = 2 * time.Second
	} else {
		proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		proc.Cancel = func() error {
			return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		}
	}

	out, err := proc.CombinedOutput()
	return formatResult(string(out), err, ctx, timeout, displayCmd, store), nil
}

// execWithAutoBackground starts a command and returns early if it exceeds the threshold.
// The command continues running and results are delivered via onComplete.
func execWithAutoBackground(ctx context.Context, cmd, displayCmd string, timeout time.Duration, store *secrets.Store, thresholdSecs int, onComplete ExecWakeFunc) (string, error) {
	// Use a separate context for the command (not tied to agent turn)
	cmdCtx, cmdCancel := context.WithTimeout(context.Background(), timeout)

	proc := exec.CommandContext(cmdCtx, "sh", "-c", cmd)
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc.Cancel = func() error {
		return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
	}

	var stdout bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stdout

	if err := proc.Start(); err != nil {
		cmdCancel()
		return "", fmt.Errorf("start command: %w", err)
	}

	// Wait for completion or threshold
	done := make(chan error, 1)
	go func() {
		done <- proc.Wait()
	}()

	threshold := time.Duration(thresholdSecs) * time.Second
	select {
	case err := <-done:
		// Command finished before threshold
		cmdCancel()
		return formatResult(stdout.String(), err, cmdCtx, timeout, displayCmd, store), nil

	case <-time.After(threshold):
		// Threshold exceeded — auto-background
		log.Infof("exec", "auto-backgrounding after %v: %s", threshold, truncateCmd(displayCmd, 100))

		// Continue waiting in background goroutine
		go func() {
			defer cmdCancel()
			err := <-done
			result := formatResult(stdout.String(), err, cmdCtx, timeout, displayCmd, store)
			msg := fmt.Sprintf("[EXEC RESULT] Command completed:\n$ %s\n\n%s", displayCmd, result)
			onComplete(displayCmd, msg)
		}()

		return fmt.Sprintf("Command still running (exceeded %ds threshold). Results will be delivered when complete.\n$ %s", thresholdSecs, displayCmd), nil

	case <-ctx.Done():
		// Agent turn cancelled — let the command continue in background
		go func() {
			defer cmdCancel()
			<-done
		}()
		return "", ctx.Err()
	}
}

// formatResult formats command output with error info, truncation, and redaction.
func formatResult(output string, err error, ctx context.Context, timeout time.Duration, displayCmd string, store *secrets.Store) string {
	result := output
	const maxLen = 100_000
	if len(result) > maxLen {
		result = result[:maxLen] + "\n... (truncated)"
	}

	// Redact secrets from output
	if store != nil {
		result = store.Redact(result)
	}

	if err != nil {
		if ctx.Err() != nil {
			log.Warnf("exec", "command timed out after %s: %s", timeout, truncateCmd(displayCmd, 100))
		}
		return result + "\nError: " + err.Error()
	}

	return result
}

func truncateCmd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

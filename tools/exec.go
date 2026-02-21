package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"clod/log"
	"clod/secrets"
)

// NewExecTool creates an exec tool. If store is non-nil, commands get
// secret template resolution, output redaction, and blocked path checks.
func NewExecTool(store *secrets.Store) *Tool {
	return &Tool{
		Name:        "exec",
		Description: "Run a shell command and return its output. Use timeout to limit execution time. Reference secrets with {{secret:NAME}} syntax.",
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
				}
			},
			"required": ["command"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return execCommand(ctx, params, store)
		},
	}
}

func execCommand(ctx context.Context, params json.RawMessage, store *secrets.Store) (string, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
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

	log.Debugf("exec", "running: %s (timeout=%s)", truncateCmd(p.Command, 200), timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc := exec.CommandContext(ctx, "sh", "-c", cmd)
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc.Cancel = func() error {
		return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
	}
	out, err := proc.CombinedOutput()

	result := string(out)
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
			log.Warnf("exec", "command timed out after %s: %s", timeout, truncateCmd(p.Command, 100))
		}
		return result + "\nError: " + err.Error(), nil
	}

	return result, nil
}

func truncateCmd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

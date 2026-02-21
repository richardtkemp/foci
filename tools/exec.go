package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"clod/log"
)

func NewExecTool() *Tool {
	return &Tool{
		Name:        "exec",
		Description: "Run a shell command and return its output. Use timeout to limit execution time.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "Shell command to execute"
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default 30)"
				}
			},
			"required": ["command"]
		}`),
		Execute: execCommand,
	}
}

func execCommand(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	timeout := 30 * time.Second
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
	}

	log.Debugf("exec", "running: %s (timeout=%s)", truncateCmd(p.Command, 200), timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", p.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	out, err := cmd.CombinedOutput()

	result := string(out)
	const maxLen = 100_000
	if len(result) > maxLen {
		result = result[:maxLen] + "\n... (truncated)"
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

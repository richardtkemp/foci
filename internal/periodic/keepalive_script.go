package periodic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foci/internal/procx"
	"foci/shared/prompts"
)

// defaultKeepaliveScriptTimeout bounds one keepalive prompt script run. A
// hung script must not delay warming much: on timeout the session still gets
// the plain ping.
const defaultKeepaliveScriptTimeout = 15 * time.Second

// keepaliveRun is what a keepalive prompt script is told about one run: the
// session it is producing a prompt for, and that session's place in this
// cycle's targets.
type keepaliveRun struct {
	sessionKey string
	index      int  // 0-based position among this cycle's targets
	count      int  // number of targets this cycle
	isDefault  bool // the session is the agent's default parent session
}

// keepaliveScriptPrompt runs the keepalive prompt script for one target
// session and returns its trimmed stdout as that session's prompt (#1351).
// Any failure — spawn error, non-zero exit, timeout, empty output — logs one
// WARN naming agent, session and reason and returns the embedded ping, so a
// script can never cancel a keepalive or leave a cache cold.
func (r *Runner) keepaliveScriptPrompt(ctx context.Context, script string, run keepaliveRun) string {
	out, err := r.runKeepaliveScript(ctx, script, run)
	if err != nil {
		r.log.Warnf("keepalive script %s for agent %s session %s: %v — using the default ping",
			script, r.agentID, run.sessionKey, err)
		return prompts.Keepalive()
	}
	return out
}

func (r *Runner) runKeepaliveScript(ctx context.Context, script string, run keepaliveRun) (string, error) {
	timeout := r.keepaliveScriptTimeout
	if timeout <= 0 {
		timeout = defaultKeepaliveScriptTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	isDefault := "0"
	if run.isDefault {
		isDefault = "1"
	}

	var stdout, stderr bytes.Buffer
	err := procx.RunWithETXTBSYRetry(cctx, func() *exec.Cmd {
		stdout.Reset()
		stderr.Reset()
		// SECURITY (#1351, S1): the script lives where the agent may be able to
		// write it, so it gets no more privilege than the agent's own shell
		// tool. procx.Spawn drops the foci-secrets group and puts the child in
		// its own process group. Operator, not Trusted: like
		// can_run_background (runCanRunBackground), this is an
		// operator-authored helper that may call the operator's own tools, and
		// Trusted would give it the daemon's PATH instead. See docs/WIRING.md,
		// "Keepalive prompt scripts".
		cmd := procx.Spawn(cctx, procx.Operator, script, r.agentID, run.sessionKey)
		cmd.Env = append(procx.Env(procx.Operator),
			"FOCI_AGENT_ID="+r.agentID,
			"FOCI_SESSION_KEY="+run.sessionKey,
			"FOCI_KEEPALIVE_INDEX="+strconv.Itoa(run.index),
			"FOCI_KEEPALIVE_COUNT="+strconv.Itoa(run.count),
			"FOCI_KEEPALIVE_DEFAULT="+isDefault,
		)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		// On timeout kill the whole process group (Spawn sets Setpgid), not
		// just the script: a child it started would otherwise hold the stdout
		// pipe open and Wait would block past the bound.
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = time.Second
		return cmd
	})
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out after %s", timeout)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w (stderr: %s)", err, truncateForLog(msg, 200))
		}
		return "", err
	}
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return "", errors.New("empty output")
	}
	return text, nil
}

// truncateForLog caps s at n bytes for a log line.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

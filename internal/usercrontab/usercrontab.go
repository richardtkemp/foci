// Package usercrontab reads and installs the gateway user's own crontab
// (`crontab -l` / `crontab -`), with a time limit on each call.
//
// The callers run inside chat flows (the /agents new wizard, an agent
// rename): an external command that never returns would hang the flow with
// no way out. On macOS `crontab -` can block on a privacy (TCC) prompt that
// nobody is at the screen to answer (#2976), so every call is bounded by
// Timeout and a timeout is reported as an error the caller can show.
//
// New content goes to `crontab -` on stdin, never through a shell: no
// quoting, no `echo` escape handling, no expansion of `$` in job lines.
package usercrontab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"foci/internal/procx"
)

// Timeout bounds each crontab command. A crontab read or write takes
// milliseconds; anything near this limit is waiting on something else.
// A variable only so tests can shorten it.
var Timeout = 15 * time.Second

// Bin is the crontab executable, resolved on the Trusted population's PATH.
// Overridden in tests.
var Bin = "crontab"

// Read returns the current crontab. An absent crontab reads as "" (crontab
// -l exits non-zero with "no crontab for <user>"). Any other failure is an
// error, so a caller never mistakes a failed read for an empty crontab and
// installs over the user's jobs.
func Read(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	// Trusted: foci editing its own user's crontab — its own machinery.
	cmd := procx.Spawn(ctx, procx.Trusted, Bin, "-l")
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", timeoutErr("crontab -l")
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(strings.ToLower(stderr.String()), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Install replaces the crontab with content.
func Install(ctx context.Context, content string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := procx.Spawn(ctx, procx.Trusted, Bin, "-")
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return timeoutErr("crontab -")
		}
		return fmt.Errorf("crontab -: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Append adds lines to the end of the crontab: it reads the current crontab,
// and installs it with the lines appended. A failed read installs nothing.
func Append(ctx context.Context, lines []string) error {
	cur, err := Read(ctx)
	if err != nil {
		return err
	}
	return Install(ctx, AppendLines(cur, lines))
}

// AppendLines returns crontab content cur with lines appended after a blank
// separator line, newline-terminated (cron ignores a last line without one).
func AppendLines(cur string, lines []string) string {
	var sb strings.Builder
	sb.WriteString(cur)
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
}

func timeoutErr(what string) error {
	msg := fmt.Sprintf("%s: no answer after %s, stopped", what, Timeout)
	if runtime.GOOS == "darwin" {
		msg += " (on macOS, crontab can wait on a privacy prompt: allow it, or give the foci process Full Disk Access)"
	}
	return errors.New(msg)
}

package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tmuxPane manages a tmux pane running claude interactively.
type tmuxPane struct {
	socketPath string // tmux -S socket (empty = default)
	windowName string // tmux window name (e.g. "cc-main")
	workDir    string // working directory for the pane
	pid        int    // PID of the shell process in the pane (0 = unknown)
}

// create creates a new tmux window running the claude command.
// The window is named windowName and started in workDir.
func (p *tmuxPane) create(ctx context.Context, claudeArgs []string) error {
	// Build the shell command: claude [args...]
	shellCmd := "claude"
	for _, a := range claudeArgs {
		shellCmd += " " + shellQuote(a)
	}

	args := []string{"new-window", "-d", "-n", p.windowName}
	if p.workDir != "" {
		args = append(args, "-c", p.workDir)
	}
	args = append(args, shellCmd)

	_, err := p.runTmux(ctx, args...)
	if err != nil {
		// Window might not exist in any session — create a detached session first.
		sessArgs := []string{"new-session", "-d", "-s", "foci-backend", "-n", p.windowName}
		if p.workDir != "" {
			sessArgs = append(sessArgs, "-c", p.workDir)
		}
		sessArgs = append(sessArgs, shellCmd)
		_, err = p.runTmux(ctx, sessArgs...)
		if err != nil {
			return fmt.Errorf("tmux new-session: %w", err)
		}
	}
	return nil
}

// sendKeys sends text to the pane followed by Enter.
// Uses load-buffer/paste-buffer for reliable handling of long and multi-line input.
func (p *tmuxPane) sendKeys(ctx context.Context, text string) error {
	target := p.target()

	if text != "" {
		// Pipe text into tmux's buffer via stdin (load-buffer -), then paste.
		// No temp file needed — works on Linux and macOS.
		if err := p.loadBufferFromStdin(ctx, text); err != nil {
			return fmt.Errorf("load-buffer: %w", err)
		}
		if _, err := p.runTmux(ctx, "paste-buffer", "-t", target); err != nil {
			return fmt.Errorf("paste-buffer: %w", err)
		}
	}

	// Brief pause so the TUI can process pasted input before Enter.
	time.Sleep(200 * time.Millisecond)
	if _, err := p.runTmux(ctx, "send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %w", err)
	}
	return nil
}

// sendSpecial sends a special key sequence (e.g. "C-c" for Ctrl-C).
func (p *tmuxPane) sendSpecial(ctx context.Context, key string) error {
	_, err := p.runTmux(ctx, "send-keys", "-t", p.target(), key)
	return err
}

// isAlive checks whether the tmux window still exists.
func (p *tmuxPane) isAlive(ctx context.Context) bool {
	out, err := p.runTmux(ctx, "list-windows", "-F", "#{window_name}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == p.windowName {
			return true
		}
	}
	return false
}

// readPanePID reads the PID of the process running in the pane.
// This is the shell process — claude is a child of this PID.
func (p *tmuxPane) readPanePID(ctx context.Context) (int, error) {
	out, err := p.runTmux(ctx, "list-panes", "-t", p.target(), "-F", "#{pane_pid}")
	if err != nil {
		return 0, fmt.Errorf("list-panes: %w", err)
	}
	pidStr := strings.TrimSpace(out)
	if pidStr == "" {
		return 0, fmt.Errorf("no pane PID found for %s", p.windowName)
	}
	// Take the first line if multiple panes exist.
	if idx := strings.IndexByte(pidStr, '\n'); idx > 0 {
		pidStr = pidStr[:idx]
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("parse pane PID %q: %w", pidStr, err)
	}
	return pid, nil
}

// findChildPID finds a child process of the given parent PID by reading
// /proc/<child>/stat. Returns the first child found, or 0 if none.
func findChildPID(parentPID int) (int, error) {
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}

	ppidStr := strconv.Itoa(parentPID)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only numeric directory names are PIDs.
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		statPath := filepath.Join(procDir, e.Name(), "stat")
		data, err := os.ReadFile(statPath)
		if err != nil {
			continue
		}
		// /proc/<pid>/stat format: "pid (comm) state ppid ..."
		// Find the closing paren to skip comm (which can contain spaces).
		s := string(data)
		idx := strings.LastIndex(s, ") ")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 2 {
			continue
		}
		// fields[0] = state, fields[1] = ppid
		if fields[1] == ppidStr {
			childPID, _ := strconv.Atoi(e.Name())
			return childPID, nil
		}
	}
	return 0, fmt.Errorf("no child process found for PID %d", parentPID)
}

// kill destroys the tmux window.
func (p *tmuxPane) kill(ctx context.Context) error {
	_, err := p.runTmux(ctx, "kill-window", "-t", p.target())
	return err
}

// target returns the tmux target string for the window.
func (p *tmuxPane) target() string {
	return p.windowName
}

// loadBufferFromStdin pipes text into tmux's paste buffer via stdin.
// "load-buffer -" reads from stdin, avoiding any temp file.
func (p *tmuxPane) loadBufferFromStdin(ctx context.Context, text string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	args := []string{"load-buffer", "-"}
	if p.socketPath != "" {
		args = append([]string{"-S", p.socketPath}, args...)
	}
	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	cmd.Stdin = strings.NewReader(text)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// runTmux executes a tmux command with the configured socket.
func (p *tmuxPane) runTmux(ctx context.Context, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if p.socketPath != "" {
		args = append([]string{"-S", p.socketPath}, args...)
	}
	cmd := exec.CommandContext(cmdCtx, "tmux", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// shellQuote wraps s in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

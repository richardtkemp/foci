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
	// Wrap in a login shell so the user's PATH is available (e.g. ~/.local/bin/claude).
	// tmux's default shell gets a bare system PATH that excludes user-installed binaries.
	innerCmd := "claude"
	for _, a := range claudeArgs {
		innerCmd += " " + shellQuote(a)
	}
	shellCmd := "sh -l -c " + shellQuote(innerCmd)

	// Each backend gets its own tmux session (not just a window) to prevent
	// any cross-talk between sessions (pane capture, window listing, etc.).
	args := []string{"new-session", "-d", "-s", p.windowName}
	if p.workDir != "" {
		args = append(args, "-c", p.workDir)
	}
	args = append(args, shellCmd)

	out, err := p.runTmux(ctx, args...)
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %s: %w", p.windowName, strings.TrimSpace(out), err)
	}
	return nil
}

// sendText sends text to the pane followed by Enter.
// Uses load-buffer/paste-buffer for reliable handling of long and multi-line input.
func (p *tmuxPane) sendText(ctx context.Context, text string) error {
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

// isAlive checks whether the tmux session still exists.
func (p *tmuxPane) isAlive(ctx context.Context) bool {
	_, err := p.runTmux(ctx, "has-session", "-t", p.windowName)
	return err == nil
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

// capturePane returns the visible text content of the pane.
func (p *tmuxPane) capturePane(ctx context.Context) (string, error) {
	out, err := p.runTmux(ctx, "capture-pane", "-t", p.target(), "-p")
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return out, nil
}

// permissionPrompt holds a parsed CC permission prompt.
type permissionPrompt struct {
	Description string            // tool/action description (above "Do you want to")
	Choices     []promptChoice    // numbered choices
	Raw         string            // full block text (for dedup)
}

type promptChoice struct {
	Number string // "1", "2", "3"
	Label  string // "Yes", "Yes, allow all edits in tmp/...", "No"
}

// extractPermissionPrompt checks if the pane is showing a CC permission prompt.
// Requires BOTH "Do you want to " AND "Esc to cancel" to be present — this avoids
// false positives from tool output, commit messages, or other text.
func extractPermissionPrompt(paneContent string) *permissionPrompt {
	if !strings.Contains(paneContent, "Esc to cancel") {
		return nil
	}
	idx := strings.Index(paneContent, "Do you want to ")
	if idx < 0 {
		return nil
	}

	// Walk backward from the "Do you want to" line to find the description.
	// CC renders a horizontal rule (─) above the prompt block.
	start := idx
	lines := strings.Split(paneContent[:idx], "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "───") {
			start = 0
			for j := 0; j <= i; j++ {
				start += len(lines[j]) + 1
			}
			break
		}
	}

	// Walk forward to capture choices after the prompt question.
	rest := paneContent[idx:]
	end := strings.Index(rest, "Esc to cancel")
	if end < 0 {
		end = len(rest)
	}

	block := strings.TrimSpace(paneContent[start : idx+end])

	// Extract the description: everything from start to the "Do you want to" line.
	desc := strings.TrimSpace(paneContent[start:idx])

	// Parse numbered choices from the block. CC formats them as:
	//   1. Yes
	//   2. Yes, allow all edits in tmp/ during this session (shift+tab)
	//   3. No
	var choices []promptChoice
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		// Strip the ❯ cursor prefix if present.
		line = strings.TrimPrefix(line, "❯ ")
		line = strings.TrimSpace(line)
		// Match "N. label" pattern.
		if len(line) >= 3 && line[0] >= '1' && line[0] <= '9' && line[1] == '.' && line[2] == ' ' {
			choices = append(choices, promptChoice{
				Number: string(line[0]),
				Label:  line[3:],
			})
		}
	}

	return &permissionPrompt{
		Description: desc,
		Choices:     choices,
		Raw:         block,
	}
}

// kill destroys the tmux session.
func (p *tmuxPane) kill(ctx context.Context) error {
	_, err := p.runTmux(ctx, "kill-session", "-t", p.windowName)
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

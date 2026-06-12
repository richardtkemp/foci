package cctmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/procx"
)

// tmuxExecFunc executes a tmux invocation with the given args (socket flags
// already included) and optional stdin, returning combined output. The
// production implementation spawns a real tmux subprocess; tests inject fakes.
type tmuxExecFunc func(ctx context.Context, stdin string, args ...string) (string, error)

// execTmuxProc is the production tmuxExecFunc: runs tmux in its own session
// group via procx with a 5-second timeout.
func execTmuxProc(ctx context.Context, stdin string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := procx.SpawnSetsid(cmdCtx, "tmux", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// enterRetryDelays is the exponential backoff schedule for re-sending Enter
// after pasting a prompt (see sendText for why retries are needed).
var enterRetryDelays = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

// tmuxPane manages a tmux pane running claude interactively.
type tmuxPane struct {
	socketPath string       // tmux -S socket (empty = default)
	windowName string       // tmux window name (e.g. "cc-main")
	workDir    string       // working directory for the pane
	pid        int          // PID of the shell process in the pane (0 = unknown)
	cols       int          // window width (0 = tmux default 80)
	rows       int          // window height (0 = tmux default 24)
	exec       tmuxExecFunc // tmux subprocess runner (nil = execTmuxProc)
}

// create creates a new tmux session running the claude command.
// envVars are KEY=VALUE pairs exported before claude starts so child
// processes (e.g. CC's Bash tool) inherit them.
func (p *tmuxPane) create(ctx context.Context, claudeArgs []string, envVars ...string) error {
	// Wrap in a login shell so the user's PATH is available.
	innerCmd := "claude"
	for _, a := range claudeArgs {
		innerCmd += " " + shellQuote(a)
	}
	// Prepend env var exports (quoted to prevent injection).
	var envPrefix string
	for _, kv := range envVars {
		envPrefix += "export " + shellQuote(kv) + "; "
	}
	shellCmd := "sh -l -c " + shellQuote(envPrefix+innerCmd)

	// Each backend gets its own tmux session (not just a window) to prevent
	// any cross-talk between sessions (pane capture, window listing, etc.).
	args := []string{"new-session", "-d", "-s", p.windowName}
	if p.cols > 0 && p.rows > 0 {
		args = append(args, "-x", fmt.Sprintf("%d", p.cols), "-y", fmt.Sprintf("%d", p.rows))
	}
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
// Short inputs (≤10 chars, single line) use send-keys -l for TUI compatibility.
// Longer inputs use load-buffer/paste-buffer for reliability.
func (p *tmuxPane) sendText(ctx context.Context, text string) error {
	target := p.target()

	if text != "" {
		if len(text) <= 10 && !strings.Contains(text, "\n") {
			// Short single-line input: use send-keys -l (literal) which works
			// correctly with TUI selection prompts that consume keypresses.
			if _, err := p.runTmux(ctx, "send-keys", "-t", target, "-l", text); err != nil {
				return fmt.Errorf("send-keys literal: %w", err)
			}
		} else {
			// Longer or multi-line input: pipe via load-buffer/paste-buffer.
			if err := p.loadBufferFromStdin(ctx, text); err != nil {
				return fmt.Errorf("load-buffer: %w", err)
			}
			if _, err := p.runTmux(ctx, "paste-buffer", "-t", target); err != nil {
				return fmt.Errorf("paste-buffer: %w", err)
			}
		}
	}

	// Send Enter with exponential backoff. On first message after startup,
	// CC's TUI sometimes accepts pasted text but doesn't register the Enter.
	// Retrying at increasing intervals catches slow TUI initialization.
	// Extra Enters are harmless — CC ignores them while processing, and
	// empty submits on the ❯ prompt are no-ops.
	for _, delay := range enterRetryDelays {
		time.Sleep(delay)
		if _, err := p.runTmux(ctx, "send-keys", "-t", target, "Enter"); err != nil {
			return fmt.Errorf("send-keys Enter: %w", err)
		}
	}
	return nil
}

// sendSpecial sends a special key sequence (e.g. "C-c" for Ctrl-C).
// Uses send-keys without -l so tmux interprets the key name.
func (p *tmuxPane) sendSpecial(ctx context.Context, key string) error {
	_, err := p.runTmux(ctx, "send-keys", "-t", p.target(), key)
	return err
}

// sendKeystroke sends a single literal character as a keypress.
// Unlike sendText (which uses paste-buffer), this sends via send-keys -l
// without Enter — suitable for TUI selection prompts that react to keypresses.
func (p *tmuxPane) sendKeystroke(ctx context.Context, key string) error {
	_, err := p.runTmux(ctx, "send-keys", "-t", p.target(), "-l", key)
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
	// -S -500: capture 500 lines of scrollback, not just the visible area.
	// Permission prompts for large edits push the diff above the visible
	// window — scrollback ensures we capture the full content.
	out, err := p.runTmux(ctx, "capture-pane", "-t", p.target(), "-p", "-S", "-500")
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return out, nil
}

// permissionPrompt holds a parsed CC permission prompt.
type permissionPrompt struct {
	Description string         // tool/action description (above "Do you want to")
	Summary     string         // short human-readable summary (e.g. "Edit memory/2026-03-27.md")
	Choices     []promptChoice // numbered choices
	Raw         string         // full block text (for dedup)
}

type promptChoice struct {
	Number string // "1", "2", "3"
	Label  string // "Yes", "Yes, allow all edits in tmp/...", "No"
}

// extractPermissionPrompt checks if the pane is showing a CC permission prompt.
// Requires BOTH "Do you want to " AND "Esc to cancel" to be present — this avoids
// false positives from tool output, commit messages, or other text.
func extractPermissionPrompt(paneContent string) *permissionPrompt {
	// Use LastIndex — with scrollback capture, earlier prompts may still
	// be in the buffer. We want the most recent one.
	escIdx := strings.LastIndex(paneContent, "Esc to cancel")
	if escIdx < 0 {
		return nil
	}
	idx := strings.LastIndex(paneContent[:escIdx], "Do you want to ")
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

	// No numbered choices found → not a real permission prompt.
	// CC always shows at least "1. Yes" and "3. No". Without choices,
	// this is a false positive from scrollback content containing
	// "Do you want to" and "Esc to cancel" strings.
	if len(choices) == 0 {
		return nil
	}

	return &permissionPrompt{
		Description: desc,
		Summary:     buildPermissionSummary(desc),
		Choices:     choices,
		Raw:         block,
	}
}

// buildPermissionSummary extracts a short summary from the permission description.
// CC formats descriptions as:
//
//	"Bash command\n\n   cd ... && go vet\n   Run go vet on backend"
//	"Edit file\n memory/2026-03-27.md\n╌╌╌\n diff\n╌╌╌"
//	"Create file\n ../../../tmp/test.txt\n╌╌╌\n content\n╌╌╌"
//	"Read file\n path/to/file"
//
// Returns e.g. "Bash: Run go vet on backend", "Edit memory/2026-03-27.md".
func buildPermissionSummary(desc string) string {
	lines := strings.Split(desc, "\n")
	// Collect non-empty, non-divider lines.
	var clean []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "╌") || strings.HasPrefix(t, "─") {
			if len(clean) > 0 {
				break // stop at first divider after content (skip diff body)
			}
			continue
		}
		clean = append(clean, t)
	}
	if len(clean) == 0 {
		return ""
	}

	// First line is the tool header: "Bash command", "Edit file", "Create file", etc.
	header := clean[0]
	if len(clean) == 1 {
		return header
	}

	// For file operations, the second line is the filename.
	// For Bash, the last clean line is the description.
	target := clean[len(clean)-1]

	switch {
	case strings.HasPrefix(header, "Edit"), strings.HasPrefix(header, "Create"),
		strings.HasPrefix(header, "Read"), strings.HasPrefix(header, "Write"):
		// Use the filename (second line), strip relative path prefixes.
		fname := clean[1]
		fname = strings.TrimPrefix(fname, "../")
		fname = strings.TrimPrefix(fname, "../")
		fname = strings.TrimPrefix(fname, "../")
		return header + " " + fname
	case strings.HasPrefix(header, "Bash"):
		return target
	default:
		return header + " — " + target
	}
}

// setEnv sets an environment variable on the tmux session so new processes
// spawned in this session inherit it.
func (p *tmuxPane) setEnv(ctx context.Context, key, value string) error {
	_, err := p.runTmux(ctx, "set-environment", "-t", p.windowName, key, value)
	return err
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
	out, err := p.runTmuxStdin(ctx, text, "load-buffer", "-")
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(out), err)
	}
	return nil
}

// runTmux executes a tmux command with the configured socket.
func (p *tmuxPane) runTmux(ctx context.Context, args ...string) (string, error) {
	return p.runTmuxStdin(ctx, "", args...)
}

// runTmuxStdin executes a tmux command with the configured socket, feeding
// stdin to the process when non-empty. All tmux invocations funnel through
// here so the socket flag and subprocess mechanics live in one place.
func (p *tmuxPane) runTmuxStdin(ctx context.Context, stdin string, args ...string) (string, error) {
	if p.socketPath != "" {
		args = append([]string{"-S", p.socketPath}, args...)
	}
	run := p.exec
	if run == nil {
		run = execTmuxProc
	}
	return run(ctx, stdin, args...)
}

// shellQuote wraps s in single quotes for shell safety.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

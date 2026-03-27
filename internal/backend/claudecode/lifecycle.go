package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/internal/backend"
)

func (b *Backend) Start(ctx context.Context, opts backend.StartOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.agentID = opts.AgentID
	b.workDir = opts.WorkDir

	label := opts.Label
	if label == "" {
		label = opts.AgentID
	}
	windowName := "cc-" + label

	// Build claude command arguments.
	var args []string
	if opts.SystemPrompt != "" {
		// Write system prompt to a file — it can be arbitrarily large,
		// exceeding tmux's command length limit if passed inline.
		promptFile := filepath.Join(opts.WorkDir, "character", ".full-prompt")
		if err := os.WriteFile(promptFile, []byte(opts.SystemPrompt), 0600); err != nil {
			return fmt.Errorf("write system prompt file: %w", err)
		}
		args = append(args, "--system-prompt-file", promptFile)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}

	// Permission handling. skip_permissions bypasses all prompts (unattended).
	// allowed_tools pre-approves specific tools but CC may still prompt for
	// directory access etc. — those are detected and forwarded to the user.
	if v, ok := b.cfg["skip_permissions"].(bool); ok && v {
		args = append(args, "--dangerously-skip-permissions")
	}
	if v, ok := b.cfg["allowed_tools"].(string); ok && v != "" {
		args = append(args, "--allowedTools", v)
	}

	// Create the tmux pane.
	b.pane = &tmuxPane{
		socketPath: b.socketPath,
		windowName: windowName,
		workDir:    opts.WorkDir,
	}

	// Check if a window already exists (crash recovery).
	if b.pane.isAlive(ctx) {
		// Reuse existing window — check if claude is still running.
		pid, err := b.pane.readPanePID(ctx)
		if err == nil && pid > 0 {
			b.pane.pid = pid
			b.discoverSession() // best-effort; falls back to lazy discovery
			return nil
		}
		// Process is gone — kill stale window and recreate.
		_ = b.pane.kill(ctx)
	}

	if err := b.pane.create(ctx, args); err != nil {
		return fmt.Errorf("create tmux pane: %w", err)
	}

	// Read the pane PID (the login shell). Claude is a child of this.
	panePID, err := b.pane.readPanePID(ctx)
	if err != nil {
		return fmt.Errorf("read pane PID: %w", err)
	}
	b.pane.pid = panePID

	// Session discovery is deferred — CC may not write the session file
	// until the first message is received. The watcher is created lazily
	// on the first SendTurn call via ensureWatcher.
	return nil
}

// ensureWatcher discovers the CC session and creates the file watcher
// if not already set up. Called lazily on the first SendTurn — CC may not
// write the session file until it receives a message. Polls with a timeout.
func (b *Backend) ensureWatcher(ctx context.Context) error {
	if b.watcher != nil {
		return nil
	}

	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.Done():
			return fmt.Errorf("timeout waiting for claude session (30s)")
		case <-ticker.C:
			childPID, err := findChildPID(b.pane.pid)
			if err != nil {
				continue
			}
			sessionID, jsonlPath, err := discoverSessionFile(childPID, b.workDir)
			if err != nil {
				continue
			}
			if err := b.startWatcher(jsonlPath); err != nil {
				continue
			}
			b.sessionID = sessionID
			return nil
		}
	}
}

// discoverSession tries to find an existing CC session and start the watcher.
// If the session file doesn't exist yet, defers to lazy discovery via ensureWatcher.
func (b *Backend) discoverSession() {
	sessionID, jsonlPath, err := discoverSessionFile(b.pane.pid, b.workDir)
	if err != nil {
		return // will be discovered lazily on first SendTurn
	}
	if err := b.startWatcher(jsonlPath); err != nil {
		return
	}
	b.sessionID = sessionID
}

// startWatcher initializes the JSONL file watcher and starts the
// long-lived watch loop goroutine.
func (b *Backend) startWatcher(jsonlPath string) error {
	w, err := newSessionWatcher(jsonlPath)
	if err != nil {
		return fmt.Errorf("init watcher: %w", err)
	}

	// Periodically check the tmux pane for permission prompts.
	w.onPermissionCheck = func() {
		b.checkPermissionPrompt()
	}

	b.watcher = w

	// Set persistent handler that delegates to the current turn's replyFunc.
	w.setHandler(&backend.EventHandler{
		OnText: func(text string) {
			b.replyMu.Lock()
			fn := b.replyFunc
			b.replyMu.Unlock()
			if fn != nil {
				fn(text)
			}
		},
	})

	// Start long-lived watch loop — lives until Close() cancels watchCtx.
	b.watchCtx, b.watchStop = context.WithCancel(context.Background())
	go w.watchLoop(b.watchCtx)
	return nil
}

// checkPermissionPrompt captures the tmux pane and checks for a CC permission
// prompt. If found (and not a duplicate), forwards it to the user via the
// platform with inline keyboard choices. Called periodically by the watcher loop.
func (b *Backend) checkPermissionPrompt() {
	pane := b.pane
	if pane == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	content, err := pane.capturePane(ctx)
	if err != nil {
		return
	}
	prompt := extractPermissionPrompt(content)
	if prompt == nil {
		return
	}

	// Skip if this is the same prompt we already sent.
	b.lastPromptMu.Lock()
	if prompt.Raw == b.lastPrompt {
		b.lastPromptMu.Unlock()
		return
	}
	b.lastPrompt = prompt.Raw
	b.lastPromptMu.Unlock()

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	replyFn := b.replyFunc
	b.replyMu.Unlock()

	// Prefer structured prompt with keyboard; fall back to plain text.
	if promptFn != nil && len(prompt.Choices) > 0 {
		var choices []backend.PromptChoice
		for _, c := range prompt.Choices {
			choices = append(choices, backend.PromptChoice{
				Label: c.Label,
				Data:  c.Number,
			})
		}
		promptFn("⚠️ Permission required:\n\n"+prompt.Description, choices)
	} else if replyFn != nil {
		replyFn("⚠️ Claude Code needs permission:\n\n" + prompt.Raw + "\n\nReply with your choice (1, 2, 3, etc.)")
	}
}

// clearLastPrompt resets the deduplication state so the next permission
// prompt is always forwarded. Called when user input is sent to the pane.
func (b *Backend) clearLastPrompt() {
	b.lastPromptMu.Lock()
	b.lastPrompt = ""
	b.lastPromptMu.Unlock()
}

func (b *Backend) IsRunning() bool {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()

	if pane == nil {
		return false
	}
	return pane.isAlive(context.Background())
}

func (b *Backend) Restart(ctx context.Context) error {
	if err := b.Close(); err != nil {
		return fmt.Errorf("close before restart: %w", err)
	}
	return b.Start(ctx, backend.StartOptions{
		WorkDir: b.workDir,
		AgentID: b.agentID,
	})
}

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Stop the watch loop and close fsnotify.
	if b.watchStop != nil {
		b.watchStop()
		b.watchStop = nil
	}
	if b.watcher != nil {
		b.watcher.close()
	}

	if b.pane == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try graceful exit first.
	_ = b.pane.sendText(ctx, "/exit")
	time.Sleep(500 * time.Millisecond)

	// Force kill if still alive.
	if b.pane.isAlive(ctx) {
		_ = b.pane.sendSpecial(ctx, "C-c")
		time.Sleep(200 * time.Millisecond)
		_ = b.pane.kill(ctx)
	}

	b.pane = nil
	b.watcher = nil
	return nil
}

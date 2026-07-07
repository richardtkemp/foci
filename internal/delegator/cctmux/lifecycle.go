package cctmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

func (b *Backend) Start(ctx context.Context, opts delegator.StartOptions) error {
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
	if delegator.SkipPermissions(b.cfg) {
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
		cols:       opts.TmuxCols,
		rows:       opts.TmuxRows,
		exec:       b.tmuxExec,
	}

	// Build env vars to inject into the tmux pane from StartOptions.Env.
	// The exec bridge (BASH_ENV, FOCI_SOCK) is created by DelegatedManager
	// and passed here via opts.Env so all backend types get it automatically.
	var paneEnv []string
	for k, v := range opts.Env {
		paneEnv = append(paneEnv, k+"="+v)
	}

	// Check if a session already exists (crash recovery).
	if b.pane.isAlive(ctx) {
		pid, err := b.pane.readPanePID(ctx)
		if err == nil && pid > 0 {
			b.pane.pid = pid
			// Inject env vars into the recovered session via
			// tmux set-environment so new shell commands pick them up.
			for k, v := range opts.Env {
				_ = b.pane.setEnv(ctx, k, v)
			}
			b.discoverSession()
			return nil
		}
		_ = b.pane.kill(ctx)
	}

	if err := b.pane.create(ctx, args, paneEnv...); err != nil {
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
	// on the first SendToPane call via ensureWatcher.
	return nil
}

// ensureWatcher discovers the CC session and creates the file watcher
// if not already set up. Called lazily on the first SendToPane — CC may not
// write the session file until it receives a message. Polls with a timeout.
//
// Safe to call without b.mu held — uses its own lock for the nil check.
// The discovery loop and startWatcher run outside any lock to avoid blocking
// other goroutines that need b.mu (e.g. SessionFilePath from RegisterSessionIndex).
func (b *Backend) ensureWatcher(ctx context.Context) error {
	b.mu.Lock()
	if b.watcher != nil {
		b.mu.Unlock()
		return nil
	}
	if b.watcherStarting {
		b.mu.Unlock()
		// Another goroutine is already discovering — wait for it.
		for {
			time.Sleep(100 * time.Millisecond)
			b.mu.Lock()
			if b.watcher != nil || !b.watcherStarting {
				b.mu.Unlock()
				return nil
			}
			b.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	b.watcherStarting = true
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.watcherStarting = false
		b.mu.Unlock()
	}()

	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var lastErr string
	for {
		select {
		case <-deadline.Done():
			return fmt.Errorf("timeout waiting for claude session (30s) — last: %s", lastErr)
		case <-ticker.C:
			// Check if the pane is still alive — if CC exited (e.g. bad --resume UUID),
			// there's no point waiting.
			if !b.pane.isAlive(ctx) {
				log.Warnf("backend/cc", "claude process exited (tmux session gone), last: %s", lastErr)
				return fmt.Errorf("claude process exited — check tmux logs (last: %s)", lastErr)
			}

			childPID, err := findChildPID(b.pane.pid)
			if err != nil {
				lastErr = "no child process: " + err.Error()
				continue
			}
			sessionID, jsonlPath, err := discoverSessionFile(childPID, b.workDir)
			if err != nil {
				lastErr = fmt.Sprintf("pid %d: %v", childPID, err)
				continue
			}
			if err := b.startWatcher(jsonlPath); err != nil {
				lastErr = "watcher: " + err.Error()
				continue
			}
			b.mu.Lock()
			b.sessionID = sessionID
			b.mu.Unlock()
			log.Infof("backend/cc", "session discovered: %s (pid %d)", sessionID, childPID)
			if b.onSessionReady != nil {
				b.onSessionReady(sessionID)
			}
			return nil
		}
	}
}

// discoverSession tries to find an existing CC session and start the watcher.
// If the session file doesn't exist yet, defers to lazy discovery via ensureWatcher.
func (b *Backend) discoverSession() {
	sessionID, jsonlPath, err := discoverSessionFile(b.pane.pid, b.workDir)
	if err != nil {
		return // will be discovered lazily on first SendToPane
	}
	if err := b.startWatcher(jsonlPath); err != nil {
		return
	}
	b.sessionID = sessionID
}

// startWatcher initializes the JSONL file watcher and starts the
// long-lived watch loop goroutine.
func (b *Backend) startWatcher(jsonlPath string) error {
	b.mu.Lock()
	offset := b.preSendOffset
	b.mu.Unlock()

	w, err := newSessionWatcher(jsonlPath, offset)
	if err != nil {
		return fmt.Errorf("init watcher: %w", err)
	}

	// Periodically check the tmux pane for permission prompts.
	w.onPermissionCheck = func() {
		b.checkPermissionPrompt()
	}

	// Forward subagent spawn/completion status to the user through the
	// session-scoped delivery sink (same pipeline as ordinary text). The tracker
	// now emits a DETAIL string (running descriptions, or "" when none), so this
	// caller owns the human-facing wording.
	w.agents.OnStatus = func(detail string) {
		se := b.sessionEvents.Load()
		if se == nil || se.OnText == nil {
			return
		}
		if detail == "" {
			se.OnText("✅ Subagents complete")
		} else {
			se.OnText("🔄 Subagents running: " + detail)
		}
	}

	b.mu.Lock()
	b.watcher = w
	b.preSendOffset = -1 // consumed — future watchers default to tail
	b.mu.Unlock()

	// The watcher dispatches to the Backend, which routes delivery to the
	// session-scoped SessionEvents and completion to the per-turn TurnEvents.
	// Set once for the watcher's lifetime — not per turn — so delivery never
	// drops between turns.
	w.setEvents(b)

	// Start long-lived watch loop — lives until Close() cancels watchCtx.
	b.mu.Lock()
	b.watchCtx, b.watchStop = context.WithCancel(context.Background())
	b.mu.Unlock()
	go w.watchLoop(b.watchCtx)

	// Read any entries written between recordPreSendOffset and now.
	// fsnotify only fires for writes AFTER the watcher is added, so
	// entries written in the gap (e.g. CC completing a turn before the
	// watcher started) would be missed without this initial read.
	w.mu.Lock()
	e := w.events
	w.mu.Unlock()
	if e != nil {
		w.readNew(e)
	}

	return nil
}

// checkPermissionPrompt captures the tmux pane and checks for a CC permission
// prompt. Tracks state transitions: fires the prompt callback when a prompt
// appears, and fires onPermCleared when it disappears (user responded, CC
// timed out, or Escape was pressed). Called periodically by the watcher loop.
//
// This drives permission-gated message queuing: while a permission prompt is
// visible, DelegatedManager.WaitForPermission blocks incoming messages and
// injections (sending text to the tmux pane during a prompt would corrupt the TUI).
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

	b.lastPromptMu.Lock()
	wasActive := b.permissionActive

	if prompt == nil {
		// No prompt visible. If one was active, fire the cleared callback.
		if wasActive {
			b.permissionActive = false
			b.lastPrompt = ""
			clearedFn := b.onPermCleared
			b.lastPromptMu.Unlock()
			if clearedFn != nil {
				clearedFn()
			}
		} else {
			b.lastPromptMu.Unlock()
		}
		return
	}

	// Prompt is visible. Skip if this is the same prompt we already sent.
	if prompt.Raw == b.lastPrompt {
		b.lastPromptMu.Unlock()
		return
	}
	b.lastPrompt = prompt.Raw
	b.permissionActive = true
	b.lastPromptMu.Unlock()

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	b.replyMu.Unlock()

	// Prefer structured prompt with keyboard; fall back to plain text through
	// the session-scoped delivery sink.
	if promptFn != nil && len(prompt.Choices) > 0 {
		var choices []delegator.PromptChoice
		for _, c := range prompt.Choices {
			choices = append(choices, delegator.PromptChoice{
				Label: c.Label,
				Data:  c.Number,
			})
		}
		promptFn("", "⚠️ Permission required:\n\n"+prompt.Description, prompt.Summary, "", choices)
	} else if se := b.sessionEvents.Load(); se != nil && se.OnText != nil {
		se.OnText("⚠️ Claude Code needs permission:\n\n" + prompt.Raw + "\n\nReply with your choice (1, 2, 3, etc.)")
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

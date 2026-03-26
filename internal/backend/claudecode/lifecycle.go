package claudecode

import (
	"context"
	"fmt"
	"time"

	"foci/internal/backend"
)

func (b *Backend) Start(ctx context.Context, opts backend.StartOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.agentID = opts.AgentID
	b.workDir = opts.WorkDir

	windowName := "cc-" + opts.AgentID

	// Build claude command arguments.
	var args []string
	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
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
			return b.discoverSession(ctx)
		}
		// Process is gone — kill stale window and recreate.
		_ = b.pane.kill(ctx)
	}

	if err := b.pane.create(ctx, args); err != nil {
		return fmt.Errorf("create tmux pane: %w", err)
	}

	// Read the pane PID — this is the shell PID, not claude's PID.
	// Claude is a child of this shell. We need to wait for claude to
	// create its PID file. The PID file name is claude's PID, which
	// we can discover by waiting for a new file in ~/.claude/sessions/.
	panePID, err := b.pane.readPanePID(ctx)
	if err != nil {
		return fmt.Errorf("read pane PID: %w", err)
	}
	b.pane.pid = panePID

	// Wait for claude to start and create its session.
	// Claude is launched by the shell in the tmux pane, so it's a child
	// of panePID. We poll for the child's PID file.
	if err := b.waitForSession(ctx); err != nil {
		return fmt.Errorf("wait for session: %w", err)
	}

	return nil
}

// waitForSession waits for the Claude Code session file to appear and
// sets up the watcher. It finds the claude child process of the tmux pane
// shell and looks up its PID file in ~/.claude/sessions/.
func (b *Backend) waitForSession(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.Done():
			return fmt.Errorf("timeout waiting for claude session (30s)")
		case <-ticker.C:
			// Find the claude process (child of the tmux pane shell).
			childPID, err := findChildPID(b.pane.pid)
			if err != nil {
				continue // claude hasn't started yet
			}
			// Look up its session file.
			sessionID, jsonlPath, err := discoverSessionFile(childPID, b.workDir)
			if err != nil {
				continue // session file not written yet
			}
			b.sessionID = sessionID
			return b.startWatcher(jsonlPath)
		}
	}
}

// discoverSession finds an existing CC session and starts the watcher.
func (b *Backend) discoverSession(_ context.Context) error {
	sessionID, jsonlPath, err := discoverSessionFile(b.pane.pid, b.workDir)
	if err != nil {
		return fmt.Errorf("discover session: %w", err)
	}
	b.sessionID = sessionID
	return b.startWatcher(jsonlPath)
}

// startWatcher initializes the JSONL file watcher.
func (b *Backend) startWatcher(jsonlPath string) error {
	w, err := newSessionWatcher(jsonlPath)
	if err != nil {
		return fmt.Errorf("init watcher: %w", err)
	}
	b.watcher = w
	return nil
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

	// Stop the watcher.
	if b.watchStop != nil {
		b.watchStop()
		b.watchStop = nil
	}

	if b.pane == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try graceful exit first.
	_ = b.pane.sendKeys(ctx, "/exit")
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

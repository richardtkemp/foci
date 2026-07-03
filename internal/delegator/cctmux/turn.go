package cctmux

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// WaitForTurn blocks until the watcher observes the next turn completion
// (assistant message with stop_reason "end_turn"). Returns ctx.Err() on
// cancellation or deadline. Safe to call after Inject — if the turn
// completes between Inject returning and WaitForTurn being called, the
// signal is buffered and WaitForTurn returns immediately.
func (b *Backend) WaitForTurn(ctx context.Context) error {
	ch := make(chan struct{}, 1)
	b.waitMu.Lock()
	b.waitCh = ch
	b.waitMu.Unlock()

	defer func() {
		b.waitMu.Lock()
		b.waitCh = nil
		b.waitMu.Unlock()
	}()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notifyTurnComplete signals any active WaitForTurn caller.
func (b *Backend) notifyTurnComplete() {
	b.waitMu.Lock()
	ch := b.waitCh
	b.waitMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WaitReady blocks until Claude Code's TUI is ready to accept prompts.
// Detected by scraping the tmux pane for the "❯" input prompt. CC shows
// its "Claude Code" banner early in startup, but the input prompt only
// appears after session loading, tool init, and auth are complete.
func (b *Backend) WaitReady(ctx context.Context) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code-tmux backend not started")
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Claude Code ready: %w", ctx.Err())
		case <-ticker.C:
			content, err := pane.capturePane(ctx)
			if err != nil {
				continue
			}
			if strings.Contains(content, "❯") {
				return nil
			}
		}
	}
}

// CheckReady reports the tmux backend as always ready. It drives an
// interactive `claude` TUI whose own login flow is handled out of band, so foci
// performs no programmatic auth gate here. See delegator.Delegator.CheckReady.
func (b *Backend) CheckReady(_ context.Context) (bool, error) {
	return true, nil
}

// sendToPane is the internal begin-turn primitive on the tmux backend.
// Pastes the prompt into the Claude Code pane and installs the per-turn
// TurnEvents (bookkeeping). Delivery flows asynchronously through the watcher
// into the session-scoped SessionEvents; callers reach turn-start through
// Inject. exclusive (SourceSystem) makes the install conditional on idle —
// the check and install happen under one turnMu hold, so system text can
// never clobber a turn that began in a race window (ErrTurnInFlight instead).
func (b *Backend) sendToPane(ctx context.Context, prompt string, turn *delegator.TurnEvents, exclusive bool) error {
	b.mu.Lock()
	if b.pane == nil {
		b.mu.Unlock()
		return fmt.Errorf("claude-code backend not started")
	}
	pane := b.pane
	b.mu.Unlock()

	// Install the per-turn bookkeeping callbacks for this turn.
	b.turnMu.Lock()
	if exclusive && b.turnEvents != nil {
		b.turnMu.Unlock()
		return delegator.ErrTurnInFlight
	}
	b.turnEvents = turn
	b.turnMu.Unlock()

	// Clear permission prompt dedup so the next prompt is forwarded.
	b.clearLastPrompt()

	// Record the JSONL file offset BEFORE sending, so the watcher
	// starts from here and catches any response written between
	// sendText and watcher start.
	b.recordPreSendOffset()

	// Send the prompt to the pane.
	if err := pane.sendText(ctx, prompt); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}

	// Signal typing indicator — CC is now working. TypingFunc propagates
	// both true (here) and false (fireTurnComplete). For backend turns,
	// processAgentMessage has already returned, so TypingFunc owns the
	// typing lifecycle from this point until end_turn.
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(true)
	}

	// Lazily discover the session and start the long-lived watch loop.
	// ensureWatcher may take seconds (session discovery + JSONL catchup).
	// Do NOT hold b.mu across this — other goroutines need it for
	// SessionFilePath, SessionID, etc. The method uses its own internal
	// sync.Once to prevent concurrent discovery.
	if err := b.ensureWatcher(ctx); err != nil {
		return fmt.Errorf("session discovery: %w", err)
	}

	return nil
}

// fireTurnComplete fires the per-turn callback (if set) with the given
// result, then nils it (one-shot). Also notifies any WaitForTurn caller.
// Stops the typing indicator via TypingFunc(false) — for backend turns,
// processAgentMessage has already returned so this is the correct place
// to stop typing (on end_turn, when CC is actually done).
func (b *Backend) fireTurnComplete(result *delegator.TurnResult) {
	// Stop typing indicator — CC's turn is done.
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(false)
	}

	// Per-turn bookkeeping (one-shot): capture-and-nil under the lock, fire
	// outside it. Mirrors ccstream's OnResult capture-then-clear.
	b.turnMu.Lock()
	turn := b.turnEvents
	b.turnEvents = nil
	b.turnMu.Unlock()
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	// Legacy WaitForTurn signal.
	b.notifyTurnComplete()
}

// recordPreSendOffset records the current JSONL file size so the watcher
// can start from this position. If the session file doesn't exist yet
// (first turn, CC hasn't created it), records 0 to read from the beginning.
func (b *Backend) recordPreSendOffset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.watcher != nil {
		return // watcher already running, offset doesn't matter
	}

	// Try to find the session file via the existing discovery path.
	// If we can't find it, default to -1 (tail from end of file) rather
	// than 0 (beginning) — replaying the entire session history would
	// flood the user with every past response.
	childPID, err := findChildPID(b.pane.pid)
	if err != nil {
		log.Warnf("backend/cc", "recordPreSendOffset: findChildPID(%d) failed: %v", b.pane.pid, err)
		return // preSendOffset stays at -1 (default)
	}
	_, jsonlPath, err := discoverSessionFile(childPID, b.workDir)
	if err != nil {
		log.Warnf("backend/cc", "recordPreSendOffset: discoverSessionFile(pid=%d) failed: %v", childPID, err)
		return // preSendOffset stays at -1 (default)
	}
	info, err := os.Stat(jsonlPath)
	if err != nil {
		b.preSendOffset = 0
		return
	}
	b.preSendOffset = info.Size()
	log.Debugf("backend/cc", "recorded pre-send offset: %d bytes", b.preSendOffset)
}

func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.permPromptFunc = fn
}

func (b *Backend) SetOnPromptsCleared(fn func()) {
	b.lastPromptMu.Lock()
	defer b.lastPromptMu.Unlock()
	b.onPermCleared = fn
}

// RegisterPromptCancelListener is a no-op for the legacy tmux backend. Tmux
// state is polled rather than event-driven, so it doesn't expose per-prompt
// cancellations the way ccstream does. The Telegram-button race that
// motivated the hook can't occur here because tmux backends don't use
// inline keyboards in the same way.
func (b *Backend) RegisterPromptCancelListener(requestID string, fn func(reason string)) {}

func (b *Backend) SetOnSessionReady(fn func(string)) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.onSessionReady = fn
}

func (b *Backend) SetTypingFunc(fn func(bool)) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	b.typingFunc = fn
}

// AttachSessionEvents installs the session-scoped delivery callbacks. The
// JSONL watcher reads them lock-free on every dispatch (atomic.Pointer), so
// delivery never drops between turns. Idempotent — re-attach replaces. Mirrors
// ccstream; the agent calls this once per RunInference.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// onText/onToolStart/onToolEnd/onTurnComplete implement watchEvents: the
// watcher calls them as it parses the session JSONL. Delivery routes to the
// session-scoped SessionEvents; completion routes to the per-turn TurnEvents.

func (b *Backend) onText(text string) {
	// Re-establish typing — CC is actively producing output. By the time the
	// watcher sees CC's first output, the inbound message's handler has
	// returned and stopped typing; restart it for the turn's duration
	// (fireTurnComplete stops it).
	b.replyMu.Lock()
	typFn := b.typingFunc
	b.replyMu.Unlock()
	if typFn != nil {
		typFn(true)
	}
	if se := b.sessionEvents.Load(); se != nil && se.OnText != nil {
		se.OnText(text)
	}
}

func (b *Backend) onToolStart(id, name, input string) {
	if se := b.sessionEvents.Load(); se != nil && se.OnToolStart != nil {
		se.OnToolStart(id, name, input)
	}
}

func (b *Backend) onToolEnd(id, name, output string, isError bool) {
	if se := b.sessionEvents.Load(); se != nil && se.OnToolEnd != nil {
		se.OnToolEnd(id, name, output, isError)
	}
}

func (b *Backend) onTurnComplete(result *delegator.TurnResult) {
	b.fireTurnComplete(result)
}

func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

func (b *Backend) SessionFilePath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watcher != nil {
		return b.watcher.path
	}
	return ""
}

// withPane resolves the active pane under the lock and invokes fn with it, or
// returns a "backend not started" error if no pane exists yet. Shared by the
// keystroke/special-key/command send paths.
func (b *Backend) withPane(fn func(p *tmuxPane) error) error {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return fmt.Errorf("claude-code-tmux backend not started")
	}
	return fn(pane)
}

func (b *Backend) SendKeystroke(ctx context.Context, key string) error {
	return b.withPane(func(p *tmuxPane) error { return p.sendKeystroke(ctx, key) })
}

func (b *Backend) SendSpecialKey(ctx context.Context, key string) error {
	return b.withPane(func(p *tmuxPane) error { return p.sendSpecial(ctx, key) })
}

// Interrupt cancels the current agent turn by sending Escape×2 (cancel
// current operation) then Ctrl-C (clear any remaining input).
func (b *Backend) Interrupt(ctx context.Context) error {
	for i := 0; i < 2; i++ {
		if err := b.SendSpecialKey(ctx, "Escape"); err != nil {
			return err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return b.SendSpecialKey(ctx, "C-c")
}

// IsTurnInFlight reports whether per-turn bookkeeping is installed but hasn't
// fired yet. ImmediateInject consults this to choose between begin-turn and follow-up
// routing on cctmux.
func (b *Backend) IsTurnInFlight() bool {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	return b.turnEvents != nil
}

// sendCommand is the internal primitive for queued user-text and slash
// commands on the tmux backend. The tmux backend has no rearm cascade —
// responses route through the file watcher's per-turn callbacks, not a
// stream-based re-arm. Called from Inject for the follow-up, steer, and
// slash-command paths.
func (b *Backend) sendCommand(ctx context.Context, command string) error {
	return b.withPane(func(p *tmuxPane) error { return p.sendText(ctx, command) })
}

// Inject is the canonical entry point for delivering a user-role event to
// the tmux backend. Mirrors the ccstream Inject contract minus the rearm
// cascade — the tmux backend uses the JSONL file watcher rather than a
// stream-based re-arm, so follow-up responses route through the same
// per-turn completion callback installed by sendToPane.
//
// Attachments are silently dropped on tmux: there's no protocol primitive
// for structured content blocks via the tmux pane keystroke channel.
// Callers who require attachments should run on the ccstream backend.
//
// Routing: see Delegator.ImmediateInject for the full matrix.
func (b *Backend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	inFlight := b.IsTurnInFlight()
	if len(inj.Attachments) > 0 {
		log.Debugf("cctmux", "Inject(%s): %d attachment(s) dropped — tmux backend has no structured content channel",
			inj.Source, len(inj.Attachments))
	}

	switch inj.Source {
	case delegator.SourceUser:
		if !inFlight {
			return b.sendToPane(ctx, inj.Text, inj.Turn, false)
		}
		return b.sendCommand(ctx, inj.Text)

	case delegator.SourceSteer:
		if !inFlight {
			// Edge case: idle steer. Degrade to begin-turn — message is
			// still delivered, just without an in-flight task to interrupt.
			return b.sendToPane(ctx, inj.Text, inj.Turn, false)
		}
		if err := b.Interrupt(ctx); err != nil {
			log.Warnf("cctmux", "Inject(Steer): Interrupt failed: %v (continuing with sendCommand)", err)
		}
		return b.sendCommand(ctx, inj.Text)

	case delegator.SourceSystem:
		// System-initiated text (foci send, cron, notifications, error and
		// restart injections) never folds into a running turn — only real
		// user input may steer. The exclusive begin rejects with
		// ErrTurnInFlight when a turn is in flight; the caller waits for
		// completion and retries.
		return b.sendToPane(ctx, inj.Text, inj.Turn, true)

	case delegator.SourceCompact, delegator.SourcePass:
		return b.sendCommand(ctx, inj.Text)
	}
	return fmt.Errorf("cctmux: Inject: unknown source %d", inj.Source)
}

// CaptureCommandOutput polls the tmux pane until content stabilises
// (unchanged for stableFor, checked every pollInterval). Returns the
// raw pane content. Implements delegator.CommandOutputCapturer.
func (b *Backend) CaptureCommandOutput(ctx context.Context, stableFor, pollInterval time.Duration) (string, error) {
	b.mu.Lock()
	pane := b.pane
	b.mu.Unlock()
	if pane == nil {
		return "", fmt.Errorf("claude-code-tmux backend not started")
	}

	var lastContent string
	var stableSince time.Time

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Overall timeout: stableFor + 5s grace for initial rendering.
	deadline, cancel := context.WithTimeout(ctx, stableFor+5*time.Second)
	defer cancel()

	for {
		select {
		case <-deadline.Done():
			if lastContent != "" {
				return lastContent, nil // return what we have
			}
			return "", deadline.Err()
		case <-ticker.C:
			content, err := pane.capturePane(ctx)
			if err != nil {
				continue
			}
			if content == lastContent && lastContent != "" {
				if time.Since(stableSince) >= stableFor {
					return content, nil
				}
			} else {
				lastContent = content
				stableSince = time.Now()
			}
		}
	}
}

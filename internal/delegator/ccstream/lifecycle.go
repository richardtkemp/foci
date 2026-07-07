package ccstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/procx"
)

// Close timeouts. Vars (not consts) so tests can shrink them; production
// path keeps the ~9s worst-case shutdown documented in Close.
var (
	closeGracefulWait = 5 * time.Second // wait for clean exit before SIGTERM
	closeSigtermWait  = 2 * time.Second // wait after SIGTERM before SIGKILL
	closeSigkillWait  = 2 * time.Second // wait after SIGKILL before abandoning the waiter goroutine
)

// Start launches the Claude Code subprocess with stream-json pipes.
func (b *Backend) Start(ctx context.Context, opts delegator.StartOptions) error {
	b.startOpts = opts
	b.workDir = opts.WorkDir
	b.agentID = opts.AgentID
	b.label = opts.Label
	b.model = opts.Model
	b.systemPrompt = opts.SystemPrompt
	b.autoApproveRules = parseAutoApproveRules(opts.AutoApproveRules)
	b.agents.MaxAge = opts.SubagentMaxAge

	// Build command args.
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--include-hook-events",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// Cold-launch effort injection (#840). apply_flag_settings (the /effort
	// runtime path) is session-local and resets to the model default on a
	// bounce (post-compaction reload, idle respawn). Passing --effort at
	// launch re-establishes the level every Start, so the persisted session
	// effort survives. Empty/"off" → omit the flag (CC uses the model
	// default). foci validates the level before it reaches here.
	if eff := opts.Effort; eff != "" && eff != "off" {
		args = append(args, "--effort", eff)
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	// skip_permissions bypasses all CC permission prompts (unattended). When
	// set, CC never asks, so foci's auto-approve hook has nothing to answer.
	if delegator.SkipPermissions(b.cfg) {
		args = append(args, "--dangerously-skip-permissions")
	}
	// Permission pre-approval rules. cfg["allowed_tools"] is the merged
	// string produced by cmd/foci-gw/agents_delegated.go (global
	// [cc_backend] default_allowed_tools combined with the agent's
	// backend_config.allowed_tools). Rules use CC's permission syntax —
	// e.g. "Write(/tmp/**)", "Bash(git:*)".
	if v, ok := b.cfg["allowed_tools"].(string); ok && v != "" {
		args = append(args, "--allowedTools", v)
	}

	component := "ccstream"
	if opts.Label != "" {
		component = "ccstream:" + opts.Label
	}

	// Build foci's hook settings JSON and append it as a --settings argv
	// so CC loads it as a flagSettings source (always enabled, merges
	// with user hooks automatically). Skipped when the foci-cc-hook
	// binary can't be located — Warn logged, ccstream runs without
	// OnToolEnd events. See hooks.go for the full flow.
	if hookSettings, ok := b.prepareHooks(); ok {
		args = append(args, "--settings", hookSettings)
	}

	// Resolve the binary to spawn. Production runs use "claude" (resolved
	// via $PATH); integration tests inject a stub via the claude_binary
	// config knob (folded into b.cfg by cmd/foci-gw/agents_delegated.go
	// from global [cc_backend].claude_binary, with per-agent override).
	claudeBin := "claude"
	if v, ok := b.cfg["claude_binary"].(string); ok && v != "" {
		claudeBin = v
	}

	log.Infof(component, "launching: %s %s (workdir=%s)", claudeBin, strings.Join(args, " "), opts.WorkDir)

	// Create command with its own cancellable context. The CC process is
	// long-lived (surviving across turns), so it must NOT be tied to the
	// caller's context — otherwise the process is killed when the turn
	// context expires or is cancelled.
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(cmdCtx, claudeBin, args...)
	cmd.Dir = opts.WorkDir
	cmd.Env = os.Environ()

	// Turn completion is keyed to CC's session_state_changed running/idle SDK
	// events (see OnSystem / onSessionIdle) — opt-in in CC, so the backend
	// enables them itself. Placed before opts.Env so a per-agent
	// backend_config.env can override for debugging (the backend then falls
	// back to complete-on-result with a Warnf).
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1")

	// Apply extra environment variables from StartOptions (e.g. BASH_ENV,
	// FOCI_SOCK from the exec bridge created by DelegatedManager).
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Get pipes.
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: stderr pipe: %w", err)
	}

	// Start the process.
	if err := cmd.Start(); err != nil {
		cmdCancel()
		return fmt.Errorf("ccstream: start: %w", err)
	}

	b.cmd = cmd
	b.writer = NewWriter(stdinPipe)
	b.cancel = cmdCancel
	b.done = make(chan struct{})

	// Reader goroutine — dispatches CC stdout messages to handler methods.
	readerCtx, readerCancel := context.WithCancel(context.Background())
	// Store readerCancel so Close can stop reader + keep-alive independently
	// of the command context.
	origCancel := b.cancel
	b.cancel = func() {
		readerCancel()
		origCancel()
	}

	go func() {
		defer close(b.done)
		reader := NewReader(stdoutPipe, b)
		reader.Run(readerCtx)
	}()

	// Stderr capture goroutine.
	go b.captureStderr(stderrPipe)

	// Keep-alive goroutine.
	go b.runKeepAlive(readerCtx)

	// Process waiter goroutine — reaps the subprocess and logs exit status.
	// Without this, a dead subprocess becomes a zombie until Close() is called.
	//
	// This goroutine is the AUTHORITATIVE source of "process is dead". The
	// reader goroutine may exit silently (ctx cancelled, partial line, etc.)
	// and miss the death; cmd.Wait() cannot. After logging, we invoke
	// finalizeExit to guarantee in-flight turn cleanup and `running=false`
	// run exactly once, even if the reader path also reaches OnReaderStopped.
	b.waitCh = make(chan error, 1)
	b.exitCh = make(chan struct{})
	go func() {
		err := cmd.Wait()
		b.exitErr = err // store for OnError; read after exitCh is closed
		close(b.exitCh)
		comp := b.logComponent()
		if err != nil {
			log.Warnf(comp, "process exited: %s", describeExitError(err))
		} else {
			log.Infof(comp, "process exited cleanly (status 0)")
		}
		// Drive cleanup regardless of whether the reader goroutine notices.
		// finalizeExit is idempotent — if OnReaderStopped already ran, this
		// is a no-op.
		b.finalizeExit(err)
		b.waitCh <- err
	}()

	// Send initialize control request with system prompt.
	// Save the request ID so OnControlResponse can detect the response
	// and close readyCh. For fresh sessions (no --resume), CC responds
	// with a control_response rather than emitting system/init.
	initReqID := newRequestID()
	b.mu.Lock()
	b.initReqID = initReqID
	b.mu.Unlock()
	if err := b.writer.SendControl(initReqID, &InitializeRequest{
		Subtype:      "initialize",
		SystemPrompt: opts.SystemPrompt,
	}); err != nil {
		return fmt.Errorf("ccstream: send initialize: %w", err)
	}

	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	return nil
}

// Close shuts down the Claude Code subprocess gracefully.
func (b *Backend) Close() error {
	b.closeOnce.Do(b.closeInner)
	return nil
}

// closeInner runs the shutdown kill-ladder exactly once (guarded by closeOnce).
// It is gated on the subprocess having been started — NOT on `running` — so a
// backend that a finalize path already marked dead (running=false) while its
// process is still alive is still reaped rather than leaked (P1-9).
func (b *Backend) closeInner() {
	b.mu.Lock()
	started := b.cmd != nil
	b.running = false
	b.closing = true
	b.mu.Unlock()

	// Never started (e.g. a unit-test backend, or Start failed before launch):
	// nothing to tear down.
	if !started {
		return
	}

	component := b.logComponent()
	pid := 0
	if b.cmd.Process != nil {
		pid = b.cmd.Process.Pid
	}
	log.Infof(component, "closing CC subprocess (pid=%d)", pid)

	// Try graceful shutdown: only send an interrupt if a turn is in flight.
	// CC's interrupt handler aborts the per-turn AbortController; sent after
	// a clean turn end it cascades through stale post-turn async work and
	// flips CC's exit code from 0 to 1 (CC keys exit code on the last result
	// message's is_error flag — the abort can replace a success result with
	// an error_during_execution one). Closing stdin alone is sufficient to
	// shut CC down cleanly when there's nothing to abort.
	if b.IsTurnInFlight() {
		// Best-effort, non-blocking: if a write is wedged on a hung pipe we must
		// not block here — Close (below) evicts it. (P2-4.)
		_ = b.writer.TrySendInterrupt()
	}
	_ = b.writer.Close()

	// Wait for process exit with timeout. The waiter goroutine (launched in
	// Start) calls cmd.Wait() and sends the result to waitCh. If the process
	// already exited, this returns immediately.
	//
	// Every wait has a bounded timeout, including the final one after SIGKILL.
	// This is a permanent liveness invariant, not a workaround: an unbounded
	// `<-waitCh` here holds m.mu in the caller (ResetSession / Get), so if the
	// waiter goroutine ever fails to report — for ANY reason — the whole agent
	// freezes until manual restart. The cap guarantees Close always returns so
	// callers can release locks and respawn, trading a possible (OS-reaped)
	// zombie-process leak for that guarantee.
	//
	// The original trigger was the #749 lock-ordering deadlock, where the
	// waiter stalled inside finalizeExit behind handler callbacks holding
	// locks. That root cause is fixed (commit 85e49f26) and has logged zero
	// stalls since 2026-05-17. The cap stays regardless: it is the backstop
	// for whatever the next unforeseen stall turns out to be.
	select {
	case <-b.waitCh:
		log.Infof(component, "CC subprocess (pid=%d) exited", pid)
	case <-time.After(closeGracefulWait):
		// SIGTERM.
		log.Warnf(component, "process (pid=%d) did not exit after %s, sending SIGTERM", pid, closeGracefulWait)
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-b.waitCh:
		case <-time.After(closeSigtermWait):
			// SIGKILL.
			log.Warnf(component, "process (pid=%d) did not exit after SIGTERM, sending SIGKILL", pid)
			if b.cmd.Process != nil {
				_ = b.cmd.Process.Kill()
			}
			select {
			case <-b.waitCh:
			case <-time.After(closeSigkillWait):
				// Liveness backstop — the waiter goroutine stalled. Process is
				// already SIGKILL'd so the OS will reap it; we just stop
				// waiting for the goroutine to confirm. Without this cap, m.mu
				// in the caller is held forever and no further messages can be
				// processed for this agent. Reaching here is unexpected post-#749
				// (zero occurrences since the 85e49f26 fix) — if it fires, it is
				// a NEW stall worth investigating, not the known deadlock.
				log.Warnf(component, "waiter goroutine did not report after SIGKILL within %s — abandoning wait (possible zombie)", closeSigkillWait)
			}
		}
	}

	// Cancel reader + keep-alive goroutines.
	if b.cancel != nil {
		b.cancel()
	}

	// Wait for reader goroutine to exit.
	if b.done != nil {
		<-b.done
	}

	// No hook cleanup needed — the CC subprocess exits with our
	// --settings temp file still on disk, but it's owned by CC and the
	// content-hash path is stable so it naturally de-dupes across
	// backend restarts.
}

// WaitReady blocks until the init message is received from CC, the
// subprocess reader exits (e.g. CC died before init — happens when
// --resume points at a missing session and CC exits non-zero), or the
// caller's context expires. Returning an error on early-exit lets
// DelegatedManager's retry-without-resume path fire immediately rather
// than burning the full ready-timeout budget waiting for an init that
// can no longer arrive.
func (b *Backend) WaitReady(ctx context.Context) error {
	select {
	case <-b.readyCh:
		return nil
	case <-b.done:
		return fmt.Errorf("ccstream: subprocess exited before init")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// OnReaderStopped handles the reader goroutine exiting for any reason, including
// expected shutdown (Close), clean process exit (io.EOF), or unexpected errors
// (broken pipe, scanner errors). It logs the reader's observation, then defers
// the actual cleanup to finalizeExit so the work runs exactly once even if the
// waiter goroutine (cmd.Wait) reached the same conclusion first.
func (b *Backend) OnReaderStopped(err error) {
	component := b.logComponent()

	b.mu.Lock()
	expected := b.closing
	b.mu.Unlock()

	if expected {
		log.Infof(component, "subprocess reader stopped (session closing)")
	} else {
		log.Warnf(component, "subprocess reader stopped: %v", err)
	}

	b.finalizeExit(err)
}

// finalizeExit performs the one-shot cleanup when the CC subprocess has died.
//
// Two independent goroutines can observe process death: the waiter goroutine
// (cmd.Wait returns) and the reader goroutine (scanner EOF / read error / ctx
// cancel). Historically only OnReaderStopped did the cleanup, so any failure
// mode that caused the reader to exit silently (ctx cancelled, partial-line
// read stuck, etc.) left the backend wedged: `running=true` so DelegatedManager
// kept handing back the corpse, and any in-flight turn handler hung on
// CompletionChan forever.
//
// finalizeExit is gated by sync.Once so whichever path notices first wins and
// the other becomes a no-op. The waiter goroutine is the authoritative source
// of truth (cmd.Wait cannot lie), but OnReaderStopped also calls in for the
// case where the reader sees EOF before cmd.Wait has returned.
//
// Reset in Restart() before the subprocess is relaunched.
func (b *Backend) finalizeExit(reason error) {
	b.finalizeOnce.Do(func() {
		component := b.logComponent()

		// Instrumentation: bracket the cleanup so we can see whether it
		// completed and where time goes when the waiter-goroutine
		// signalling chain stalls (TODO #749). Should be sub-millisecond
		// in the happy path; >1s indicates a callback or lock issue.
		start := time.Now()
		log.Debugf(component, "finalizeExit: enter reason=%v", reason)
		defer func() {
			log.Debugf(component, "finalizeExit: exit elapsed=%s", time.Since(start))
		}()

		b.mu.Lock()
		expected := b.closing
		b.running = false
		b.mu.Unlock()
		log.Debugf(component, "finalizeExit: post-mu elapsed=%s", time.Since(start))

		// If the waiter goroutine has set exitErr, prefer its detail for the
		// user-visible message. Wait briefly in case finalizeExit was invoked
		// from the reader path before cmd.Wait returned. The waiter writes
		// exitErr immediately before closing exitCh, so the read is only
		// happens-before-safe once we've received from exitCh — snapshot it
		// inside the receive arm. On the timeout path the waiter hasn't
		// finished, so exitErr stays nil (detail unknown) rather than racing on
		// b.exitErr. exitCh is nil when finalizeExit is called from a unit test
		// that bypasses Start; skip the wait in that case.
		var exitErr error
		if b.exitCh != nil {
			select {
			case <-b.exitCh:
				exitErr = b.exitErr
			case <-time.After(2 * time.Second):
				log.Debugf(component, "finalizeExit: exitCh wait timed out (waiter goroutine has not set exitErr) elapsed=%s", time.Since(start))
			}
		}
		log.Debugf(component, "finalizeExit: post-exitCh-wait elapsed=%s", time.Since(start))

		if !expected && exitErr != nil {
			log.Warnf(component, "process exit detail: %s", describeExitError(exitErr))
		}

		// Drain any in-flight turn so callers waiting on CompletionChan or
		// WaitForTurn don't block forever. The subprocess is gone, so no
		// idle event will ever complete this turn — claim it here.
		b.turnMu.Lock()
		turn := b.turnEvents
		b.turnEvents = nil
		b.turnActive = false
		b.setAutonomousActiveLocked(false) // subprocess gone: no idle will clear it
		b.stashedResult = nil
		b.stashedResultMsg = nil
		b.redispatchInFlight = false
		resultCh := b.turnResultCh
		b.turnMu.Unlock()
		b.drainEdgeCallbacks()
		b.agents.ClearAll() // subprocess gone: pending agents can never complete
		log.Debugf(component, "finalizeExit: post-turnMu turn_nil=%v turn_otc_nil=%v elapsed=%s", turn == nil, turn == nil || turn.OnTurnComplete == nil, time.Since(start))

		if turn != nil && turn.OnTurnComplete != nil {
			var msg string
			if expected {
				msg = "Session closed while turn was in flight"
			} else {
				msg = fmt.Sprintf("Error: CC process exited unexpectedly: %v", reason)
				if exitErr != nil && exitErr != reason {
					msg += " (" + describeExitError(exitErr) + ")"
				}
			}
			log.Debugf(component, "finalizeExit: pre-OnTurnComplete elapsed=%s", time.Since(start))
			turn.OnTurnComplete(&delegator.TurnResult{Text: msg})
			log.Debugf(component, "finalizeExit: post-OnTurnComplete elapsed=%s", time.Since(start))
		}

		if b.typingFunc != nil {
			log.Debugf(component, "finalizeExit: pre-typingFunc(false) elapsed=%s", time.Since(start))
			b.typingFunc(false)
			log.Debugf(component, "finalizeExit: post-typingFunc(false) elapsed=%s", time.Since(start))
		}

		// Unblock WaitForTurn.
		if resultCh != nil {
			select {
			case resultCh <- &ResultMessage{Subtype: "error_during_execution", IsError: true}:
			default:
			}
		}
	})
}

// runKeepAlive sends periodic keep-alive messages to CC's stdin.
//
// NOTE: As of CC 1.x, CC silently ignores keep_alive messages in --pipe mode
// (structuredIO.ts drops them). This goroutine runs but has no observable
// effect. The original intent was to prevent idle timeout, but CC's pipe
// transport has no idle timeout to prevent. Kept for forward-compatibility
// in case CC adds pipe-mode keepalive handling.
func (b *Backend) runKeepAlive(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := b.writer.SendKeepAlive(); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// captureStderr reads CC's stderr line by line and logs it. CC's stderr
// can contain progress info, warnings, and errors. Lines containing "error"
// or "fatal" are logged at warn level; everything else at debug.
//
// Buffer matches the stdout reader's 1MB cap (reader.go maxTokenSize) so
// a misbehaving subprocess writing one huge stderr line doesn't silently
// stall its own stderr pipe: bufio.Scanner with default 64KB buffer
// would return ErrTooLong and this goroutine would exit, causing the pipe
// to fill and the subprocess to block on its next stderr write — wedging
// the whole turn before stdout ever delivered a single envelope.
func (b *Backend) captureStderr(r io.Reader) {
	component := b.logComponent()
	scanner := bufio.NewScanner(r)
	const maxLine = 1 << 20 // 1MB — matches stdout reader cap
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic") {
			log.Warnf(component, "stderr: %s", line)
		} else {
			log.Debugf(component, "stderr: %s", line)
		}
		// Secondary 401 detection: a dead token can surface on stderr rather
		// than as an error result. The re-login gate single-flights, so firing
		// alongside OnResult is harmless (#843).
		if isAuthFailure(line) {
			b.fireAuthFailure(line)
		}
	}
	// Surface scanner-level errors (e.g. ErrTooLong on a >1MB line). Without
	// this the goroutine exited silently and the subprocess's stderr pipe
	// would back up. EOF is the normal exit when the subprocess closes
	// stderr — don't warn on that.
	if err := scanner.Err(); err != nil {
		log.Warnf(component, "stderr capture stopped: %v", err)
	}
}

// describeExitError returns a human-readable description of a process exit
// error including exit code, signal, and stderr snippet when available.
func describeExitError(err error) string {
	if err == nil {
		return "exit status 0"
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err.Error()
	}

	ps := exitErr.ProcessState
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return fmt.Sprintf("exit code %d", exitErr.ExitCode())
	}

	var parts []string
	if ws.Exited() {
		parts = append(parts, fmt.Sprintf("exit code %d", ws.ExitStatus()))
	}
	if ws.Signaled() {
		parts = append(parts, fmt.Sprintf("signal %s", ws.Signal()))
		if ws.CoreDump() {
			parts = append(parts, "core dumped")
		}
	}

	// Include a stderr snippet if the ExitError captured any.
	if len(exitErr.Stderr) > 0 {
		snippet := string(exitErr.Stderr)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		parts = append(parts, fmt.Sprintf("stderr: %s", snippet))
	}

	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, ", ")
}

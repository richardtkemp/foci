package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/provider"
	"foci/shared/prompts"
)

// delegatedCompactTimeout bounds runDelegatedCompact's wait for a delegated
// backend to finish compacting. It is a hang guard, not an expected duration:
// a codex compaction of a 379k-token context took 5m18s (work Mac,
// 2026-11-07), so the earlier 5-minute budget reported real successes as
// failures (#2947).
const delegatedCompactTimeout = 20 * time.Minute

// markCompacting / clearCompacting / IsCompacting latch whether a compaction
// is in flight for a session, so /status can show "compacting" instead of
// "idle" (#725). The stored deadline is a self-heal backstop: if a compaction
// errors between mark and clear (so the defer-clear is skipped only on a
// panic that unwinds past it), the latch expires rather than wedging forever.
func (a *Agent) markCompacting(sessionKey string) {
	a.compacting.Store(sessionKey, time.Now().Add(delegatedCompactTimeout))
}

func (a *Agent) clearCompacting(sessionKey string) {
	a.compacting.Delete(sessionKey)
}

// IsCompacting reports whether a compaction is currently in flight for the
// session. Expired latches self-heal on read.
func (a *Agent) IsCompacting(sessionKey string) bool {
	v, ok := a.compacting.Load(sessionKey)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		a.compacting.Delete(sessionKey)
		return false
	}
	return true
}

// CompactResult holds the outcome of a compaction operation.
type CompactResult struct {
	OldMessageCount int
	Summary         string
}

// doCompact executes the core compaction sequence: resolve prompts, resolve
// call site, run Compactor.Compact (which archives the old content in place),
// fire hooks.
//
// Callers are responsible for pre-compaction actions (threshold checks,
// memory formation) and post-compaction cleanup (bootstrap reload, cache
// invalidation) since those differ between auto-compaction and manual compaction.
func (a *Agent) doCompact(ctx context.Context, sessionKey string, system []provider.SystemBlock, oldCount int, dryRun bool) (CompactResult, error) {
	if !dryRun {
		a.markCompacting(sessionKey)
		defer a.clearCompacting(sessionKey)
	}
	if dryRun {
		for _, fn := range a.CompactionStartFunc {
			fn(sessionKey, "⏳ Running compaction dry-run...")
		}
	} else {
		for _, fn := range a.CompactionStartFunc {
			fn(sessionKey, "⏳ Compacting context...")
		}
	}

	summaryPrompt := a.compactionSummaryPrompt()
	handoffMsg := a.compactionHandoffMsg()
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), a.PromptSearchDirs...)
	}

	compactClient, compactModel, compactFormat := a.ResolveCallSite(config.CallCompaction, sessionKey)
	summary, err := a.Compactor.Compact(ctx, compactClient, sessionKey, compactModel, compactFormat, system, summaryPrompt, handoffMsg, dryRun)
	if err != nil {
		// The ⏳ is already out: resolve it, or the chat shows "in progress"
		// forever (#2947). Dry-run too: it sent its own ⏳.
		label := "Compaction"
		if dryRun {
			label = "Compaction dry-run"
		}
		a.resolveCompactionNotice(sessionKey, fmt.Sprintf("❌ %s failed: %v", label, err))
		return CompactResult{OldMessageCount: oldCount}, &CompactionNoticeShownError{Err: fmt.Errorf("compaction failed: %w", err)}
	}

	result := CompactResult{OldMessageCount: oldCount, Summary: summary}

	if dryRun {
		if summary != "" {
			for _, fn := range a.CompactionDebugFunc {
				fn(sessionKey, summary)
			}
		}
		for _, fn := range a.CompactionNotifyFunc {
			fn(sessionKey, fmt.Sprintf("✅ Dry-run complete — %d messages would be summarised. Summary sent.", oldCount), summary)
		}
	} else {
		accounting.NoteCacheReset(sessionKey) // a new conversation: its next call is no bust (#2930)
		for _, fn := range a.CompactionNotifyFunc {
			fn(sessionKey, fmt.Sprintf("✅ Context compacted — %d messages summarised.", oldCount), summary)
		}
		if summary != "" {
			for _, fn := range a.CompactionDebugFunc {
				fn(sessionKey, summary)
			}
		}
	}

	return result, nil
}

// runDelegatedCompact sends "/compact $instructions" to a delegated backend
// and waits for the compact_boundary signal. Fires start and notify hooks,
// reloads nudge rules. Shared by manual /compact (CompactSession) and the
// auto-compaction path (DelegatedTransport.RunCompaction). Callers own their
// own pre/post work (memory formation, threshold checks, session meta cleanup).
//
// The input ctx is wrapped with delegatedCompactTimeout. Auto-compaction
// passes context.Background because the turn context is already cancelled;
// manual compaction passes the user command context so /stop can interrupt.
//
// force ("/compact force") makes the post-compaction reload bounce restart CC
// at once even while background subagents run (killing them); otherwise the
// bounce waits for that work to finish (#1539).
func (a *Agent) runDelegatedCompact(ctx context.Context, be delegator.Delegator, sessionKey string, force bool) error {
	summaryPrompt := a.compactionSummaryPrompt()
	if summaryPrompt == "" {
		return fmt.Errorf("compaction summary prompt is empty")
	}

	// Latch compaction-in-flight for /status; cleared on every return path
	// (success, error, timeout). The defer is the load-bearing clear — the
	// deadline stored by markCompacting is only a backstop (#725).
	a.markCompacting(sessionKey)
	defer a.clearCompacting(sessionKey)

	cctx, cancel := context.WithTimeout(ctx, delegatedCompactTimeout)
	defer cancel()

	// Arm waiters before sending so stream events are never missed.
	if cw, ok := delegator.As[delegator.CompactionWaiter](be); ok {
		cw.ArmCompactionWait()
	}
	if csw, ok := delegator.As[delegator.CompactionStartWaiter](be); ok {
		csw.ArmCompactionStartWait()
	}

	if err := be.ImmediateInject(cctx, delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   fmt.Sprintf("/compact %s", summaryPrompt),
	}); err != nil {
		return fmt.Errorf("send /compact: %w", err)
	}

	// Wait for CC to confirm compaction is underway before notifying the user.
	// This prevents the ⏳ from racing ahead of buffered content messages.
	// Timeout/error here is non-fatal: send the notification anyway and
	// continue waiting for completion below.
	if csw, ok := delegator.As[delegator.CompactionStartWaiter](be); ok {
		_ = csw.WaitForCompactionStart(cctx)
	}
	for _, fn := range a.CompactionStartFunc {
		fn(sessionKey, "⏳ Compacting context...")
	}

	var waitErr error
	if cw, ok := delegator.As[delegator.CompactionWaiter](be); ok {
		waitErr = cw.WaitForCompaction(cctx)
	} else {
		waitErr = be.WaitForTurn(cctx)
	}
	if errors.Is(waitErr, delegator.ErrCompactionNoBoundary) {
		// The backend declined to compact (e.g. too few messages). Benign
		// no-op: the /compact run already finished, so return promptly (the
		// deferred clearCompacting unblocks the held inbox — #1267) and skip
		// the "compacted" notification and the session bounce, since nothing
		// changed. Callers distinguish this via errors.Is.
		a.logger().Debugf("session=%s delegated compaction declined by backend (no boundary)", sessionKey)
		a.resolveCompactionNotice(sessionKey, "ℹ️ Nothing to compact — session too short.")
		return &CompactionNoticeShownError{Err: delegator.ErrCompactionNoBoundary}
	}
	if waitErr != nil {
		// The ⏳ notice is already out: resolve it, or the chat shows
		// "in progress" forever (#2947).
		a.resolveCompactionNotice(sessionKey, compactionWaitFailedNotice(waitErr))
		return &CompactionNoticeShownError{Err: fmt.Errorf("wait for compaction: %w", waitErr)}
	}
	// The conversation is a new one now: its next call reads only what the
	// summary shares with the old prefix, which is no bust (#2930).
	accounting.NoteCacheReset(sessionKey)

	// The backend compacted internally; recover its summary where it can so
	// the notice gets a tappable summary chit (#1390). A missing summary only
	// costs the chit, never the notice.
	var summary string
	if cs, ok := delegator.As[delegator.CompactionSummarizer](be); ok {
		s, err := cs.CompactionSummary(cctx)
		if err != nil {
			a.logger().Warnf("session=%s delegated compaction summary unavailable: %v", sessionKey, err)
		}
		summary = s
	}
	for _, fn := range a.CompactionNotifyFunc {
		fn(sessionKey, "✅ Context compacted (delegated).", summary)
	}

	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}

	// #828 Part B: bounce the CC session (close, keep resume ID) so the next
	// message respawns with --resume — rebuilding the system prompt from disk
	// via StartOptions.SystemPromptFunc while resuming the now-compacted
	// conversation, so character/skill edits reload. Per-agent gated
	// (reload_on_compact, default on). The SystemPromptFunc closure re-reads
	// Bootstrap itself, so no Bootstrap.Reload() is needed here.
	// Bounce only when the on-disk system prompt actually changed (character
	// edits or a skill added/removed); an unchanged prompt means the running
	// session is already current, so the restart would interrupt the flow for
	// nothing. The resume nudge is gated on an actual bounce: with no restart
	// there is no interruption to recover from (pre-#828 compaction is seamless).
	// The restart kills background subagents running inside CC, so unless
	// forced it is deferred until they finish (#1539); a deferred bounce
	// returns false here and sends no nudge (the session is idle by then).
	if a.reloadOnCompact() && a.DelegatedManager != nil {
		if a.DelegatedManager.BounceAfterCompaction(sessionKey, force) {
			a.maybeInjectCompactionResume(ctx, sessionKey)
		}
	}
	return nil
}

// CompactionNoticeShownError marks a compaction error whose outcome the chat
// has already been told: the ⏳ notice was edited to ❌ or "nothing to
// compact" through CompactionNotifyFunc. /compact uses it to drop its own
// reply where that notice shows, so the user sees one message, not two
// (#2891, #2947). It unwraps to the underlying error, so errors.Is checks
// such as delegator.ErrCompactionNoBoundary still match.
type CompactionNoticeShownError struct {
	Err error
}

func (e *CompactionNoticeShownError) Error() string { return e.Err.Error() }
func (e *CompactionNoticeShownError) Unwrap() error { return e.Err }

// CompactionNoticeShown reports whether err is (or wraps) a
// CompactionNoticeShownError.
func CompactionNoticeShown(err error) bool {
	var shown *CompactionNoticeShownError
	return errors.As(err, &shown)
}

// resolveCompactionNotice edits the ⏳ "Compacting context..." notice to msg
// through the notify hooks, for an outcome that is not "compacted". It is
// called only after the ⏳ was sent, so it never posts a stray message for a
// compaction that was never shown as started.
func (a *Agent) resolveCompactionNotice(sessionKey, msg string) {
	for _, fn := range a.CompactionNotifyFunc {
		fn(sessionKey, msg, "")
	}
}

// compactionWaitFailedNotice is the ❌ text for a failed wait. On a timeout or
// a cancel foci only stopped waiting: the backend may still finish the
// compaction (#2947 saw codex finish 18s after foci gave up), so the text says
// so rather than claiming the compaction failed.
func compactionWaitFailedNotice(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("❌ Compaction did not finish within %s; foci stopped waiting. The backend may still complete it.", delegatedCompactTimeout)
	case errors.Is(err, context.Canceled):
		return "❌ Compaction cancelled; foci stopped waiting. The backend may still complete it."
	default:
		return fmt.Sprintf("❌ Compaction failed: %v", err)
	}
}

// compactionResumePrompt is the self-injected nudge sent after a compaction
// bounce (#845). The bounce closes the CC session (keeping the resume ID) so
// the next message respawns it — but a mid-task flow has no next message and
// would silently stall. This synthesises one: the agent resumes if it was
// mid-task, or emits the no-response sentinel if it was idle.
const compactionResumePrompt = "[system: your context was just compacted and your CC session restarted, interrupting any work in progress. If you were in the middle of a task when this happened, resume it now from where you left off. If you were NOT mid-task (idle, or the previous turn fully completed), reply with " + NoResponseSentinel + " and nothing else — do not narrate this message.]"

// maybeInjectCompactionResume self-injects the resume nudge after a compaction
// bounce so an interrupted flow continues autonomously (#845). It is suppressed
// when:
//   - async self-injection is unavailable (AsyncNotifier nil), or
//   - the user already queued a follow-up message — that message will respawn
//     CC and drive continuation with the user's actual intent, so our generic
//     nudge would be redundant (and could race a second turn), or
//   - the compaction is a compact_session request with a wake message
//     (#3041): that wake is the next turn and says what to do.
//
// Runs from the post-turn worker goroutine (auto-compaction) or the command
// handler (manual /compact); both serialise with the inbox check below.
func (a *Agent) maybeInjectCompactionResume(ctx context.Context, sessionKey string) {
	if a.AsyncNotifier == nil {
		return
	}
	if compactionWakeFollows(ctx) {
		// The compact_session request's own wake follows and restarts the
		// agent (#3041); a nudge as well would run a second turn.
		a.logger().Debugf("session=%s skip compaction-resume nudge: the request's wake follows", sessionKey)
		return
	}
	if a.InboxHasPendingInput(sessionKey) {
		a.logger().Debugf("session=%s skip compaction-resume nudge: user input already queued", sessionKey)
		return
	}
	a.AsyncNotifier.InjectToAgent(sessionKey, compactionResumePrompt, "", "compaction-resume")
}

// maybeCompact checks whether context compaction is needed and performs it.
// Triggers: (1) main threshold, (2) user /compact. It reports whether it ran one.
func (a *Agent) maybeCompact(ctx context.Context, sessionKey string, messages []provider.Message, system []provider.SystemBlock, usage *provider.Usage, sm *sessionMeta) bool {
	if a.Compactor == nil {
		return false
	}

	totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	ctxLimit := a.SessionContextLimit(sessionKey)

	if !a.Compactor.ShouldCompactWithLimit(sessionKey, messages, usage, ctxLimit) {
		return false
	}

	if a.SessionNoCompact(sessionKey) {
		percent := int(float64(totalTokens) / float64(ctxLimit) * 100)
		a.logger().Infof("session=%s context at %d%% capacity for no_compact session", sessionKey, percent)
		return false
	}

	for _, fn := range a.CompactionMemoryFunc {
		fn(sessionKey)
	}

	oldCount := len(messages)
	if _, err := a.doCompact(ctx, sessionKey, system, oldCount, false); err != nil {
		a.logger().Errorf("session=%s %v", sessionKey, err)
	}

	// Reload system prompt — compaction may have changed memory files.
	// Only invalidate THIS session's cached system blocks so other sessions
	// keep their byte-identical prompts and don't suffer cache busts.
	a.Bootstrap.Reload()
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	a.resetAfterCompaction(sm)
	return true
}

// summarizeServerToolResult extracts a brief text summary from a server tool result block.
// Server tool result blocks (web_search_tool_result, web_fetch_tool_result) contain
// structured data in their Raw JSON. We extract a human-readable snippet for observers.
func summarizeServerToolResult(block provider.ContentBlock) string {
	// Try to extract content from the raw JSON
	if len(block.Raw) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(block.Raw, &raw); err == nil {
			// web_search_tool_result has a "content" array with search results
			if content, ok := raw["content"]; ok {
				var items []json.RawMessage
				if json.Unmarshal(content, &items) == nil && len(items) > 0 {
					return fmt.Sprintf("%d results", len(items))
				}
			}
		}
	}
	return block.Type
}

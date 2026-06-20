package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/shared/prompts"
)

const delegatedCompactTimeout = 5 * time.Minute

// markCompacting / clearCompacting / IsCompacting latch whether a compaction
// is in flight for a session, so /status can show "compacting" instead of
// "idle" (#725). The key is normalised to SessionKeyBase because compaction
// rotates the session version mid-flight — set and clear would otherwise miss
// each other. The stored deadline is a self-heal backstop: if a compaction
// errors between mark and clear (so the defer-clear is skipped only on a
// panic that unwinds past it), the latch expires rather than wedging forever.
func (a *Agent) markCompacting(sessionKey string) {
	a.compacting.Store(session.SessionKeyBase(sessionKey), time.Now().Add(delegatedCompactTimeout))
}

func (a *Agent) clearCompacting(sessionKey string) {
	a.compacting.Delete(session.SessionKeyBase(sessionKey))
}

// IsCompacting reports whether a compaction is currently in flight for the
// session (keyed by SessionKeyBase). Expired latches self-heal on read.
func (a *Agent) IsCompacting(sessionKey string) bool {
	base := session.SessionKeyBase(sessionKey)
	v, ok := a.compacting.Load(base)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		a.compacting.Delete(base)
		return false
	}
	return true
}

// CompactResult holds the outcome of a compaction operation.
type CompactResult struct {
	OldMessageCount int
	Summary         string
	NewSessionKey   string // empty on dry-run or no rotation
}

// doCompact executes the core compaction sequence: resolve prompts, resolve
// call site, run Compactor.Compact, rotate session on success, fire hooks.
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

	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
	handoffMsg := a.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), a.PromptSearchDirs...)
	}

	compactClient, compactModel, compactFormat := a.ResolveCallSite(config.CallCompaction, sessionKey)
	summary, newKey, err := a.Compactor.Compact(ctx, compactClient, sessionKey, compactModel, compactFormat, system, summaryPrompt, handoffMsg, dryRun)
	if err != nil {
		return CompactResult{OldMessageCount: oldCount}, fmt.Errorf("compaction failed: %w", err)
	}

	result := CompactResult{OldMessageCount: oldCount, Summary: summary}

	if dryRun {
		if summary != "" {
			for _, fn := range a.CompactionDebugFunc {
				fn(sessionKey, summary)
			}
		}
		for _, fn := range a.CompactionNotifyFunc {
			fn(sessionKey, "✅ Dry-run complete — summary sent.")
		}
	} else {
		if newKey != "" {
			a.RotateSession(sessionKey, newKey)
			result.NewSessionKey = newKey
		}
		for _, fn := range a.CompactionNotifyFunc {
			fn(sessionKey, fmt.Sprintf("✅ Context compacted — %d messages summarised.", oldCount))
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
func (a *Agent) runDelegatedCompact(ctx context.Context, be delegator.Delegator, sessionKey string) error {
	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
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
	if cw, ok := be.(delegator.CompactionWaiter); ok {
		cw.ArmCompactionWait()
	}
	if csw, ok := be.(delegator.CompactionStartWaiter); ok {
		csw.ArmCompactionStartWait()
	}

	if err := be.Inject(cctx, delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   fmt.Sprintf("/compact %s", summaryPrompt),
	}); err != nil {
		return fmt.Errorf("send /compact: %w", err)
	}

	// Wait for CC to confirm compaction is underway before notifying the user.
	// This prevents the ⏳ from racing ahead of buffered content messages.
	// Timeout/error here is non-fatal: send the notification anyway and
	// continue waiting for completion below.
	if csw, ok := be.(delegator.CompactionStartWaiter); ok {
		_ = csw.WaitForCompactionStart(cctx)
	}
	for _, fn := range a.CompactionStartFunc {
		fn(sessionKey, "⏳ Compacting context...")
	}

	var waitErr error
	if cw, ok := be.(delegator.CompactionWaiter); ok {
		waitErr = cw.WaitForCompaction(cctx)
	} else {
		waitErr = be.WaitForTurn(cctx)
	}
	if waitErr != nil {
		return fmt.Errorf("wait for compaction: %w", waitErr)
	}

	for _, fn := range a.CompactionNotifyFunc {
		fn(sessionKey, "✅ Context compacted (delegated).")
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
	if a.ReloadOnCompact && a.DelegatedManager != nil {
		a.DelegatedManager.BounceSession(sessionKey)
		a.maybeInjectCompactionResume(sessionKey)
	}
	return nil
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
//     nudge would be redundant (and could race a second turn).
//
// Runs from the post-turn worker goroutine (auto-compaction) or the command
// handler (manual /compact); both serialise with the inbox check below.
func (a *Agent) maybeInjectCompactionResume(sessionKey string) {
	if a.AsyncNotifier == nil {
		return
	}
	if a.InboxHasPendingInput(sessionKey) {
		a.logger().Debugf("session=%s skip compaction-resume nudge: user input already queued", sessionKey)
		return
	}
	a.AsyncNotifier.InjectToAgent(sessionKey, compactionResumePrompt, "", "compaction-resume")
}

// maybeCompact checks whether context compaction is needed and performs it.
// Three triggers: (1) main threshold, (2) mana-refresh, (3) user /compact.
func (a *Agent) maybeCompact(ctx context.Context, sessionKey string, messages []provider.Message, system []provider.SystemBlock, usage *provider.Usage, sm *sessionMeta) {
	if a.Compactor == nil {
		return
	}

	totalTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	ctxLimit := a.SessionContextLimit(sessionKey)

	// Check mana-refresh trigger: compact at a lower threshold when mana
	// reset is imminent so the new window starts with a smaller context.
	isManaRefresh := false
	if a.AutocompactBeforeManaRefresh {
		usageClient := a.SessionUsageClient(sessionKey)
		if usageClient != nil {
			manaRefreshThreshold := parseDurationFallback(a.AutocompactBeforeManaRefreshThreshold, 5*time.Minute)
			if w, err := usageClient.GetUsage(ctx); err == nil && w != nil && !w.ResetsAt.IsZero() {
				if compaction.ManaResetImminent(w.ResetsAt, manaRefreshThreshold) {
					secondaryThreshold := int(float64(ctxLimit) * a.Compactor.Threshold() * a.AutocompactBeforeManaRefreshFactor)
					if totalTokens > secondaryThreshold {
						isManaRefresh = true
						untilReset := time.Until(w.ResetsAt).Round(time.Minute)
						a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens)",
							sessionKey, untilReset, totalTokens, ctxLimit)
					}
				}
			}
		}
	}

	// Standard threshold check (if mana-refresh didn't trigger)
	if !isManaRefresh {
		if !a.Compactor.ShouldCompactWithLimit(sessionKey, messages, usage, ctxLimit) {
			return
		}
	}

	if a.SessionNoCompact(sessionKey) {
		percent := int(float64(totalTokens) / float64(ctxLimit) * 100)
		a.logger().Infof("session=%s context at %d%% capacity for no_compact session", sessionKey, percent)
		return
	}

	// Mana-refresh mode: preserve more messages than normal compaction.
	// Priority: explicit *int count > percentage-based > normal preserve count.
	if isManaRefresh {
		oldPreserve := a.Compactor.PreserveMessages()
		defer a.Compactor.SetPreserveMessages(oldPreserve)

		if a.AutocompactBeforeManaRefreshPreserve != nil {
			// Explicit message count configured — use it directly.
			a.Compactor.SetPreserveMessages(*a.AutocompactBeforeManaRefreshPreserve)
		} else {
			// Percentage-based: preserve AutocompactBeforeManaRefreshPreservePct of messages
			// (default 0.5 = 50%). This ensures meaningful summarisation of older messages
			// while keeping the recent half of the conversation intact.
			pct := a.AutocompactBeforeManaRefreshPreservePct
			if pct <= 0 || pct > 1.0 {
				pct = 0.5
			}
			preserveN := int(float64(len(messages)) * pct)
			a.Compactor.SetPreserveMessages(preserveN)
		}
	}

	for _, fn := range a.CompactionMemoryFunc {
		fn(sessionKey)
	}

	oldCount := len(messages)
	result, err := a.doCompact(ctx, sessionKey, system, oldCount, false)
	if err != nil {
		a.logger().Errorf("session=%s %v", sessionKey, err)
	} else if result.NewSessionKey != "" {
		a.logger().Infof("session=%s compaction rotated → %s (pre_messages=%d)", sessionKey, result.NewSessionKey, oldCount)
	}

	// Reload system prompt — compaction may have changed memory files.
	// Only invalidate THIS session's cached system blocks so other sessions
	// keep their byte-identical prompts and don't suffer cache busts.
	a.Bootstrap.Reload()
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	sm.systemBlocks = nil
	// Reset cache baseline — next request will have a different prefix
	sm.prevCacheRead = 0
}

// parseDurationFallback parses a Go duration string, returning fallback on error or empty.
func parseDurationFallback(s string, fallback time.Duration) time.Duration {
	if s == "" || s == "0" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
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

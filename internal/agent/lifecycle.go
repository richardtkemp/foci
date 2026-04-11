package agent

// lifecycle.go — agent lifecycle operations (reset, compact, reload).
//
// These methods encapsulate multi-step sequences that were previously
// orchestrated by command handlers reaching into agent internals.
// Commands become thin wrappers that call these methods.

import (
	"context"
	"fmt"

	"foci/internal/log"
	"foci/shared/prompts"
)

// reloadAfterMutation reloads the bootstrap (system prompt files from disk),
// refreshes nudge rules, and invalidates all per-session system prompt caches.
//
// Call after any explicit user action that changes the system prompt or session
// state (e.g. /reset, /reload, /compact). Auto-compaction (maybeCompact) does
// its own targeted per-session cache reset instead.
func (a *Agent) reloadAfterMutation() {
	a.Bootstrap.Reload()
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	a.InvalidateSystemCaches()
}

// ReloadSystem reloads the bootstrap (system prompt files from disk),
// refreshes nudge rules, invalidates system caches, and reloads extra
// system blocks (e.g. skills) via the ReloadSystemFn callback.
// Returns the number of reloaded extra items (e.g. skills count).
func (a *Agent) ReloadSystem() int {
	a.reloadAfterMutation()

	if a.ReloadSystemFn == nil {
		return 0
	}
	blocks, count := a.ReloadSystemFn()
	a.ExtraSystemBlocks = blocks
	return count
}

// ResetSession clears session history with memory formation.
//
// For API agents: fires memory formation as an async branch, then rotates
// the session key and reloads the bootstrap.
//
// For delegated agents: sends a memory formation prompt to the live backend
// session, waits for completion, destroys the backend session, then rotates.
//
// Returns the new session key on success.
func (a *Agent) ResetSession(ctx context.Context, sessionKey string) (string, error) {
	if a.IsProcessing() {
		return "", fmt.Errorf("agent is processing — send stop first, then reset")
	}

	if a.DelegatedManager != nil {
		return a.resetDelegatedSession(ctx, sessionKey)
	}

	// Traditional mode: fire memory formation as a branch, then rotate.
	// Memory runs on a separate branch — safe to proceed without waiting.
	orientTemplate := ""
	if a.ResetOrientTemplateFn != nil {
		orientTemplate = a.ResetOrientTemplateFn()
	}
	go a.FireSessionEndMemory(ctx, sessionKey, orientTemplate, false)

	newKey, err := a.Sessions.RotateKey(sessionKey)
	if err != nil {
		return "", err
	}
	a.RotateSession(sessionKey, newKey)
	a.reloadAfterMutation()
	return newKey, nil
}

// CompactSession triggers manual context compaction for a session.
// When dryRun is true, the full pipeline runs but the session is left unchanged.
//
// Platform concerns (e.g. sending the dry-run summary as a document) are
// handled via CompactionDebugFunc hooks. If no hooks are registered, the
// summary is returned in CompactResult.Summary for the caller to handle.
//
// For delegated agents, CC owns the session file and the compaction mechanics —
// this method sends "/compact $instructions" to CC and waits for the boundary
// signal. Dry-run is not supported because CC has no dry-run mode.
func (a *Agent) CompactSession(ctx context.Context, sessionKey string, dryRun bool) (CompactResult, error) {
	// Neither transport available — agent is misconfigured. Surface this
	// first because it's more diagnostic than "no active session".
	if a.Compactor == nil && a.DelegatedManager == nil {
		return CompactResult{}, fmt.Errorf("compaction is not configured")
	}
	if sessionKey == "" {
		return CompactResult{}, fmt.Errorf("no active session to compact")
	}

	// Fire memory formation before compaction so the memory agent sees the
	// pre-compaction transcript. Both auto paths (maybeCompact for API,
	// RunCompaction for delegated) fire this hook; manual /compact used to
	// skip it, losing the chance to save memories before summarisation.
	// Dry-run is excluded — it must not cause side effects.
	if !dryRun {
		for _, fn := range a.CompactionMemoryFunc {
			fn(sessionKey)
		}
	}

	if a.DelegatedManager != nil {
		return a.compactDelegatedSession(ctx, sessionKey, dryRun)
	}

	mc, _ := a.Sessions.MessageCount(sessionKey)
	if mc < 5 {
		return CompactResult{}, fmt.Errorf("too few messages to compact (%d)", mc)
	}

	system := a.Bootstrap.SystemBlocks()
	result, err := a.doCompact(ctx, sessionKey, system, mc, dryRun)
	if err != nil {
		return result, err
	}

	if !dryRun {
		a.reloadAfterMutation()
		resetKey := sessionKey
		if result.NewSessionKey != "" {
			resetKey = result.NewSessionKey
		}
		a.ResetCacheBaseline(resetKey)
	}
	return result, nil
}

// compactDelegatedSession handles manual /compact for backend-managed agents.
// Looks up the backend and calls the shared runDelegatedCompact primitive.
// Returns an empty CompactResult on success — CC owns the session file so
// foci has no message count to report.
func (a *Agent) compactDelegatedSession(ctx context.Context, sessionKey string, dryRun bool) (CompactResult, error) {
	if dryRun {
		return CompactResult{}, fmt.Errorf("dry-run compaction is not supported for delegated backends")
	}
	be, err := a.DelegatedManager.Get(ctx, sessionKey)
	if err != nil {
		return CompactResult{}, fmt.Errorf("get backend: %w", err)
	}
	if err := a.runDelegatedCompact(ctx, be, sessionKey); err != nil {
		return CompactResult{}, err
	}
	return CompactResult{}, nil
}

// resetDelegatedSession handles session reset for backend-managed agents.
// Sends memory formation prompt to the live backend session (blocking until
// CC completes), then destroys the backend and rotates the session key.
func (a *Agent) resetDelegatedSession(ctx context.Context, sessionKey string) (string, error) {
	for _, fn := range a.ResetNotifyFunc {
		fn(sessionKey, "♻️ Session reset — saving memories...")
	}

	// Send memory formation prompt to the live backend session.
	// HandleMessage blocks until CC completes the turn.
	if a.Reflection.SessionEndEnabled {
		prompt := prompts.ResolvePrompt(
			a.Reflection.SessionEndPrompt,
			"reflection.md", prompts.Reflection(),
			a.PromptSearchDirs...)
		if prompt != "" {
			log.Infof("reset", "sending memory formation to backend session %s", sessionKey)
			if err := a.HandleMessage(ctx, sessionKey, []string{prompt}, nil); err != nil {
				log.Warnf("reset", "memory formation failed for %s: %v", sessionKey, err)
			}
		}
	}

	// Destroy backend — HandleMessage already completed, safe to close.
	a.DelegatedManager.ResetSession(sessionKey)

	// Rotate foci session key, reload, invalidate caches.
	newKey, err := a.Sessions.RotateKey(sessionKey)
	if err != nil {
		return "", err
	}
	a.RotateSession(sessionKey, newKey)
	a.reloadAfterMutation()
	return newKey, nil
}

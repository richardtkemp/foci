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
func (a *Agent) CompactSession(ctx context.Context, sessionKey string, dryRun bool) (CompactResult, error) {
	if a.Compactor == nil {
		return CompactResult{}, fmt.Errorf("compaction is not configured")
	}
	if sessionKey == "" {
		return CompactResult{}, fmt.Errorf("no active session to compact")
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

// resetDelegatedSession handles session reset for backend-managed agents.
// Sends memory formation prompt to the live backend session (blocking until
// CC completes), then destroys the backend and rotates the session key.
func (a *Agent) resetDelegatedSession(ctx context.Context, sessionKey string) (string, error) {
	for _, fn := range a.ResetNotifyFunc {
		fn(sessionKey, "♻️ Session reset — saving memories...")
	}

	// Send memory formation prompt to the live backend session.
	// HandleMessage blocks until CC completes the turn.
	if a.MemoryFormationConfig.SessionEndEnabled {
		prompt := prompts.ResolvePrompt(
			a.MemoryFormationConfig.SessionEndPrompt,
			"memory-formation.md", prompts.MemoryFormation(),
			a.PromptSearchDirs...)
		if prompt != "" {
			log.Infof("reset", "sending memory formation to backend session %s", sessionKey)
			if _, err := a.HandleMessage(ctx, sessionKey, prompt); err != nil {
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

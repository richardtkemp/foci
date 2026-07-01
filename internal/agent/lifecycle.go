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
	"foci/internal/session"
)

// reloadAfterMutation reloads the bootstrap (system prompt files from disk),
// refreshes nudge rules, and invalidates all per-session system prompt caches.
//
// Call after any explicit user action that changes the system prompt or session
// state (e.g. /reset, /compact). Auto-compaction (maybeCompact) does
// its own targeted per-session cache reset instead.
func (a *Agent) reloadAfterMutation() {
	a.Bootstrap.Reload()
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	a.InvalidateSystemCaches()
}

// ResetSession clears session history with memory formation.
//
// For API agents: fires memory formation as an async branch, then rotates
// the session key and reloads the bootstrap.
//
// For delegated agents: rotates immediately to a fresh session, then runs
// memory formation on the old CC session and destroys it in the background —
// the caller is not blocked on reflection.
//
// Returns the new session key on success.
func (a *Agent) ResetSession(ctx context.Context, sessionKey string) (string, error) {
	if a.IsTurnInFlight(session.SessionKeyBase(sessionKey)) {
		return "", fmt.Errorf("session is processing — send stop first, then reset")
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

// ResetSessionHard clears the session immediately, without waiting for any
// in-flight turn and without sending memory formation prompts.
//
// Cancels the in-flight turn ctx (if any), interrupts the delegated backend
// (best-effort), destroys the backend, rotates the session key, and reloads
// the bootstrap. Use this when the agent is stuck or the user wants a clean
// reset without saving session memories.
//
// Unlike ResetSession, this does not check IsProcessing — that's the whole
// point. Cancellation propagates through the same paths /stop uses.
//
// Returns the new session key on success.
func (a *Agent) ResetSessionHard(ctx context.Context, sessionKey string) (string, error) {
	if sessionKey == "" {
		return "", fmt.Errorf("no active session to reset")
	}

	// Cancel any in-flight turn ctx. CancelSession is a no-op if no turn is
	// running, so this is safe to call unconditionally.
	a.CancelSession(sessionKey)

	if a.DelegatedManager != nil {
		// Interrupt CC if there's a backend. Best-effort — backend may not
		// exist yet, or interrupt may fail; we proceed to Close regardless.
		if err := a.DelegatedManager.StopSession(ctx, sessionKey); err != nil {
			log.Debugf("reset", "hard reset interrupt for %s: %v (proceeding to close)", sessionKey, err)
		}
		a.DelegatedManager.ResetSession(sessionKey)
	}

	newKey, err := a.Sessions.RotateKey(sessionKey)
	if err != nil {
		return "", err
	}
	a.RotateSession(sessionKey, newKey)
	a.reloadAfterMutation()
	return newKey, nil
}

// resetDelegatedSession resets a backend-managed session without blocking the
// caller. It rotates the foci session key immediately — the chat maps to a
// fresh session right away (a new CC backend is spawned lazily on the next
// message) — then runs reflection on the old CC session and tears it down in
// the background.
//
// The old backend deliberately stays mapped under sessionKey so the background
// reflection can drive it (RotateSession no longer migrates backends). The old
// CC resume ID is cleared up front so the rotated-to key starts a genuinely
// fresh CC session rather than inheriting the old conversation when its
// metadata is renamed forward.
func (a *Agent) resetDelegatedSession(ctx context.Context, sessionKey string) (string, error) {
	orientTemplate := ""
	if a.ResetOrientTemplateFn != nil {
		orientTemplate = a.ResetOrientTemplateFn()
	}

	// Drop the old CC resume ID before rotating: RotateSession renames
	// sessionKey's metadata (including cc_resume_id) forward to newKey, and we
	// must not let the fresh session resume the old conversation. The live
	// backend stays in the manager map, so reflection below still reaches it.
	a.DelegatedManager.ClearResumeID(sessionKey)

	newKey, err := a.Sessions.RotateKey(sessionKey)
	if err != nil {
		return "", err
	}
	a.RotateSession(sessionKey, newKey)
	a.reloadAfterMutation()

	// Reflect on the live CC session, then destroy it — in the background so
	// the user isn't blocked. FireSessionEndMemory injects the reflection into
	// the existing CC session (it special-cases delegated mode) and blocks
	// internally until CC completes, so the backend is only closed once
	// memories are saved. Detach from the request context so the work survives
	// this call returning.
	bg := context.WithoutCancel(ctx)
	go func() {
		a.FireSessionEndMemory(bg, sessionKey, orientTemplate, false)
		a.DelegatedManager.ResetSession(sessionKey)
		// Reflection ran after the metadata rename, so it left rows under the
		// now-defunct old key. Clear them so nothing leaks.
		if a.SessionIndex != nil {
			if err := a.SessionIndex.DeleteAllSessionMetadata(sessionKey); err != nil {
				log.Warnf("reset", "cleanup metadata for %s: %v", sessionKey, err)
			}
		}
	}()

	return newKey, nil
}

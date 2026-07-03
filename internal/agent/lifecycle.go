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

// ResetSession clears session history with memory formation. The session key
// is a stable identity — the history is archived in place and per-session
// state cleared, but the key (and everything that holds it) is unchanged.
//
// Sequence, unified across API and delegated transports:
//
//  1. Prepare the reflection branch against the still-live history. For
//     delegated agents this also remaps the live backend (and its resume ID)
//     to the branch, so the main key is clean for a fresh backend.
//  2. Archive the session file in place (Store.Reset).
//  3. Clear per-session state (model/effort overrides, cc_resume_id,
//     no_compact, …) so the reset session starts from a clean slate.
//  4. Run reflection on the branch in the background — the caller is not
//     blocked on it.
// ResetMemoryOutcome reports what happened to the previous session's memories on
// a soft reset, so the caller can tell the user accurately.
type ResetMemoryOutcome int

const (
	// ResetMemoryNone: no reflection ran (session-end memory disabled, or the
	// reset couldn't fire one right now).
	ResetMemoryNone ResetMemoryOutcome = iota
	// ResetMemoryReflecting: a reflection pass was started in the background.
	ResetMemoryReflecting
	// ResetMemoryAlreadySaved: a prior reflection already captured this session
	// and nothing substantive happened since, so there was nothing new to save.
	ResetMemoryAlreadySaved
)

func (a *Agent) ResetSession(ctx context.Context, sessionKey string) (ResetMemoryOutcome, error) {
	if a.IsTurnInFlight(sessionKey) {
		return ResetMemoryNone, fmt.Errorf("session is processing — send stop first, then reset")
	}

	orientTemplate := ""
	if a.ResetOrientTemplateFn != nil {
		orientTemplate = a.ResetOrientTemplateFn()
	}
	reflectKey, doReflect := a.PrepareSessionEndMemory(sessionKey, orientTemplate, false)

	// Classify the memory outcome before the reset mutates index state. A
	// no-reflection reset is "already saved" only when a prior reflection
	// covered everything (ReflectionRedundant); any other reason
	// (disabled/rate-limited) is ResetMemoryNone.
	outcome := ResetMemoryNone
	if doReflect {
		outcome = ResetMemoryReflecting
	} else if a.SessionIndex != nil && a.SessionIndex.ReflectionRedundant(sessionKey) {
		outcome = ResetMemoryAlreadySaved
	}

	// No reflection branch adopted the live backend — make sure the main key
	// doesn't keep one (delegated agents: close it and clear the resume ID so
	// the next message starts a genuinely fresh CC session).
	if !doReflect && a.DelegatedManager != nil {
		a.DelegatedManager.ResetSession(sessionKey)
	}

	if err := a.Sessions.Reset(sessionKey); err != nil {
		return ResetMemoryNone, err
	}
	a.ClearSessionState(sessionKey)
	a.reloadAfterMutation()

	if doReflect {
		// Detach from the request context so the reflection survives this
		// call returning. RunSessionEndMemory destroys the branch's backend
		// when it finishes.
		bg := context.WithoutCancel(ctx)
		go a.RunSessionEndMemory(bg, reflectKey)
	}
	return outcome, nil
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
		a.ResetCacheBaseline(sessionKey)
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
// (best-effort), destroys the backend, archives the session file in place,
// clears per-session state, and reloads the bootstrap. Use this when the agent
// is stuck or the user wants a clean reset without saving session memories.
//
// Unlike ResetSession, this does not check IsProcessing — that's the whole
// point. Cancellation propagates through the same paths /stop uses.
func (a *Agent) ResetSessionHard(ctx context.Context, sessionKey string) error {
	if sessionKey == "" {
		return fmt.Errorf("no active session to reset")
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

	if err := a.Sessions.Reset(sessionKey); err != nil {
		return err
	}
	a.ClearSessionState(sessionKey)
	a.reloadAfterMutation()
	return nil
}

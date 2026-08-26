package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// ForkSession forks a Codex thread into a new thread ID by copying stored
// history. Implements delegator.BackendBrancher.
//
// The fork is an app-server RPC and therefore requires this backend to be
// running. DelegatedManager owns starting/resuming the parent before calling
// this method, so the connection remains available for the parent session.
func (b *Backend) ForkSession(ctx context.Context, req delegator.ForkRequest) (delegator.ForkResult, error) {
	if !b.IsRunning() {
		return delegator.ForkResult{}, fmt.Errorf("codex: backend not started (fork requires an active app-server connection)")
	}

	parentID := req.ParentSessionID
	if req.FociSessionKey != "" {
		if mapped := b.threadForSession(req.FociSessionKey); mapped != "" {
			parentID = mapped
		}
	}
	params := threadForkParams{ThreadID: parentID, Ephemeral: false}
	if req.TruncateAfter > 0 {
		return delegator.ForkResult{}, fmt.Errorf("codex: truncateAfter is not yet supported")
	}

	result, err := b.sendAndWait("thread/fork", params)

	if err != nil {
		return delegator.ForkResult{}, fmt.Errorf("codex: thread/fork failed: %w", err)
	}

	var tr threadResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return delegator.ForkResult{}, fmt.Errorf("codex: parse thread/fork response: %w", err)
	}

	// Deliberately do NOT registerThread here. req.FociSessionKey is the
	// PARENT's key (DelegatedManager.ForkParentSession passes parentKey), so
	// binding it to the fork's thread repoints the still-live parent: its next
	// beginTurn/SessionID/Interrupt would resolve through sessionThreads to the
	// FORK's thread, and the fork's notifications would route to the parent's
	// facade. Worse, when the branch session later closes, unregisterThread
	// deletes every key pointing at that thread -- including the parent's --
	// stranding the parent on "codex: no active thread".
	//
	// The mapping is not needed here anyway: the caller binds the forked thread
	// to its NEW branch session key and starts a backend with
	// ResumeSessionID=<forked thread>, and resumeThread already registers
	// (startOpts.SessionKey -> threadID) for that session.
	return delegator.ForkResult{SessionID: tr.Thread.ID}, nil
}

// ForkRequiresRunningBackend tells DelegatedManager to start/resume this
// backend before issuing a fork request.
func (b *Backend) ForkRequiresRunningBackend() bool { return true }

// noRolloutFoundMarker is the substring of codex's thread/delete error when
// the thread id has no on-disk rollout — verified live against codex
// app-server 0.144.5: identical wording ("no rollout found for thread id
// ...") for a never-existed id AND a thread already deleted once, matching
// BackendBrancher.CleanupSession's documented "deleting an already-absent
// session is NOT an error" contract.
const noRolloutFoundMarker = "no rollout found for thread id"

// OpenCleanupScope implements delegator.RunningBackendCleaner. Codex deletes a
// thread by RPC to a live app-server, so an idle agent could never have its
// expired sessions collected: the daily sweep built a fresh Backend, found no
// connection, and failed once per session — permanently, for exactly the agents
// whose sessions were expiring. 1df68081 fixed this bug class for opencode on
// 2026-08-15 but left codex out, because WIRING recorded codex as deleting
// straight from disk. It does not.
//
// Acquisition is deliberately Start, the SAME path an interactive session or a
// background batch uses, so a cleanup-triggered spawn is indistinguishable from
// any other: it attaches to the pooled app-server for this agent, or launches
// one if none exists. BatchOnly is what makes it safe to reuse — it returns as
// soon as the connection is ready, BEFORE Start would otherwise start or resume
// a thread. Cleaning up must not create the very thing it reclaims.
//
// Release closes this facade, handing back the pool ref; if this scope launched
// the app-server, that drops the refcount to zero and shuts it down again.
func (b *Backend) OpenCleanupScope(ctx context.Context, req delegator.CleanupRequest) (func(), error) {
	if req.AgentID == "" {
		return nil, fmt.Errorf("codex cleanup scope: empty agent id")
	}
	if err := b.Start(ctx, delegator.StartOptions{
		WorkDir:   req.WorkDir,
		AgentID:   req.AgentID,
		BatchOnly: true,
	}); err != nil {
		return nil, fmt.Errorf("codex cleanup scope: start backend: %w", err)
	}
	return func() { _ = b.Close() }, nil
}

// pooledOwner returns the app-server owner for agentID, or nil. A Backend built
// fresh by DelegatedManager.NewBackend has never run Start, so its `shared`
// pointer is nil and process() returns itself — with no writer. The sweep builds
// exactly such a Backend per session, so without this lookup an OpenCleanupScope
// that successfully launched a server would still be invisible to every
// CleanupSession that followed it, and the fix would appear to do nothing.
func pooledOwner(agentID string) *Backend {
	if agentID == "" {
		return nil
	}
	sharedPool.Lock()
	owner := sharedPool.servers[agentID]
	sharedPool.Unlock()
	if owner == nil || !owner.IsRunning() {
		return nil
	}
	return owner
}

// CleanupSession deletes a Codex thread by ID. Implements
// delegator.BackendBrancher.
func (b *Backend) CleanupSession(ctx context.Context, req delegator.CleanupRequest) error {
	p := b.process()
	p.mu.Lock()
	wr := p.writer
	p.mu.Unlock()
	if wr == nil {
		// No connection of our own: fall back to the agent's pooled app-server,
		// which OpenCleanupScope guarantees for the duration of a sweep.
		if owner := pooledOwner(req.AgentID); owner != nil {
			b = owner
			p = owner
			p.mu.Lock()
			wr = p.writer
			p.mu.Unlock()
		}
	}
	if wr == nil {
		return fmt.Errorf("codex: backend not started (cleanup requires an active app-server connection)")
	}

	_, err := b.sendAndWait("thread/delete", struct {
		ThreadID string `json:"threadId"`
	}{
		ThreadID: req.SessionID,
	})
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), noRolloutFoundMarker) {
		// Already absent (never existed, or a prior delete already ran) —
		// documented as not-an-error, matching the contract above.
		b.logDebugf("thread/delete for %s: already absent: %v", req.SessionID, err)
		return nil
	}
	// A genuine failure was previously swallowed here (Debugf, return nil)
	// regardless of cause, so callers had no way to notice or retry a real
	// cleanup failure (e.g. a live app-server RPC error unrelated to the
	// session already being gone).
	b.logWarnf("thread/delete for %s failed: %v", req.SessionID, err)
	return fmt.Errorf("codex: thread/delete failed: %w", err)
}

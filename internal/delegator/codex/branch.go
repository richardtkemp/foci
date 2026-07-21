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

	params := struct {
		ThreadID   string `json:"threadId"`
		LastTurnID string `json:"lastTurnId,omitempty"`
	}{
		ThreadID: req.ParentSessionID,
	}
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

// CleanupSession deletes a Codex thread by ID. Implements
// delegator.BackendBrancher.
func (b *Backend) CleanupSession(ctx context.Context, req delegator.CleanupRequest) error {
	b.mu.Lock()
	wr := b.writer
	b.mu.Unlock()
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
		b.lg.Debugf("thread/delete for %s: already absent: %v", req.SessionID, err)
		return nil
	}
	// A genuine failure was previously swallowed here (Debugf, return nil)
	// regardless of cause, so callers had no way to notice or retry a real
	// cleanup failure (e.g. a live app-server RPC error unrelated to the
	// session already being gone).
	b.lg.Warnf("thread/delete for %s failed: %v", req.SessionID, err)
	return fmt.Errorf("codex: thread/delete failed: %w", err)
}

package codex

import (
	"context"
	"encoding/json"
	"fmt"

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

// CleanupSession deletes a Codex thread by ID. Implements
// delegator.BackendBrancher.
func (b *Backend) CleanupSession(ctx context.Context, req delegator.CleanupRequest) error {
	if b.writer == nil {
		return fmt.Errorf("codex: backend not started (cleanup requires an active app-server connection)")
	}

	_, err := b.sendAndWait("thread/delete", struct {
		ThreadID string `json:"threadId"`
	}{
		ThreadID: req.SessionID,
	})
	if err != nil {
		b.lg.Debugf("thread/delete for %s: %v", req.SessionID, err)
	}
	return nil
}

package ccstream

import (
	"context"
)

// SetOnCompactionStart sets a callback fired when CC begins compacting.
func (b *Backend) SetOnCompactionStart(fn func()) { b.onCompactionStart = fn }

// SetOnCompactionDone sets a callback fired when CC finishes compaction.
// preTokens is the token count before compaction.
func (b *Backend) SetOnCompactionDone(fn func(preTokens int)) { b.onCompactionDone = fn }

// ArmCompactionWait sets up a one-shot channel that will be closed when
// compact_boundary is received. Must be called before the /compact command
// is sent so the signal is never missed.
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	b.compactDoneCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// WaitForCompaction blocks until compact_boundary is received or ctx expires.
// Returns immediately if no waiter is armed (ArmCompactionWait was not called).
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactDoneCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ArmCompactionStartWait sets up a one-shot channel that will be closed when
// status="compacting" is received. Must be called before the /compact command
// is sent so the signal is never missed.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	b.compactStartCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// WaitForCompactionStart blocks until status="compacting" is received or ctx
// expires. Returns immediately if no waiter is armed.
func (b *Backend) WaitForCompactionStart(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactStartCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

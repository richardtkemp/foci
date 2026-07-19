package ccstream

import (
	"context"

	"foci/internal/delegator"
)

// SetOnCompactionStart sets a callback fired when CC begins compacting.
func (b *Backend) SetOnCompactionStart(fn func()) { b.onCompactionStart = fn }

// SetOnCompactionDone sets a callback fired when CC finishes compaction.
// preTokens is the token count before compaction.
func (b *Backend) SetOnCompactionDone(fn func(preTokens int)) { b.onCompactionDone = fn }

// ArmCompactionWait sets up the one-shot channels signalled when compaction
// settles: compactDoneCh on compact_boundary (success), compactAbortCh when the
// /compact run goes idle without a boundary (backend declined — #1267). Must be
// called before the /compact command is sent so neither signal is missed.
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	b.compactDoneCh = make(chan struct{}, 1)
	b.compactAbortCh = make(chan struct{}, 1)
	b.turnMu.Unlock()
}

// signalCompactionAbort fires the abort waiter if a compaction wait is still
// armed (compact_boundary never arrived) and clears both channels. Called from
// the idle handler: since compact_boundary always precedes idle on a real
// compaction (it nils compactDoneCh first), a still-armed waiter at idle means
// the backend declined to compact. No-op if no wait is armed or a boundary
// already fired. Caller must hold turnMu.
func (b *Backend) signalCompactionAbort() {
	if b.compactDoneCh == nil {
		return
	}
	if b.compactAbortCh != nil {
		select {
		case b.compactAbortCh <- struct{}{}:
		default:
		}
	}
	b.compactDoneCh = nil
	b.compactAbortCh = nil
}

// WaitForCompaction blocks until compact_boundary is received (nil), the
// /compact run goes idle without a boundary (ErrCompactionNoBoundary), or ctx
// expires. Returns immediately if no waiter is armed (ArmCompactionWait was not
// called).
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	done := b.compactDoneCh
	abort := b.compactAbortCh
	b.turnMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-abort:
		return delegator.ErrCompactionNoBoundary
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

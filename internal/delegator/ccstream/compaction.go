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

// ArmCompactionWait sets up the one-shot outcome channel signalled when
// compaction settles: resolveCompactionWait(nil) on compact_boundary
// (success), resolveCompactionWait(ErrCompactionNoBoundary) when the /compact
// run goes idle without a boundary (backend declined — #1267). Must be called
// before the /compact command is sent so neither outcome is missed.
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	b.compactCh = make(chan error, 1)
	b.turnMu.Unlock()
}

// resolveCompactionWait records the resolved outcome for the current
// compaction wait (if one is armed) and wakes any blocked WaitForCompaction
// caller. Caller must hold turnMu.
//
// Deliberately does NOT clear b.compactCh — only a WaitForCompaction call
// that has actually received a value resets it (see there). That is what
// keeps this race-free (#1526): the old design signalled resolution by nil-ing
// shared fields, so a WaitForCompaction call that hadn't yet captured its
// local copies of those fields could observe them already nil'd by whichever
// path resolved first and misread "already resolved via abort" as "never
// armed", silently returning success for a declined compaction. Here the
// channel identity never changes underneath a late reader — it always
// captures the same non-nil channel and receives whatever value was (or will
// be) sent into it, so the outcome is never lost regardless of scheduling.
//
// The buffered(1) capacity also preserves the original exclusivity property
// (a legitimate boundary success and a later spurious abort must never both
// be observable, or a select would pick between them pseudo-randomly): once
// one outcome lands in the single buffer slot, a second resolveCompactionWait
// call for the same arm hits the full-buffer default case and no-ops.
func (b *Backend) resolveCompactionWait(err error) {
	if b.compactCh == nil {
		return
	}
	select {
	case b.compactCh <- err:
	default:
	}
}

// signalCompactionAbort resolves the compaction wait with
// ErrCompactionNoBoundary if one is still armed (compact_boundary never
// arrived). Called from the idle handler: since compact_boundary always
// precedes idle on a real compaction (it resolves the wait first), a still-
// armed waiter at idle means the backend declined to compact. No-op if no
// wait is armed or a boundary already resolved it. Caller must hold turnMu.
func (b *Backend) signalCompactionAbort() {
	b.resolveCompactionWait(delegator.ErrCompactionNoBoundary)
}

// WaitForCompaction blocks until compact_boundary is received (nil), the
// /compact run goes idle without a boundary (ErrCompactionNoBoundary), or ctx
// expires. Returns immediately if no waiter is armed (ArmCompactionWait was not
// called, or a previous call already consumed this arm's outcome).
func (b *Backend) WaitForCompaction(ctx context.Context) error {
	b.turnMu.Lock()
	ch := b.compactCh
	b.turnMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case err := <-ch:
		b.turnMu.Lock()
		if b.compactCh == ch {
			b.compactCh = nil
		}
		b.turnMu.Unlock()
		return err
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

// compaction.go — CompactionWaiter + CompactionStartWaiter implementation.
// All four compaction-wait methods live here per plan §8.2.
//
// Implements delegator.CompactionWaiter (ArmCompactionWait /
// WaitForCompaction) + delegator.CompactionStartWaiter
// (ArmCompactionStartWait / WaitForCompactionStart).

package opencode

import "context"

// ArmCompactionWait resets compactDoneCh for the next /compact cycle.
// Closed by onSessionCompacted (Step 7's handler).
func (b *Backend) ArmCompactionWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactDoneCh = make(chan struct{}, 1)
}

// WaitForCompaction blocks on compactDoneCh or ctx. Returns immediately
// (nil) if not armed — matches ccstream's no-arm semantics.
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

// ArmCompactionStartWait arms the one-shot "compaction started" waiter.
// Since opencode has no status="compacting" event, we synthesise it by
// immediately closing compactStartCh — the /compact command was already
// accepted by the server (Inject SourceCompact fired), so "compaction
// started" is a reasonable proxy.
//
// Documented divergence (plan §8.2): the "⏳ notification" appears
// instantly rather than after the LLM has started summarising. For
// operators who rely on the notification as a "compaction is underway"
// signal, this is slightly premature but not misleading — the
// compaction IS in progress, just not yet at the LLM stage.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactStartCh = make(chan struct{}, 1)
	// Immediately close — synthesise "started" since we can't detect
	// the real start. WaitForCompactionStart will return immediately.
	close(b.compactStartCh)
}

// WaitForCompactionStart blocks on compactStartCh or ctx. Since
// ArmCompactionStartWait closes the channel immediately, this always
// returns nil right away (unless called without arming → returns nil
// per the no-arm contract, matching WaitForCompaction's behaviour).
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

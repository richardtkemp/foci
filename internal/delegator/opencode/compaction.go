// compaction.go — CompactionWaiter + CompactionStartWaiter implementation.
// All four compaction-wait methods live here.
//
// Implements delegator.CompactionWaiter (ArmCompactionWait /
// WaitForCompaction) + delegator.CompactionStartWaiter
// (ArmCompactionStartWait / WaitForCompactionStart).

package opencode

import "context"

// ArmCompactionWait resets compactDoneCh for the next /compact cycle.
// Closed by onSessionCompacted (handlers.go).
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
// The channel is closed by handleCompactionPart (handlers.go) when the
// compaction part arrives — opencode's real start signal, emitted at
// summarize initiation, before the summary LLM streams (~2.5s ahead of
// the first reasoning token, measured on 1.17.11). Must be called before
// Inject(SourceCompact) so the signal is never missed; see
// agent.runDelegatedCompact.
func (b *Backend) ArmCompactionStartWait() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.compactStartCh = make(chan struct{}, 1)
}

// WaitForCompactionStart blocks until the compaction part arrives
// (compactStartCh closed by handleCompactionPart) or ctx expires. Returns
// nil immediately if not armed (matching WaitForCompaction's no-arm
// contract). The ctx branch is reachable: if /summarize is accepted but
// opencode never emits a compaction part, this unblocks on the context
// deadline so the ⏳ notification fires anyway (non-fatal — the wait for
// completion below proceeds regardless).
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

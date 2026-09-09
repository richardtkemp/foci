package ccstream

// Cache-write TTL accounting (#1866 phase 1).
//
// Anthropic prices a 1-hour cache write at ~1.6x a 5-minute one, and foci
// priced every write at the 1h rate. That was measured as correct for the main
// thread and WRONG for subagents, which cache at 5m: a 60% overcharge on their
// writes, reconciled to six decimals on two production turns and reproduced on
// a second model with a live probe.
//
// The split exists ONLY on per-message usage. The result message's ModelUsage
// merges it into one cacheCreationInputTokens, so a turn summarised from the
// result can no longer be split. Hence this accumulator, fed per assistant
// message.
//
// Two properties of CC's stream shape everything here, both probe-verified
// 2026-09-09 (CC 2.1.261, verify-cc-stream-hooks/ttl_probe.sh):
//
//  1. CC emits ONE assistant line PER CONTENT BLOCK, each repeating the whole
//     message-level usage. A three-block message arrives three times with
//     identical counts. Accumulating without dedupe therefore inflates by the
//     block count, not by some small margin — so dedupe by message id is
//     load-bearing, not hygiene.
//
//  2. Subagent messages carry a non-nil ParentToolUseID. Keeping their tokens
//     in a separate bucket is what lets a later phase reconcile the foreground
//     shortfall (a foreground subagent's pure-text message is suppressed from
//     the parent stream ALONG WITH its usage) without guessing which bucket the
//     missing tokens came from.

// cacheWriteSplit accumulates cache-write tokens by TTL class.
//
// Unknown is deliberately its own class rather than being folded into either
// rate. CC reporting cache-write tokens with no breakdown means the TTL is
// unobserved, which is a different fact from "they were 5m" — and assuming
// either one is exactly the mistake that produced this bug.
type cacheWriteSplit struct {
	Ephemeral5m int
	Ephemeral1h int
	Unknown     int
}

// total is every cache-write token seen, whatever its TTL.
func (s cacheWriteSplit) total() int {
	return s.Ephemeral5m + s.Ephemeral1h + s.Unknown
}

// add folds one API call's usage into the accumulator.
//
// When CC supplies a breakdown it is trusted verbatim, including the case where
// its parts do not sum to CacheCreationInputTokens: any shortfall lands in
// Unknown so the total still reconciles against ModelUsage. Silently discarding
// the difference would make this accumulator disagree with the figure it exists
// to explain.
func (s *cacheWriteSplit) add(u TokenUsage) {
	if u.CacheCreationInputTokens <= 0 && u.CacheCreation == nil {
		return
	}
	if u.CacheCreation == nil {
		s.Unknown += u.CacheCreationInputTokens
		return
	}
	s.Ephemeral5m += u.CacheCreation.Ephemeral5m
	s.Ephemeral1h += u.CacheCreation.Ephemeral1h
	if rest := u.CacheCreationInputTokens - u.CacheCreation.Ephemeral5m - u.CacheCreation.Ephemeral1h; rest > 0 {
		s.Unknown += rest
	}
}

// add returns s with o's counts added, class by class.
func (s cacheWriteSplit) addSplit(o cacheWriteSplit) cacheWriteSplit {
	return cacheWriteSplit{
		Ephemeral5m: s.Ephemeral5m + o.Ephemeral5m,
		Ephemeral1h: s.Ephemeral1h + o.Ephemeral1h,
		Unknown:     s.Unknown + o.Unknown,
	}
}

// noteCacheWriteSplit records one assistant message's cache-write TTL split
// against this turn, ignoring repeats of a message already counted.
//
// Called for EVERY assistant message including subagents' — the partition by
// ParentToolUseID happens here rather than at the call site, so a caller that
// filters subagents out for its own reasons cannot silently drop them from the
// accounting too. That coupling is what OnAssistant's existing top-level guard
// would otherwise create.
func (b *Backend) noteCacheWriteSplit(msg *AssistantMessage) {
	if msg == nil {
		return
	}
	id := msg.Message.ID
	usage := msg.Message.Usage

	b.turnMu.Lock()
	defer b.turnMu.Unlock()

	// An empty id cannot be deduped, so it is dropped rather than risking a
	// block-count inflation. CC has always supplied one; if that changes, the
	// totals go LOW against ModelUsage, which the divergence warning reports —
	// preferable to a silent multiple.
	if id == "" {
		return
	}
	if b.turnWriteSeen == nil {
		b.turnWriteSeen = make(map[string]struct{})
	}
	if _, dup := b.turnWriteSeen[id]; dup {
		return
	}
	b.turnWriteSeen[id] = struct{}{}

	if msg.ParentToolUseID != nil {
		b.turnWriteSub.add(usage)
		return
	}
	b.turnWriteTop.add(usage)
}

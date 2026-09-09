package ccstream

import "sort"

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

// addSplit returns s with o's counts added, class by class.
func (s cacheWriteSplit) addSplit(o cacheWriteSplit) cacheWriteSplit {
	return cacheWriteSplit{
		Ephemeral5m: s.Ephemeral5m + o.Ephemeral5m,
		Ephemeral1h: s.Ephemeral1h + o.Ephemeral1h,
		Unknown:     s.Unknown + o.Unknown,
	}
}

// turnUsage is one turn's usage for ONE model, split along the two dimensions
// pricing needs: token class, and — for cache writes — TTL.
type turnUsage struct {
	Input     int
	Output    int
	CacheRead int
	Write     cacheWriteSplit
}

// usageAccumulator holds a turn's usage bucketed by model and by whether a
// subagent produced it.
//
// Bucketed by MODEL because a turn routinely spans several: measured over every
// subagent transcript on disk, 73% ran a different model from their parent, and
// the delegate skill prescribes that (opus parent, sonnet/haiku children). A
// single-model accumulator would be wrong more often than right, and CC's
// result-level ModelUsage — keyed by one model — is exactly the shape that
// caused the spend of those subagents to be dropped entirely (#1872).
//
// Bucketed by SUBAGENT because a foreground subagent's usage arrives from a
// different source than the parent's (its transcript, not the parent stream),
// and knowing which bucket a shortfall belongs to is what makes the source
// completeness testable rather than merely plausible.
//
// ONE struct rather than parallel per-dimension fields, so the turn boundary
// has one thing to clear. The predecessor of this type was two fields reset in
// one place and read in another; splitting reset state across sites is how half
// of it gets forgotten (#1848).
type usageAccumulator struct {
	top  map[string]*turnUsage
	sub  map[string]*turnUsage
	seen map[string]struct{}
}

// note folds one API call's usage into the accumulator, ignoring a message
// already counted.
//
// Dedupe is by message id and is load-bearing twice over. CC emits one
// assistant line PER CONTENT BLOCK, each repeating the whole message's usage,
// so a three-block message arrives three times with identical counts. And a
// FOREGROUND subagent's messages can arrive by BOTH routes: the tail runs only
// for foreground subagents (subagent_tail.go maybeStart gates on expectFg),
// while the parent stream still carries the SOME of that same subagent's
// messages — the probe saw 2 of its 3 reach the stream, and all 3 are in the
// transcript. Background subagents get no tail at all, so they arrive by the
// stream alone.
func (a *usageAccumulator) note(model string, isSub bool, id string, u TokenUsage) {
	// An empty id cannot be deduped, so it is dropped rather than risking a
	// multiple. That makes totals read LOW against ModelUsage, which the
	// divergence check reports — preferable to a silent over-count.
	if id == "" {
		return
	}
	if a.seen == nil {
		a.seen = make(map[string]struct{})
	}
	if _, dup := a.seen[id]; dup {
		return
	}
	a.seen[id] = struct{}{}

	bucket := &a.top
	if isSub {
		bucket = &a.sub
	}
	if *bucket == nil {
		*bucket = make(map[string]*turnUsage)
	}
	tu := (*bucket)[model]
	if tu == nil {
		tu = &turnUsage{}
		(*bucket)[model] = tu
	}
	tu.Input += u.InputTokens
	tu.Output += u.OutputTokens
	tu.CacheRead += u.CacheReadInputTokens
	tu.Write.add(u)
}

// writeSplit totals cache-write tokens across every model in one bucket.
func (a *usageAccumulator) writeSplit(sub bool) cacheWriteSplit {
	m := a.top
	if sub {
		m = a.sub
	}
	var out cacheWriteSplit
	for _, tu := range m {
		out = out.addSplit(tu.Write)
	}
	return out
}

// models lists every model seen this turn, in either bucket. The count being
// greater than one is the condition under which pricing from
// ModelUsage[resultModel] silently loses money.
func (a *usageAccumulator) models() []string {
	seen := make(map[string]struct{}, len(a.top)+len(a.sub))
	for _, m := range []map[string]*turnUsage{a.top, a.sub} {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reset clears the whole turn-scoped group at once.
func (a *usageAccumulator) reset() { *a = usageAccumulator{} }

// noteAssistantUsage records one assistant message from the PARENT STREAM.
//
// Called for every assistant message including subagents' — the partition
// happens here rather than at the call site, so a caller that filters subagents
// out for its own reasons cannot silently drop them from the accounting too.
func (b *Backend) noteAssistantUsage(msg *AssistantMessage) {
	if msg == nil {
		return
	}
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnUsageAcc.note(msg.Message.Model, msg.ParentToolUseID != nil, msg.Message.ID, msg.Message.Usage)
}

// noteSubagentTranscriptUsage records one assistant message read from a
// subagent's own TRANSCRIPT file.
//
// This is the completeness half of the source. A FOREGROUND subagent's
// pure-text messages are suppressed from the parent stream, and their USAGE
// goes with them — measured at 237 tokens missing from a 36,586-token turn,
// invisible from the stream alone. The transcript has every message, and the
// tail that reads it already runs for exactly this case; it simply discarded
// the usage.
//
// Always the subagent bucket: this file only exists for a subagent.
func (b *Backend) noteSubagentTranscriptUsage(model, id string, u TokenUsage) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnUsageAcc.note(model, true, id, u)
}

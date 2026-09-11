package ccstream

import (
	"sort"

	"foci/internal/modelinfo"
)

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

// raise lifts each class to the larger of its current value and what this
// delivery reports, mirroring add's allocation rules.
//
// Separate from add because add ACCUMULATES across different messages while
// this reconciles repeated deliveries of the SAME message. Folding them into
// one function would make the difference invisible at the call site, and that
// difference is exactly what the output-token bug turned on.
func (s *cacheWriteSplit) raise(u TokenUsage) {
	var in cacheWriteSplit
	in.add(u)
	s.Ephemeral5m = maxInt(s.Ephemeral5m, in.Ephemeral5m)
	s.Ephemeral1h = maxInt(s.Ephemeral1h, in.Ephemeral1h)
	s.Unknown = maxInt(s.Unknown, in.Unknown)
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
//
// CUMULATIVE, NOT PER-TURN (#1880 phase B). The maps accumulate for the life of
// the Backend and are never wiped at a turn boundary; a turn's figure is the
// DIFFERENCE from a baseline. The predecessor wiped at `beginTurnLocked` — turn
// START — while pricing measures from the PREVIOUS RESULT, so everything in the
// priced window that arrived before the turn opened was charged for and never
// accumulated. Measured coverage was 1-15% of the cache-write tokens each turn
// was priced on, and the tokens it missed were exactly the mispriced ones.
//
// Diffing rather than wiping also makes the failure mode strictly better: a
// message can now only be attributed to the WRONG turn, never dropped from
// every turn. Losing tokens is a pricing error; misfiling them is an
// attribution error (#1880 phase C), and only one of those costs money.
// subKey identifies one subagent's usage on one model. Keyed by BOTH because a
// subagent is the unit spend should be ATTRIBUTED to (#1880 phase C) while the
// model is the unit it is PRICED at — and one subagent can touch more than one
// model if it spawns its own.
//
// Agent is the Agent tool_use id, which is also what names the transcript file,
// so a row written from this key can be traced back to the work that incurred
// it. It is empty only for a subagent message that arrived before its
// task_started named it; those aggregate under "" rather than being dropped.
type subKey struct {
	Agent string
	Model string
}

type usageAccumulator struct {
	top map[string]*turnUsage
	sub map[subKey]*turnUsage
	// applied is what has already been folded into the buckets for each
	// message id — the high-water mark per class, so a later delivery of the
	// same message contributes only its INCREASE. Replaces a plain seen-set:
	// a set could only ignore a repeat, and the repeat is where the real
	// output token count arrives.
	applied map[string]turnUsage

	// atLastResult is the running total as of the most recent RESULT message —
	// the same boundary modelUsageDelta snapshots at. Marked by markResult.
	atLastResult usageTotals
	// atTurnStart is atLastResult's value when the current turn opened, i.e.
	// the total as of the last result BEFORE this turn. Every per-turn figure
	// is measured from here. Deliberately NOT the total at turn start itself:
	// that is the boundary whose mismatch caused the bug.
	atTurnStart usageTotals
}

// usageTotals is a point-in-time copy of an accumulator's per-model totals,
// held BY VALUE so a later note() cannot mutate a baseline already taken.
type usageTotals struct {
	top map[string]turnUsage
	sub map[subKey]turnUsage
}

// snapshot copies the current totals by value.
func (a *usageAccumulator) snapshot() usageTotals {
	top := make(map[string]turnUsage, len(a.top))
	for k, v := range a.top {
		top[k] = *v
	}
	sub := make(map[subKey]turnUsage, len(a.sub))
	for k, v := range a.sub {
		sub[k] = *v
	}
	return usageTotals{top: top, sub: sub}
}

// markResult records the totals as of a result message. Called at the same
// point modelUsageDelta takes its snapshot, so the two describe one window.
func (a *usageAccumulator) markResult() { a.atLastResult = a.snapshot() }

// beginTurn moves the per-turn baseline to the last result's totals.
//
// It does NOT snapshot the current totals: messages that arrived between the
// last result and this turn opening are inside the window that will be priced,
// so the baseline has to sit before them, not after.
func (a *usageAccumulator) beginTurn() { a.atTurnStart = a.atLastResult }

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
func (a *usageAccumulator) note(model, agent string, isSub bool, id string, u TokenUsage) {
	// An empty id cannot be reconciled against an earlier delivery of the same
	// message, so it is dropped rather than risking a multiple. That makes
	// totals read LOW against ModelUsage, which the divergence check reports —
	// preferable to a silent over-count. Measured 2026-09-10 across 5.5M
	// assistant records on this host: zero had an empty id, so this is a guard,
	// not a live loss.
	if id == "" {
		return
	}
	if a.applied == nil {
		a.applied = make(map[string]turnUsage)
	}

	// HIGH-WATER MARK PER CLASS, not first-wins.
	//
	// CC delivers one line per content block and each repeats the message's
	// usage — but only THREE of the four classes are final on the first line.
	// Output is a running count that starts at 1-3 and is only completed on the
	// line carrying a non-nil stop_reason. Measured on a subagent transcript
	// 2026-09-10: across 29 message ids, input / cache_read / cache_write
	// varied on ZERO of them and output varied on 26. First-wins therefore
	// locked in the placeholder and discarded the real figure — 88.6% of
	// subagent output tokens on this host, ~894,000 opus tokens, and output is
	// the most expensive class ($25/MTok on opus-5 against $5 input).
	//
	// Taking the max per class is a no-op for the three stable classes and
	// repairs the fourth, without depending on stop_reason being present or on
	// the lines arriving in order. Only the INCREASE is added to the bucket, so
	// a repeated delivery still cannot inflate a total — which is what the
	// previous first-wins rule existed to prevent.
	prev := a.applied[id]
	cur := turnUsage{
		Input:     maxInt(prev.Input, u.InputTokens),
		Output:    maxInt(prev.Output, u.OutputTokens),
		CacheRead: maxInt(prev.CacheRead, u.CacheReadInputTokens),
	}
	cur.Write = prev.Write
	cur.Write.raise(u)
	if cur == prev {
		return
	}
	a.applied[id] = cur

	var tu *turnUsage
	if isSub {
		if a.sub == nil {
			a.sub = make(map[subKey]*turnUsage)
		}
		k := subKey{Agent: agent, Model: model}
		if tu = a.sub[k]; tu == nil {
			tu = &turnUsage{}
			a.sub[k] = tu
		}
	} else {
		if a.top == nil {
			a.top = make(map[string]*turnUsage)
		}
		if tu = a.top[model]; tu == nil {
			tu = &turnUsage{}
			a.top[model] = tu
		}
	}
	tu.Input += cur.Input - prev.Input
	tu.Output += cur.Output - prev.Output
	tu.CacheRead += cur.CacheRead - prev.CacheRead
	tu.Write.Ephemeral5m += cur.Write.Ephemeral5m - prev.Write.Ephemeral5m
	tu.Write.Ephemeral1h += cur.Write.Ephemeral1h - prev.Write.Ephemeral1h
	tu.Write.Unknown += cur.Write.Unknown - prev.Write.Unknown
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// writeSplit totals cache-write tokens across every model in one bucket, for
// THIS TURN — the running total minus the baseline taken at the last result
// before the turn opened.
//
// Returning the running total instead would print a per-SESSION breakdown
// beside a per-TURN cost, which is the shape of #1848 and reads as a wildly
// mispriced turn.
func (a *usageAccumulator) writeSplit(sub bool) cacheWriteSplit {
	var out cacheWriteSplit
	if sub {
		for k, tu := range a.sub {
			out = out.addSplit(deltaWrite(tu.Write, a.atTurnStart.sub[k].Write))
		}
		return out
	}
	for model, tu := range a.top {
		out = out.addSplit(deltaWrite(tu.Write, a.atTurnStart.top[model].Write))
	}
	return out
}

// splitFor allocates `total` authoritative cache-write tokens for one model
// across the TTL classes, using what the accumulator actually observed.
//
// THIS IS NOT A RATIO. When the accumulator saw exactly `total` tokens, its
// split IS the answer and is returned verbatim — measured 100.00% on a
// production turn (369,913 = 369,913) and on a live probe (30,921 = 30,921),
// so the exact path is the normal path.
//
// When the counts DISAGREE, the honest answer is that the TTL of the
// difference was not observed, so the difference lands in Unknown — which
// prices at the 1h rate, i.e. the pre-#1866 behaviour, erring toward
// over-charging. Scaling the observed split proportionally onto the
// authoritative total would manufacture a number no one measured; that is the
// fudge this function exists to refuse.
//
// `total` always wins: the returned classes sum to it exactly, so pricing still
// reconciles against ModelUsage whatever the coverage.
func splitFor(observed cacheWriteSplit, total int) modelinfo.CacheWrites {
	if total <= 0 {
		return modelinfo.CacheWrites{}
	}
	w := modelinfo.CacheWrites{
		Ephemeral5m: observed.Ephemeral5m,
		Ephemeral1h: observed.Ephemeral1h,
		Unknown:     observed.Unknown,
	}
	// Never allocate more than the authoritative total. An accumulator reading
	// HIGH means a message was counted that ModelUsage does not include; drop
	// from the cheaper class first so the residue cannot under-charge.
	for w.Ephemeral5m+w.Ephemeral1h+w.Unknown > total {
		switch {
		case w.Ephemeral5m > 0:
			w.Ephemeral5m--
		case w.Unknown > 0:
			w.Unknown--
		default:
			w.Ephemeral1h--
		}
	}
	w.Unknown += total - (w.Ephemeral5m + w.Ephemeral1h + w.Unknown)
	return w
}

// models lists every model seen this turn, in either bucket. The count being
// greater than one is the condition under which pricing from
// ModelUsage[resultModel] silently loses money.
func (a *usageAccumulator) models() []string {
	seen := make(map[string]struct{}, len(a.top)+len(a.sub))
	// Only models that did work THIS TURN. The maps are cumulative for the
	// Backend's life (#1880 phase B), so listing every key would name every
	// model the session ever touched and make the multi-model warning fire
	// permanently after the first cross-model turn.
	for model, tu := range a.top {
		if *tu != a.atTurnStart.top[model] {
			seen[model] = struct{}{}
		}
	}
	for k, tu := range a.sub {
		if *tu != a.atTurnStart.sub[k] {
			seen[k.Model] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reset clears everything, including the cumulative totals. Session-scoped, not
// turn-scoped: a turn boundary moves the baseline (beginTurn) and must never
// wipe the totals, because the window being priced starts before the turn does.
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
	// ParentToolUseID is BOTH the subagent flag and the subagent's identity —
	// it is the Agent tool_use id that names the transcript file.
	var agent string
	if msg.ParentToolUseID != nil {
		agent = *msg.ParentToolUseID
	}
	b.turnUsageAcc.note(msg.Message.Model, agent, msg.ParentToolUseID != nil, msg.Message.ID, msg.Message.Usage)
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
func (b *Backend) noteSubagentTranscriptUsage(agent, model, id string, u TokenUsage) {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	b.turnUsageAcc.note(model, agent, true, id, u)
}

// deltaWrite is cur minus base, class by class. The zero value of turnUsage is
// a valid base, so a key absent from the baseline correctly yields cur.
func deltaWrite(cur, base cacheWriteSplit) cacheWriteSplit {
	return cacheWriteSplit{
		Ephemeral5m: cur.Ephemeral5m - base.Ephemeral5m,
		Ephemeral1h: cur.Ephemeral1h - base.Ephemeral1h,
		Unknown:     cur.Unknown - base.Unknown,
	}
}

// subagentUsage returns this turn's usage per SUBAGENT, summed across whatever
// models each one used. This is the attribution #1880 phase C needs: the map
// key is the Agent tool_use id that also names the transcript, so spend can be
// traced to the work that incurred it rather than to whichever turn happened to
// close while it was running.
func (a *usageAccumulator) subagentUsage() map[string]turnUsage {
	out := make(map[string]turnUsage, len(a.sub))
	for k, d := range a.subagentDelta() {
		e := out[k.Agent]
		e.Input += d.Input
		e.Output += d.Output
		e.CacheRead += d.CacheRead
		e.Write = e.Write.addSplit(d.Write)
		out[k.Agent] = e
	}
	return out
}

// subagentDelta returns this turn's usage per (agent, model) — the accumulator's
// own key, kept whole rather than summed across models the way subagentUsage
// does.
//
// Both dimensions are needed to WRITE a row: the agent is what the row is
// attributed to and the model is what it is priced at, and a row carries one
// model in its model column. Entries whose delta is entirely zero are dropped,
// because the maps are cumulative for the Backend's life (phase B) and every
// subagent the session ever ran is a key — emitting those would write a
// zero-cost row per historical subagent on every turn.
func (a *usageAccumulator) subagentDelta() map[subKey]turnUsage {
	out := make(map[subKey]turnUsage, len(a.sub))
	for k, tu := range a.sub {
		b := a.atTurnStart.sub[k]
		if *tu == b {
			continue
		}
		out[k] = turnUsage{
			Input:     tu.Input - b.Input,
			Output:    tu.Output - b.Output,
			CacheRead: tu.CacheRead - b.CacheRead,
			Write:     deltaWrite(tu.Write, b.Write),
		}
	}
	return out
}

// topWriteSplitByModel returns this turn's cache-write TTL split for each model,
// MAIN THREAD ONLY — the subagents' writes are priced from their own entries in
// subagentDelta.
//
// Per model because pricing is per model: a turn routinely spans several (73% of
// subagents run a different model from their parent), and each has its own 5m
// and 1h rates.
//
// Split by bucket only since #1880 phase C, and only because the buckets are now
// priced onto SEPARATE ROWS. This is NOT the inference that caused #1866. The
// TTL is still read from what each bucket actually OBSERVED — a subagent whose
// messages report 1h writes is charged at 1h — and nothing here derives a TTL
// from "it was a subagent, so it must be 5m". The mapping happens to be clean
// today (main thread 1h, subagents 5m) and is still never assumed;
// TestOnResult_ParentKeepsItsOwnTTLWhenTheSubagentSharesItsModel runs it the
// other way round on purpose.
//
// The combined figure cannot be used for the parent: with the subagents' share
// already subtracted off the total, splitFor would allocate the parent's
// remaining tokens out of a split that still includes the subagents' — measured
// at $0.525 against a true $0.375 on the test turn, a 40% overcharge on exactly
// the tokens #1866 was about.
func (a *usageAccumulator) topWriteSplitByModel() map[string]cacheWriteSplit {
	out := make(map[string]cacheWriteSplit, len(a.top))
	for model, tu := range a.top {
		out[model] = out[model].addSplit(deltaWrite(tu.Write, a.atTurnStart.top[model].Write))
	}
	return out
}

// sortedSubagentCosts flattens the per-(agent, model) map into a stable slice.
//
// Sorted because the order becomes the order of api_calls rows, and an
// unordered map would make two identical turns produce different row sequences
// — which shows up as spurious diffs in any test or report that compares them.
func sortedSubagentCosts(m map[subKey]modelinfo.SubagentCost) []modelinfo.SubagentCost {
	if len(m) == 0 {
		return nil
	}
	keys := make([]subKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Agent != keys[j].Agent {
			return keys[i].Agent < keys[j].Agent
		}
		return keys[i].Model < keys[j].Model
	})
	out := make([]modelinfo.SubagentCost, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

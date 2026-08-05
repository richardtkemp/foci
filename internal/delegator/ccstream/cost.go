package ccstream

// Cost and token accounting for CC turns (#1674).
//
// The one fact everything here exists for: EVERY counter in CC's per-result
// ModelUsage map is CUMULATIVE over the life of the CC PROCESS — cost, output
// tokens, cache reads, cache writes. Storing any of them as a per-turn figure
// over-counts by roughly the turn index, which compounds to ~quadratic when
// rows are summed (13x measured across 28 Jul - 4 Aug 2026: $32,566 reported
// against ~$2,500 real).
//
// Probe-verified 2026-08-05, one CC process fed four turns over
// --input-format stream-json, four identical trivial prompts:
//
//	turn | costUSD  | outputTokens | cacheReadInputTokens
//	  1  | 0.0099234|      56      |   21,624
//	  2  | 0.0141382|     105      |   46,722
//	  3  | 0.0170425|     141      |   72,545
//	  4  | 0.0199124|     174      |   98,434
//
// cacheRead climbing in near-equal ~25k steps for a trivial turn is the tell:
// a running sum, not a growing context.
//
// NOTE FOR ANYONE RE-VERIFYING THIS: `claude -p --resume` spawns a FRESH
// PROCESS PER TURN, so a per-process counter cannot accumulate and the probe
// cannot distinguish cumulative from per-turn. It must be ONE process fed
// multiple turns.

// modelUsageDelta converts CC's cumulative ModelUsage snapshot into this
// cycle's own figures by subtracting the previous snapshot for the same model,
// then records the new snapshot. Caller must hold b.mu.
//
// The previous snapshot lives on the Backend precisely because a Backend's
// lifetime IS the CC process's, which IS the counters' reset boundary. A
// resumed session in a NEW process restarts them at zero despite an unchanged
// session id (probe-verified 2026-08-05: 0.0243 -> 0.0035 across a --resume),
// so keying this on the session or its transcript file would miss the reset
// entirely and mint a huge negative delta.
//
// Each field is guarded independently rather than trusting one to speak for
// the rest: a counter that has gone DOWN means the process restarted beneath a
// reused Backend, and the current value is then the whole delta. Guarding only
// (say) cost would let a mixed snapshot yield a negative token count.
func (b *Backend) modelUsageDelta(model string, cur ModelUsage) ModelUsage {
	prev, seen := b.lastModelUsage[model]
	if b.lastModelUsage == nil {
		b.lastModelUsage = make(map[string]ModelUsage)
	}
	b.lastModelUsage[model] = cur
	if !seen {
		return cur
	}

	sub := func(now, before int) int {
		if now < before {
			return now
		}
		return now - before
	}

	d := cur
	d.InputTokens = sub(cur.InputTokens, prev.InputTokens)
	d.OutputTokens = sub(cur.OutputTokens, prev.OutputTokens)
	d.CacheReadInputTokens = sub(cur.CacheReadInputTokens, prev.CacheReadInputTokens)
	d.CacheCreationInputTokens = sub(cur.CacheCreationInputTokens, prev.CacheCreationInputTokens)
	if cur.CostUSD < prev.CostUSD {
		d.CostUSD = cur.CostUSD
	} else {
		d.CostUSD = cur.CostUSD - prev.CostUSD
	}
	return d
}

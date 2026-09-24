package ccstream

import (
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// Live checks on the Claude Code behaviours foci's cost accounting rests on
// (#2013). See delegator/expectations.go for delivery and rate limiting; this
// file holds only the CC-specific predicates and the measurements they need.

// expectBackend names this backend in violation reports and version lines.
const expectBackend = "claude-code"

// Invariant names. They are the rate-limit keys, so keep them stable.
const (
	// A CC process's first ModelUsage, minus the baseline Start seeded for it,
	// covers exactly that process's work. modelUsageDelta prices the first
	// result from that baseline, which #2012's fix reads from the last
	// cost-state record in the transcript on the assumption that CC restores
	// exactly that record on --resume (and nothing on a fresh session). CC
	// 2.1.280 began restoring; an empty baseline then booked the whole
	// conversation to one turn. If CC stops restoring, or restores something
	// other than the record foci read, the first turn is again mispriced.
	invFreshProcessUsage = "first ModelUsage minus the seeded resume baseline covers only this process"
	// ModelUsage counters never go DOWN within one CC process (#1674). A drop
	// means CC is no longer reporting a per-process running sum, and every
	// delta modelUsageDelta takes is then wrong.
	invModelUsageMonotonic = "ModelUsage is cumulative within a process"
	// A per-message usage with cache writes carries the per-TTL split. Without
	// it splitFor prices every write at the 1h rate (#1866).
	invCacheWriteSplit = "per-message usage carries the cache-write TTL split"
)

// The fresh-process check allows for what legitimately differs between
// ModelUsage and the stream: CC's internal utility calls (916 input tokens on
// one probe turn) and a subagent line whose delivery lagged the result. Both
// are small against a restore gone wrong, which is off by the conversation's
// history: even one restored turn re-reads the system prompt, ~13k tokens on
// the probe, and the production cases ran to 280M.
const (
	freshUsageSlackTokens   = 5000
	freshUsageSlackFraction = 0.25
)

// expectations returns the guard this Backend reports to.
func (b *Backend) expectations() *delegator.ExpectationGuard {
	if b.expect != nil {
		return b.expect
	}
	return delegator.Expectations
}

// backendVersion is the CC version this process reported at init, or "".
func (b *Backend) backendVersion() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initMsg == nil {
		return ""
	}
	return b.initMsg.ClaudeCodeVersion
}

func (b *Backend) violated(invariant, detail string) {
	b.expectations().Violated(b.logger(), expectBackend, b.backendVersion(), invariant, detail)
}

// cacheTokens is the input-side cache traffic a usage record carries. Cache
// reads and writes are what a restored conversation shifts; plain input is left
// out because CC's utility calls land there and on no stream line.
func cacheTokens(read, write int) int { return read + write }

// modelUsageCache sums cacheTokens over every model in a ModelUsage map.
func modelUsageCache(mu map[string]ModelUsage) int {
	n := 0
	for _, u := range mu {
		n += cacheTokens(u.CacheReadInputTokens, u.CacheCreationInputTokens)
	}
	return n
}

// checkFreshProcessUsage runs on the FIRST result of a CC process that carries
// ModelUsage, the one result modelUsageDelta measures from the baseline Start
// seeded rather than from a snapshot this process reported. baseCache is that
// baseline's cache traffic (0 for a fresh session).
//
// The delta (ModelUsage minus the baseline, summed over every model) must match
// what this process was seen doing: the stream accumulator's totals since it
// started (main thread plus every completed subagent message), or result.usage
// for the main thread if that is larger. Probe-verified 2026-09-10 that the
// stream matches ModelUsage exactly for cache reads and writes.
//
// It fires in both directions:
//   - the delta EXCEEDS the seen work: CC restored more than the record foci
//     read (or restores where foci expected a fresh start). This is #2012
//     itself if the baseline is missing: the probe's resumed turn reported
//     45,769 against 23,003 seen.
//   - the delta FALLS SHORT of the seen work, down to negative: CC restored
//     less than the record foci read, or stopped restoring at all. The
//     per-field clamp in modelUsageDelta would then hide a negative delta.
//
// Summed across models rather than per model, because the question is whether
// the baseline matches what CC restored, not how CC splits a turn by model.
func (b *Backend) checkFreshProcessUsage(msg *ResultMessage, baseCache int, observed usageTotals) {
	reported := modelUsageCache(msg.ModelUsage)
	top, sub := 0, 0
	for _, u := range observed.top {
		top += cacheTokens(u.CacheRead, u.Write.total())
	}
	for _, u := range observed.sub {
		sub += cacheTokens(u.CacheRead, u.Write.total())
	}
	main := max(top, cacheTokens(msg.Usage.CacheReadInputTokens, msg.Usage.CacheCreationInputTokens))
	seen := main + sub
	delta := reported - baseCache
	gap := delta - seen
	mag := gap
	if mag < 0 {
		mag = -mag
	}
	if mag <= freshUsageSlackTokens || float64(mag) <= freshUsageSlackFraction*float64(seen) {
		return
	}
	var what string
	if gap > 0 {
		what = fmt.Sprintf("%d MORE than was seen: CC restored more than the baseline foci read, so this turn is overcharged by roughly that much", gap)
	} else {
		what = fmt.Sprintf("%d LESS than was seen: CC restored less than the baseline foci read (or stopped restoring), so this turn's delta is wrong", -gap)
	}
	b.violated(invFreshProcessUsage, fmt.Sprintf(
		"first result of this process reports %d cache tokens in ModelUsage against a seeded baseline of %d, "+
			"a delta of %d; the process was seen doing %d (main thread %d, subagents %d). That is %s (session %s)",
		reported, baseCache, delta, seen, main, sub, what, msg.SessionID))
}

// modelUsageRegression names every ModelUsage counter that went DOWN from prev
// to cur, or returns "" when none did.
func modelUsageRegression(prev, cur ModelUsage) string {
	var out []string
	add := func(name string, before, now int) {
		if now < before {
			out = append(out, fmt.Sprintf("%s %d->%d", name, before, now))
		}
	}
	add("input", prev.InputTokens, cur.InputTokens)
	add("output", prev.OutputTokens, cur.OutputTokens)
	add("cache_read", prev.CacheReadInputTokens, cur.CacheReadInputTokens)
	add("cache_write", prev.CacheCreationInputTokens, cur.CacheCreationInputTokens)
	add("web_search", prev.WebSearchRequests, cur.WebSearchRequests)
	if cur.CostUSD < prev.CostUSD {
		out = append(out, fmt.Sprintf("cost %.6f->%.6f", prev.CostUSD, cur.CostUSD))
	}
	return strings.Join(out, ", ")
}

// checkCacheWriteSplit flags a main-thread message that reports cache writes
// without saying at which TTL. Measured 2026-09-24 over 23,414 transcript
// messages with cache writes: every one carried the split.
func (b *Backend) checkCacheWriteSplit(msg *AssistantMessage) {
	u := msg.Message.Usage
	if u.CacheCreationInputTokens <= 0 || u.CacheCreation != nil {
		return
	}
	b.violated(invCacheWriteSplit, fmt.Sprintf(
		"message %s (%s) reports %d cache-write tokens with no cache_creation breakdown; "+
			"they will be priced at the 1h rate", msg.Message.ID, msg.Message.Model, u.CacheCreationInputTokens))
}

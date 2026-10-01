package ccstream

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"foci/internal/delegator"
)

// Live checks on the Claude Code behaviours foci's cost accounting rests on
// (#2013). See delegator/expectations.go for delivery and rate limiting; this
// file holds only the CC-specific predicates. The ledger adapter (ccbook.go)
// raises its own alarms on what it books; these watch CC's inputs to it.

// expectBackend names this backend in violation reports and version lines.
const expectBackend = "claude-code"

// Invariant names. They are the rate-limit keys, so keep them stable.
const (
	// ModelUsage counters never go DOWN within one CC process (#1674). The
	// adapter's remainder is modelUsage less the process's baseline and its
	// booked calls; a drop means CC is no longer reporting a per-process
	// running sum, and the remainder is then wrong.
	invModelUsageMonotonic = "ModelUsage is cumulative within a process"
	// A per-message usage with cache writes carries the per-TTL split.
	// Without it the call's writes are TTL-unknown, priced at the 1h rate
	// (#1866).
	invCacheWriteSplit = "per-message usage carries the cache-write TTL split"
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

// checkModelUsageMonotonic compares a result's modelUsage with this process's
// previous one, and reports every counter that went down. The first result of
// a process has nothing to compare with: what CC restored on --resume is the
// adapter's baseline, not a result of this process.
func (b *Backend) checkModelUsageMonotonic(mu map[string]ModelUsage) {
	if len(mu) == 0 {
		return
	}
	b.mu.Lock()
	prev := b.prevModelUsage
	b.prevModelUsage = maps.Clone(mu)
	b.mu.Unlock()
	var regressions []string
	for _, m := range slices.Sorted(maps.Keys(mu)) {
		if p, seen := prev[m]; seen {
			if r := modelUsageRegression(p, mu[m]); r != "" {
				regressions = append(regressions, m+": "+r)
			}
		}
	}
	if len(regressions) > 0 {
		b.violated(invModelUsageMonotonic, fmt.Sprintf(
			"counters went DOWN within one CC process (%s); the ledger's remainder for this process is now unreliable",
			strings.Join(regressions, "; ")))
	}
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

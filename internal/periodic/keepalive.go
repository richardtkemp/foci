package periodic

import (
	"context"
	"time"

	"foci/shared/prompts"
)

func (r *Runner) maybeKeepalive(ctx context.Context) { // nolint:unparam
	if !r.kaCfg.Enabled || r.agent == nil {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip keepalive: %s", skip)
		}
	}()

	// Check if caching is still available.
	// cachingOverride allows models with auto-detected caching (OpenAI, DeepSeek) to
	// bypass the client.IsCachingAvailable() check which may return false for non-Anthropic clients.
	cachingAvailable := true
	if r.cachingOverride != nil {
		cachingAvailable = *r.cachingOverride
	} else if r.client != nil {
		cachingAvailable = r.client.IsCachingAvailable()
	}
	if !cachingAvailable {
		skip = "caching not available"
		return
	}

	interval, ok := r.parseDuration("keepalive interval", r.kaCfg.Interval)
	if !ok {
		return
	}

	r.mu.Lock()
	running := r.keepaliveRunning
	reflectionRunning := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	resetRunning := r.resetRunning
	quietCompactionRunning := r.quietCompactionRunning
	r.mu.Unlock()

	if running {
		skip = "already running"
		return
	}
	// Defer to the memory-forming passes — the same three that are already
	// mutually exclusive with each other. Reflection and consolidation branch
	// off the same parent session keepalive would, and the branch key carries
	// only a one-second timestamp (session.withChild) — so two branches off one
	// parent inside the same second collide on the key and one has to retry.
	// Reset is included for a second reason as well as that one: it forms
	// memory and then ROTATES the session key, so a keepalive racing it warms
	// a cache that is about to be discarded (ClearSessionState nulls
	// last_cache_touch) — wasted at best.
	//
	// The run loop runs all three BEFORE keepalive precisely so this check sees
	// their flags on the same tick; do not reorder without reading the comment
	// there. Yielding costs a single tick of cache warmth and never a cache
	// expiry (the keepalive window's upper bound is the cache TTL, which is
	// orders of magnitude longer than a tick), and it is the right way round:
	// these are real work with deadlines of their own, keepalive is
	// maintenance that is equally happy 30 seconds later.
	if reflectionRunning || consolidationRunning || resetRunning {
		skip = "memory task running"
		return
	}
	// Defer to a quiet-hours compaction (#2218) the same way: it is about to
	// shrink the very context keepalive would re-warm, so warming first pays
	// the full-context cache write the compaction exists to make cheap. The
	// next tick after the compaction warms the small post-compaction prefix.
	if quietCompactionRunning {
		skip = "quiet compaction running"
		return
	}

	targets, why := r.keepaliveTargets(interval)
	if len(targets) == 0 {
		skip = why
		return
	}

	promptText := prompts.ResolvePrompt(r.kaCfg.Prompt, "keepalive.md", prompts.Keepalive(), r.promptSearchDirs...)

	r.mu.Lock()
	r.keepaliveRunning = true
	r.mu.Unlock()

	r.log.Infof("firing keepalive for agent %s (%d session(s))", r.agentID, len(targets))

	go func() {
		defer func() {
			r.mu.Lock()
			r.keepaliveRunning = false
			r.mu.Unlock()
		}()
		for _, sk := range targets {
			r.agent.Branch("keepalive", sk, promptText, true)
		}
	}()
}

// keepaliveTargets returns the sessions to warm this cycle. Candidates are the
// open app chats (with warm_open_app_chats) or else the default session; each is
// kept only if it's in the warm WINDOW: its cache was touched at least `interval`
// ago (due for a refresh) but less than cacheTTL ago (not yet expired). A
// session with no recorded cache-touch — never warmed, or just reset — is skipped:
// there is no live cache to keep alive. An in-flight turn also skips it. A session
// no human has touched within max_user_idle is skipped too — an abandoned session
// is left to expire rather than warmed indefinitely.
//
// When nothing is ready, why says so. The in-flight check runs LAST, so a
// session reported as "due but turn in flight" is one that would have been
// warmed this tick had the in-flight gate not deferred it — the event the L2
// gate test (TestL2_Cron_KeepaliveSkippedWhenTurnInFlight) syncs on (#2084).
func (r *Runner) keepaliveTargets(interval time.Duration) (ready []string, why string) {
	why = "no session in the warm window"
	var candidates []string
	if r.kaCfg.WarmOpenAppChats && r.openSessionsFn != nil {
		candidates = r.openSessionsFn()
	}
	if len(candidates) == 0 {
		if parentKey := r.defaultParentKey(); parentKey != "" {
			candidates = []string{parentKey}
		}
	}
	if r.sessionIndex == nil {
		return nil, why
	}

	var maxIdle time.Duration
	if r.kaCfg.MaxUserIdle != "" {
		if d, ok := r.parseDuration("keepalive max_user_idle", r.kaCfg.MaxUserIdle); ok {
			maxIdle = d
		}
	}

	now := time.Now()
	for _, sk := range candidates {
		if skip := r.checkRateLimit(sk); skip != "" {
			continue // endpoint rate-limited — don't warm into a cap
		}
		if maxIdle > 0 {
			if ua, ok := r.sessionIndex.LastUserActivity(sk); !ok || now.Sub(ua) > maxIdle {
				continue // no human touched this session within max_user_idle — let it expire
			}
		}
		touchAge, ok := r.lastCacheTouchAge(sk, now)
		if !ok {
			continue // never warmed / reset — no live cache to keep alive
		}
		if touchAge < interval {
			continue // warmed recently — not due yet
		}
		if r.cacheTTL > 0 && touchAge >= r.cacheTTL {
			continue // cache already expired — don't warm a corpse
		}
		if r.parentTurnInFlight(sk) {
			why = "due but turn in flight on " + sk
			continue
		}
		ready = append(ready, sk)
	}
	return ready, why
}

// lastCacheTouchAge returns how long ago the session's prompt cache was last
// touched, and whether a touch is recorded at all. The ONE read of
// last_cache_touch for cache-warmth decisions: keepalive's warm window and
// quiet compaction's provably-warm gate (#2218) share it, so the two can
// never disagree about what "warm" means.
func (r *Runner) lastCacheTouchAge(sessionKey string, now time.Time) (time.Duration, bool) {
	if r.sessionIndex == nil {
		return 0, false
	}
	touch, ok := r.sessionIndex.LastCacheTouch(sessionKey)
	if !ok {
		return 0, false
	}
	return now.Sub(touch), true
}

// cacheProvablyWarm reports whether the session's prompt cache is provably
// still warm at now: a recorded last_cache_touch younger than the runner's
// cacheTTL. Quiet-hours compaction (#2218) requires this — compacting a cold
// session pays a full-context read for nothing. Fail closed: no recorded
// touch, or an unknown TTL (cacheTTL == 0, warmth unprovable), is NOT warm.
func (r *Runner) cacheProvablyWarm(sessionKey string, now time.Time) bool {
	age, ok := r.lastCacheTouchAge(sessionKey, now)
	return ok && r.cacheTTL > 0 && age < r.cacheTTL
}

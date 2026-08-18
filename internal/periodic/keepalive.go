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

	targets := r.keepaliveTargets(interval)
	if len(targets) == 0 {
		skip = "no session in the warm window"
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
func (r *Runner) keepaliveTargets(interval time.Duration) []string {
	var candidates []string
	if r.kaCfg.WarmOpenAppChats && r.openSessionsFn != nil {
		candidates = r.openSessionsFn()
	}
	if len(candidates) == 0 {
		if parentKey, skip := r.readyParentKey(); skip == "" {
			candidates = []string{parentKey}
		}
	}
	if r.sessionIndex == nil {
		return nil
	}

	var maxIdle time.Duration
	if r.kaCfg.MaxUserIdle != "" {
		if d, ok := r.parseDuration("keepalive max_user_idle", r.kaCfg.MaxUserIdle); ok {
			maxIdle = d
		}
	}

	now := time.Now()
	var ready []string
	for _, sk := range candidates {
		if r.parentTurnInFlight(sk) {
			continue
		}
		if skip := r.checkRateLimit(sk); skip != "" {
			continue // endpoint rate-limited — don't warm into a cap
		}
		if maxIdle > 0 {
			if ua, ok := r.sessionIndex.LastUserActivity(sk); !ok || now.Sub(ua) > maxIdle {
				continue // no human touched this session within max_user_idle — let it expire
			}
		}
		touch, ok := r.sessionIndex.LastCacheTouch(sk)
		if !ok {
			continue // never warmed / reset — no live cache to keep alive
		}
		elapsed := now.Sub(touch)
		if elapsed < interval {
			continue // warmed recently — not due yet
		}
		if r.cacheTTL > 0 && elapsed >= r.cacheTTL {
			continue // cache already expired — don't warm a corpse
		}
		ready = append(ready, sk)
	}
	return ready
}

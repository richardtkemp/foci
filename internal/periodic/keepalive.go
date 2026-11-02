package periodic

import (
	"context"
	"slices"
	"sync"
	"time"

	"foci/internal/config"
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

	if !r.cachingAvailable() {
		skip = "caching not available"
		return
	}

	interval, ok := r.parseDuration("keepalive interval", r.kaCfg.Interval)
	if !ok {
		return
	}

	r.mu.Lock()
	reflectionRunning := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	resetRunning := r.resetRunning
	quietCompactionRunning := r.quietCompactionRunning
	precoldCompactionRunning := r.precoldCompactionRunning
	r.mu.Unlock()

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
	// Likewise for a pre-cold compaction (#1066). It only picks sessions
	// keepalive will not warm, so the two rarely meet; the yield covers a
	// config change or a new open chat between the pick and the dispatch.
	if precoldCompactionRunning {
		skip = "pre-cold compaction running"
		return
	}

	targets, why := r.keepaliveTargets(interval)
	if len(targets) == 0 {
		skip = why
		return
	}

	// The prompt may be a script (#1351): it then runs once per target, just
	// before that target's keepalive, so it can decide per session (e.g. ask
	// for routine work in one session a cycle and a bare ping in the rest).
	src := prompts.ResolveKeepaliveSource(r.kaCfg.Prompt, r.promptSearchDirs...)
	defaultKey := r.defaultParentKey()
	timeout := r.keepaliveTimeout()

	// Each session's keepalive runs on its own (#2487): a slow or stuck one
	// must not delay another session's, whose cache may expire meanwhile.
	// keepaliveTargets already dropped the sessions with a keepalive running,
	// so each session still has at most one at a time.
	r.mu.Lock()
	if r.keepaliveRunning == nil {
		r.keepaliveRunning = map[string]bool{}
	}
	for _, sk := range targets {
		r.keepaliveRunning[sk] = true
	}
	r.mu.Unlock()

	r.log.Infof("firing keepalive for agent %s (%d session(s))", r.agentID, len(targets))

	// One dispatcher starts the sessions' keepalives in target order: each
	// prompt script runs in turn (bounded by its own timeout), and each
	// keepalive starts before the next session's script runs, so the order a
	// script sees (index/count, #1351) is the order the turns start. After it
	// starts, a keepalive runs on its own and nothing waits for it.
	go func() {
		for i, sk := range targets {
			run := keepaliveRun{
				sessionKey: sk,
				index:      i,
				count:      len(targets),
				isDefault:  sk == defaultKey,
			}
			started := make(chan struct{})
			go r.runKeepalive(ctx, src, run, timeout, started)
			<-started
		}
	}()
}

// runKeepalive warms one session and releases its run state. A keepalive
// that has not completed within timeout (#2489) is cancelled: its context
// ends, which stops the backend turn, and the session's run state is
// released at once so the next due tick can warm it again. A healthy
// keepalive is one cache-read turn that ends in seconds; one that hangs (a
// rejected usage limit holding it in a retry loop, 10 Oct: 7h52m) warmed
// nothing, so the cache touch it recorded is put back.
//
// started is closed just before the turn starts.
func (r *Runner) runKeepalive(ctx context.Context, src prompts.Source, run keepaliveRun, timeout time.Duration, started chan<- struct{}) {
	sk := run.sessionKey
	startedOnce := sync.OnceFunc(func() { close(started) })
	defer startedOnce()
	defer func() {
		r.mu.Lock()
		delete(r.keepaliveRunning, sk)
		r.mu.Unlock()
	}()

	promptText := src.Text
	if src.Script != "" {
		promptText = r.keepaliveScriptPrompt(ctx, src.Script, run)
	}

	var touchBefore time.Time
	var hadTouch bool
	if r.sessionIndex != nil {
		touchBefore, hadTouch = r.sessionIndex.LastCacheTouch(sk)
	}
	began := time.Now()

	kctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		startedOnce()
		r.agent.Branch(kctx, "keepalive", sk, promptText, true)
	}()
	select {
	case <-done:
		return
	case <-kctx.Done():
	}
	if ctx.Err() != nil {
		return // the runner is stopping, not a stuck keepalive
	}
	r.log.Warnf("keepalive timed out: agent=%s session=%s reason=not completed within %s; cancelled", r.agentID, sk, timeout)
	r.restoreCacheTouchAfterTimeout(sk, touchBefore, hadTouch, began)
}

// restoreCacheTouchAfterTimeout puts back the session's last_cache_touch a
// timed-out keepalive advanced: its turn reached the backend (which records
// the touch) but never completed, so the cache was not warmed. A real turn
// that ended on the session since the keepalive started did warm it, and
// its touch is kept.
func (r *Runner) restoreCacheTouchAfterTimeout(sk string, before time.Time, hadTouch bool, started time.Time) {
	if r.sessionIndex == nil || !hadTouch {
		return
	}
	if end := r.agent.LastTurnEnd(sk); end.After(started) {
		return
	}
	if now, ok := r.sessionIndex.LastCacheTouch(sk); ok && now.After(before) {
		r.sessionIndex.TouchCacheTouch(sk, before)
	}
}

// defaultKeepaliveTimeout bounds one keepalive turn when keepalive.timeout is
// unset or unparseable (#2489).
const defaultKeepaliveTimeout = 2 * time.Minute

// keepaliveTimeout is the parsed keepalive.timeout; the default when unset,
// zero, negative or unparseable (the last logs a warning).
func (r *Runner) keepaliveTimeout() time.Duration {
	if r.kaCfg.Timeout == "" {
		return defaultKeepaliveTimeout
	}
	d, ok := r.parseDuration("keepalive timeout", r.kaCfg.Timeout)
	if !ok || d <= 0 {
		return defaultKeepaliveTimeout
	}
	return d
}

// keepaliveRunningFor reports whether a keepalive is running on sk now.
func (r *Runner) keepaliveRunningFor(sk string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keepaliveRunning[sk]
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
	return r.keepaliveTargetsAt(interval, time.Now())
}

// keepaliveTargetsAt is keepaliveTargets evaluated at now. Keepalive itself
// passes the wall clock; tests replay it at chosen instants to check that
// keepaliveWillRewarm agrees with it (#1066).
func (r *Runner) keepaliveTargetsAt(interval time.Duration, now time.Time) (ready []string, why string) {
	why = "no session in the warm window"
	candidates := r.keepaliveCandidates()
	if r.sessionIndex == nil {
		return nil, why
	}

	maxIdle := r.keepaliveMaxUserIdle()

	for _, sk := range candidates {
		if r.keepaliveRunningFor(sk) {
			why = "already running on " + sk
			continue // one keepalive per session at a time (#2487)
		}
		if skip := r.checkKeepaliveRateLimit(sk); skip != "" {
			continue // endpoint rate-limited or usage stopped — don't warm into a cap
		}
		if !r.keepaliveUserIdleAllows(sk, now, maxIdle) {
			continue // no human touched this session within max_user_idle — let it expire
		}
		touchAge, ok := r.lastCacheTouchAge(sk, now)
		if !ok {
			continue // never warmed / reset — no live cache to keep alive
		}
		if !keepaliveWarmWindow(touchAge, interval, r.ttlFor(sk)) {
			continue // warmed recently (not due yet), or already expired (don't warm a corpse)
		}
		if r.parentTurnInFlight(sk) {
			why = "due but turn in flight on " + sk
			continue
		}
		ready = append(ready, sk)
	}
	// The sessions a usage hold paused (#2491): warmed during the hold only.
	have := make(map[string]bool, len(candidates))
	for _, sk := range candidates {
		have[sk] = true
	}
	ready = append(ready, r.holdPausedKeepaliveTargets(interval, now, have)...)
	return ready, why
}

// The helpers below are keepalive's per-session decision, shared by
// keepaliveTargets (which warms) and keepaliveWillRewarm (pre-cold
// compaction's "will keepalive warm this again before it expires?", #1066).
// One copy of each rule, so the two can never disagree.

// cachingAvailable reports whether keepalive can warm anything at all.
// cachingOverride allows models with auto-detected caching (OpenAI, DeepSeek)
// to bypass client.IsCachingAvailable(), which may return false for
// non-Anthropic clients.
func (r *Runner) cachingAvailable() bool {
	if r.cachingOverride != nil {
		return *r.cachingOverride
	}
	if r.client != nil {
		return r.client.IsCachingAvailable()
	}
	return true
}

// keepaliveCandidates returns the sessions keepalive considers: the open app
// chats while warm_open_app_chats is on, else (or when none are open) the
// default session.
func (r *Runner) keepaliveCandidates() []string {
	var candidates []string
	if r.kaCfg.WarmOpenAppChats && r.openChatSessionsFn != nil {
		candidates = r.openChatSessionsFn()
	}
	if len(candidates) == 0 {
		if parentKey := r.defaultParentKey(); parentKey != "" {
			candidates = []string{parentKey}
		}
	}
	return candidates
}

// keepaliveMaxUserIdle is the parsed max_user_idle; 0 = no limit (unset, "0",
// or unparseable — the last logs a warning).
func (r *Runner) keepaliveMaxUserIdle() time.Duration {
	if config.KeepaliveMaxUserIdleUnlimited(r.kaCfg.MaxUserIdle) {
		if r.kaCfg.MaxUserIdle != "" {
			// Log an unparseable value the same way as before (a "0" parses
			// cleanly and logs nothing).
			r.parseDuration("keepalive max_user_idle", r.kaCfg.MaxUserIdle)
		}
		return 0
	}
	d, _ := time.ParseDuration(r.kaCfg.MaxUserIdle)
	return d
}

// keepaliveUserIdleAllows reports whether keepalive's max_user_idle gate lets
// it warm sk at the moment at: no limit, or a human touched sk within maxIdle
// of at. No recorded human activity fails the gate when a limit is set.
func (r *Runner) keepaliveUserIdleAllows(sk string, at time.Time, maxIdle time.Duration) bool {
	if maxIdle <= 0 {
		return true
	}
	ua, ok := r.sessionIndex.LastUserActivity(sk)
	return ok && at.Sub(ua) <= maxIdle
}

// keepaliveWarmWindow reports whether a cache last touched touchAge ago is in
// keepalive's warm window [interval, ttl): due for a refresh and not yet
// expired. ttl 0 = unknown, no upper bound.
func keepaliveWarmWindow(touchAge, interval, ttl time.Duration) bool {
	if touchAge < interval {
		return false
	}
	return ttl <= 0 || touchAge < ttl
}

// keepaliveWillRewarm reports whether keepalive, as configured now, will warm
// sk's cache again before it expires. Pre-cold compaction (#1066) runs only
// when this is false. It replays keepaliveTargets' own rules at the first
// moment keepalive could fire — the later of now and touch+interval:
// keepalive enabled with caching available, sk among its candidates, a
// recorded touch, that moment inside the warm window, and the max_user_idle
// gate still open at that moment. The transient gates (rate limit, a turn in
// flight, a yield to another task) only delay keepalive by a tick and are not
// replayed.
func (r *Runner) keepaliveWillRewarm(sk string, now time.Time) bool {
	if !r.kaCfg.Enabled || !r.cachingAvailable() || r.sessionIndex == nil {
		return false
	}
	if !slices.Contains(r.keepaliveCandidates(), sk) {
		return false
	}
	interval, err := time.ParseDuration(r.kaCfg.Interval)
	if err != nil {
		return false // keepalive cannot parse it either, and never fires
	}
	touchAge, ok := r.lastCacheTouchAge(sk, now)
	if !ok {
		return false
	}
	wait := time.Duration(0)
	if touchAge < interval {
		wait = interval - touchAge
	}
	if !keepaliveWarmWindow(touchAge+wait, interval, r.ttlFor(sk)) {
		return false // the warm window is empty or already over
	}
	return r.keepaliveUserIdleAllows(sk, now.Add(wait), r.keepaliveMaxUserIdle())
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
	ttl := r.ttlFor(sessionKey)
	return ok && ttl > 0 && age < ttl
}

// ttlFor is the session's prompt-cache lifetime: CacheTTLFor when wired,
// else the static CacheTTL. 0 = unknown.
func (r *Runner) ttlFor(sessionKey string) time.Duration {
	if r.cacheTTLFor != nil {
		return r.cacheTTLFor(sessionKey)
	}
	return r.cacheTTL
}

package periodic

import (
	"context"
	"fmt"
	"time"

	"foci/internal/session"
	"foci/internal/usagehold"
)

// The usage hold, scheduler side (#2491 requirements 8 and 9, stage d).
//
// At a hold start (the account goes from open to HOLD) UsageHoldStarted
// reflects and then compacts each session that is worth it: warm (used
// within its cache TTL), with longer until the lift than its TTL (else the
// cache outlives the hold and compacting buys nothing), and with its context
// more than half way to its compaction trigger. The candidates are the chats
// quiet-hours compaction considers (the default session and the open app
// chats) plus the sessions the hold paused (spawns, background-work
// branches). The reflection and the compaction carry the hold's permit
// (usagehold.WithPermit), the only periodic work that runs in a hold.
//
// During a hold, keepalive also warms the sessions the hold paused, in their
// warm window, without max_user_idle: nobody "uses" a paused spawn.

// usageHoldPausedReporter is the optional BackgroundAgent side of the hold:
// the sessions it paused, by kind ("spawn", "background").
type usageHoldPausedReporter interface {
	UsageHoldPaused() map[string]string
}

// holdCompactionFraction is how far to its compaction trigger a session's
// context must be for the hold to compact it (Dick 2026-11-02: "only ... when
// >50% of the way to the compaction-trigger context level").
const holdCompactionFraction = 0.5

// turnIdleWaiter is the optional BackgroundAgent side that tells when a
// session's turn has really ended (the agent's in-flight signal).
type turnIdleWaiter interface {
	WaitTurnIdle(ctx context.Context, sessionKey string) bool
}

// defaultHoldIdleTimeout bounds the wait for a target's interrupted turn to
// end before the hold compacts it. An interrupted turn ends within seconds;
// one that does not is stuck, and the compaction's own guard then refuses.
const defaultHoldIdleTimeout = 2 * time.Minute

// UsageHoldStarted runs the reflection and compaction of a hold start in the
// background. liftAt is when the hold lifts (zero: unknown, so nothing is
// compacted).
func (r *Runner) UsageHoldStarted(liftAt time.Time) {
	if r == nil || r.agent == nil {
		return
	}
	go r.holdCompaction(context.Background(), liftAt)
}

// holdCompaction is UsageHoldStarted's body. The targets are fixed at the
// hold start (the interrupt has already recorded the paused sessions); each
// is reflected and compacted once its interrupted turn has really ended.
func (r *Runner) holdCompaction(ctx context.Context, liftAt time.Time) {
	pctx := usagehold.WithPermit(ctx)
	for _, sk := range r.holdCompactionTargets(liftAt, time.Now()) {
		r.waitHoldTargetIdle(ctx, sk)
		r.log.Infof("usage hold: reflecting and compacting %s before the hold (lifts %s)", sk, liftAt.Format(time.RFC3339))
		r.reflectSession(pctx, sk)
		if err := r.agent.QuietCompact(pctx, sk); err != nil {
			r.log.Warnf("usage hold: compaction of %s: %v", sk, err)
		}
	}
}

// waitHoldTargetIdle waits for sk's turn to end (the in-flight signal), up to
// holdIdleTimeout; on the timeout it warns and goes on (QuietCompact then
// re-checks its guards).
func (r *Runner) waitHoldTargetIdle(ctx context.Context, sk string) {
	w, ok := r.agent.(turnIdleWaiter)
	if !ok {
		return
	}
	timeout := r.holdIdleTimeout
	if timeout <= 0 {
		timeout = defaultHoldIdleTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !w.WaitTurnIdle(wctx, sk) {
		r.log.Warnf("usage hold: the turn on %s did not end within %s — compacting anyway (the guard re-checks)", sk, timeout)
	}
}

// holdCompactionCandidates are the chats quiet-hours compaction considers
// plus the sessions the hold paused, deduplicated.
func (r *Runner) holdCompactionCandidates() (chats []string, paused map[string]bool) {
	chats = r.quietCandidates()
	paused = map[string]bool{}
	if rep, ok := r.agent.(usageHoldPausedReporter); ok {
		for sk := range rep.UsageHoldPaused() {
			paused[sk] = true
		}
	}
	return chats, paused
}

// holdCompactionTargets are the sessions a hold that lifts at liftAt compacts
// (worked example A). Each skip logs one DEBUG line.
func (r *Runner) holdCompactionTargets(liftAt, now time.Time) []string {
	if liftAt.IsZero() {
		r.log.Infof("usage hold: no lift time known — no compaction at the hold")
		return nil
	}
	chats, paused := r.holdCompactionCandidates()
	all := append([]string(nil), chats...)
	seen := map[string]bool{}
	for _, sk := range chats {
		seen[sk] = true
	}
	for sk := range paused {
		if !seen[sk] {
			all = append(all, sk)
		}
	}
	var out []string
	for _, sk := range all {
		if skip := r.holdCompactionSkip(sk, paused[sk], liftAt, now); skip != "" {
			r.log.Debugf("usage hold compaction: skip %s: %s", sk, skip)
			continue
		}
		out = append(out, sk)
	}
	return out
}

// holdCompactionSkip is why the hold does not compact sk, or "" when it does.
// A paused spawn's no_compact is foci's own (spawns are never auto-compacted)
// and does not stop this one; a chat's is the owner's and does.
func (r *Runner) holdCompactionSkip(sk string, paused bool, liftAt, now time.Time) string {
	if !paused && r.sessionIndex != nil {
		if v, err := r.sessionIndex.GetSessionMetadata(sk, session.MetaKeyNoCompact); err == nil && v == "true" {
			return "no_compact set"
		}
	}
	if !r.cacheProvablyWarm(sk, now) {
		return "cache not warm"
	}
	ttl := r.ttlFor(sk)
	if until := liftAt.Sub(now); until <= ttl {
		return fmt.Sprintf("lifts in %s, within the cache TTL %s", until.Round(time.Minute), ttl)
	}
	fill, limit := r.agent.ContextUsage(sk)
	if limit <= 0 || fill <= 0 {
		return "unknown context fill or trigger"
	}
	if frac := float64(fill) / float64(limit); frac <= holdCompactionFraction {
		return fmt.Sprintf("context %d%% of the compaction trigger, not over %d%%", int(frac*100), int(holdCompactionFraction*100))
	}
	return ""
}

// holdPausedKeepaliveTargets are the sessions the hold paused that keepalive
// warms now (requirement 9): during a hold only, in their warm window,
// without max_user_idle. Outside a hold the agent reports none paused.
func (r *Runner) holdPausedKeepaliveTargets(interval time.Duration, now time.Time, have map[string]bool) []string {
	rep, ok := r.agent.(usageHoldPausedReporter)
	if !ok {
		return nil
	}
	var out []string
	for sk := range rep.UsageHoldPaused() {
		if have[sk] || r.keepaliveRunningFor(sk) || r.keepaliveOff(sk) || r.keepaliveStopped(sk) {
			continue
		}
		if skip := r.checkKeepaliveRateLimit(sk); skip != "" {
			continue // a stop: keepalives stop too
		}
		touchAge, ok := r.lastCacheTouchAge(sk, now)
		if !ok || !keepaliveWarmWindow(touchAge, interval, r.ttlFor(sk)) {
			continue
		}
		if r.warmAttemptWait(sk, interval, now) > 0 {
			continue // tried within interval (#2509)
		}
		if r.parentTurnInFlight(sk) {
			continue
		}
		out = append(out, sk)
	}
	return out
}

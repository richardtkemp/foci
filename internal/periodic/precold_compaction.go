package periodic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/timeutil"
)

// Pre-cold idle compaction (#1066). When a session has been idle past the
// prompt-cache lifetime, its next turn pays a cache WRITE of the whole prefix.
// Compacting first makes that write smaller. This trigger compacts an idle
// open session shortly before its cache expires, but only when keepalive will
// not warm it again: keepalive is off, or keepalive's max_user_idle gate closes
// before the next warm (Dick, 2026-10-10: "compact before cache goes idle when
// keepalives with limited duration are enabled, or no keepalives are enabled.
// optional, default disabled."). The compaction starts while the cache is
// still warm, so it reads the context cheaply.
//
// It shares quiet-hours compaction's candidate set, safety guards and
// dispatch (Agent.QuietCompact, the /compact pipeline), and keepalive's own
// per-session rules (keepaliveWillRewarm) for the "will keepalive warm it?"
// judgement.

// precoldTarget is one session scheduled for pre-cold compaction, with the
// schedule-time facts the outcome note reports. The dispatch goroutine must
// not read r.precoldCfg, which applySettings can swap while it runs.
type precoldTarget struct {
	sessionKey   string
	fillPct      int
	thresholdPct int
	expiresIn    time.Duration
}

// maybePrecoldCompaction is the pre-cold trigger. An exact no-op when the
// feature is off: it returns before any session, ledger or index lookup.
func (r *Runner) maybePrecoldCompaction(ctx context.Context) {
	if r.agent == nil || !r.precoldCfg.Enabled {
		return
	}
	lead, ok := r.parseDuration("compaction_precold_lead", r.precoldCfg.Lead)
	if !ok {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip pre-cold compaction: %s", skip)
		}
	}()

	r.mu.Lock()
	running := r.precoldCompactionRunning
	quietRunning := r.quietCompactionRunning
	reflectionRunning := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	resetRunning := r.resetRunning
	r.mu.Unlock()

	if running {
		skip = "already running"
		return
	}
	// The same yields as quiet compaction (see maybeQuietCompaction), plus
	// quiet compaction itself: both compact the same sessions. A keepalive is
	// yielded to per session, in precoldCompactionTargets (#2487).
	if quietRunning || reflectionRunning || consolidationRunning || resetRunning {
		skip = "another periodic task running"
		return
	}

	now := r.now()
	targets, why := r.precoldCompactionTargets(now, lead, r.precoldCfg.Threshold)
	if len(targets) == 0 {
		skip = why
		return
	}

	r.mu.Lock()
	r.precoldCompactionRunning = true
	r.mu.Unlock()

	r.log.Infof("firing pre-cold compaction for agent %s (%d session(s))", r.agentID, len(targets))

	go func() {
		defer func() {
			r.mu.Lock()
			r.precoldCompactionRunning = false
			r.mu.Unlock()
		}()
		for _, t := range targets {
			r.runPrecoldCompact(ctx, t)
		}
	}()
}

// precoldCompactionTargets returns the sessions to compact this tick, from
// the same candidate set quiet compaction considers. Each skip logs one
// DEBUG line naming the session and the reason.
func (r *Runner) precoldCompactionTargets(now time.Time, lead time.Duration, threshold float64) (targets []precoldTarget, why string) {
	why = "no idle session about to go cold"
	if r.sessionIndex == nil {
		return nil, "no session index"
	}
	for _, sk := range r.quietCandidates() {
		if r.keepaliveRunningFor(sk) {
			r.log.Debugf("pre-cold compaction: skip %s: keepalive running", sk)
			continue
		}
		t, skip := r.precoldCandidate(sk, now, lead, threshold)
		if skip != "" {
			r.log.Debugf("pre-cold compaction: skip %s: %s", sk, skip)
			continue
		}
		targets = append(targets, t)
	}
	return targets, why
}

// precoldEffectiveLead caps lead at half the cache lifetime: a lead as long as
// the lifetime (a 5-minute cache with the 10m default) would otherwise put
// the window at the very start of the cache's life, right after the user's
// turn, and compact a conversation that is still going on.
func precoldEffectiveLead(lead, ttl time.Duration) time.Duration {
	if half := ttl / 2; lead > half {
		return half
	}
	return lead
}

// precoldCandidate applies the per-session conditions (#1066, all must hold)
// to one candidate. Cheap durable state first, agent calls last.
func (r *Runner) precoldCandidate(sk string, now time.Time, lead time.Duration, threshold float64) (precoldTarget, string) {
	if v, err := r.sessionIndex.GetSessionMetadata(sk, session.MetaKeyNoCompact); err == nil && v == "true" {
		return precoldTarget{}, "no_compact set"
	}

	// A session with no recorded human activity is never compacted: an
	// unprompted action targets sessions somebody used, and the once-per-idle
	// rule below is keyed on that activity.
	ua, ok := r.sessionIndex.LastUserActivity(sk)
	if !ok {
		return precoldTarget{}, "no recorded user activity"
	}

	// Cache warm now, and cold within the lead. Fail closed: no recorded
	// touch, or an unknown TTL, is not provably warm.
	age, touched := r.lastCacheTouchAge(sk, now)
	ttl := r.ttlFor(sk)
	if !touched || ttl <= 0 || age >= ttl {
		return precoldTarget{}, "cache not provably warm"
	}
	expiresIn := ttl - age
	if eff := precoldEffectiveLead(lead, ttl); expiresIn > eff {
		return precoldTarget{}, fmt.Sprintf("cache expires in %s, more than the lead %s", expiresIn.Round(time.Second), eff)
	}

	// Keepalive will not warm it again before it expires.
	if r.keepaliveWillRewarm(sk, now) {
		return precoldTarget{}, "keepalive will re-warm the cache"
	}

	// Once per idle period: a new human interaction re-arms it. Also not
	// after a quiet-hours compaction in this idle period.
	if stamp, ok := r.metaStamp(sk, session.MetaKeyPrecoldCompactedAt); ok && !ua.After(stamp) {
		return precoldTarget{}, "already pre-cold-compacted since the last human interaction"
	}
	if stamp, ok := r.metaStamp(sk, session.MetaKeyQuietCompactedAt); ok && !ua.After(stamp) {
		return precoldTarget{}, "already quiet-compacted since the last human interaction"
	}

	// Usage at or above the threshold, measured since the last compaction
	// (the ledger reports 0 for a fill that predates one).
	fill, limit := r.agent.ContextUsage(sk)
	if limit <= 0 {
		return precoldTarget{}, "unknown context limit"
	}
	if fill <= 0 {
		return precoldTarget{}, "unknown context fill"
	}
	frac := float64(fill) / float64(limit)
	if frac < threshold {
		return precoldTarget{}, fmt.Sprintf("context %d%% below threshold %d%%", int(frac*100), int(threshold*100))
	}

	if skip := r.checkRateLimit(sk); skip != "" {
		return precoldTarget{}, skip
	}
	// Never mid-turn, with queued input, or with background work: the guard
	// set Agent.QuietCompact rechecks immediately before compacting.
	if reason := r.agent.QuietCompactBlocked(sk); reason != "" {
		return precoldTarget{}, reason
	}

	return precoldTarget{
		sessionKey:   sk,
		fillPct:      int(frac * 100),
		thresholdPct: int(threshold * 100),
		expiresIn:    expiresIn,
	}, ""
}

// metaStamp reads an RFC3339Nano timestamp row from the session index (no
// index, no row, or an unparseable row = none recorded).
func (r *Runner) metaStamp(sk, key string) (time.Time, bool) {
	v, err := r.sessionIndex.GetSessionMetadata(sk, key)
	if err != nil || v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// runPrecoldCompact compacts one eligible session and records the outcome.
// Targets compact one after another, so the cache is rechecked first: the
// compaction must START while the cache is warm. Every attempt that passed
// the guards (success, a backend decline, a failure) is stamped
// (precold_compacted_at), which is what makes "once per idle period" hold; a
// dispatch-time refusal is not an attempt and is not stamped.
func (r *Runner) runPrecoldCompact(ctx context.Context, t precoldTarget) {
	now := r.now()
	if !r.cacheProvablyWarm(t.sessionKey, now) {
		r.log.Debugf("pre-cold compaction: %s went cold before dispatch — skipped", t.sessionKey)
		return
	}
	err := r.agent.QuietCompact(ctx, t.sessionKey)
	if errors.Is(err, ErrQuietCompactionRefused) {
		r.log.Debugf("pre-cold compaction: %s refused at dispatch: %v", t.sessionKey, err)
		return
	}

	if serr := r.sessionIndex.SetSessionMetadata(t.sessionKey, session.MetaKeyPrecoldCompactedAt, timeutil.FormatNano(now)); serr != nil {
		r.log.Warnf("pre-cold compaction: stamp %s: %v", t.sessionKey, serr)
	}

	switch {
	case errors.Is(err, delegator.ErrCompactionNoBoundary):
		r.log.Debugf("pre-cold compaction: %s declined by the backend (no boundary) — not retried this idle period", t.sessionKey)
	case err != nil:
		r.log.Warnf("pre-cold compaction: %s failed: %v", t.sessionKey, err)
	default:
		// Posted regardless of compaction_notify: the only sign of an
		// unprompted action.
		mins := int(t.expiresIn.Minutes())
		note := fmt.Sprintf("🧊 Pre-cold compaction: the prompt cache was about to expire (in %dm) and keepalive will not re-warm it; context was %d%% of the window (threshold %d%%).",
			mins, t.fillPct, t.thresholdPct)
		r.log.Infof("pre-cold compaction: %s compacted (cache expiry in %dm, context %d%% of the window, threshold %d%%)",
			t.sessionKey, mins, t.fillPct, t.thresholdPct)
		if r.notifyQuietCompact != nil {
			r.notifyQuietCompact(t.sessionKey, note)
		}
	}
}

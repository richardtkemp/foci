package periodic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/timeutil"
)

// Quiet-hours idle compaction (#2218). A session left overnight at high
// context usage pays a full-context cache write on every keepalive tick for
// hours. During the configured compaction_quiet_hours window, one scheduled
// check per tick compacts each idle open session whose context usage is at or
// above compaction_quiet_threshold — once per window occurrence, while the
// cache from the user's last turn is still warm so the compaction reads
// cheaply, and before the next keepalive re-warms the full context. Keepalive
// yields while a pass runs (see keepalive.go).

// ErrQuietCompactionRefused wraps the error BackgroundAgent.QuietCompact
// returns when the agent's guards refused the compaction at dispatch time
// (a turn started, input was queued, background work appeared). A refusal is
// not an attempt: the runner logs it at DEBUG and does NOT stamp the session,
// so a later tick in the same window may retry once the blocker clears.
var ErrQuietCompactionRefused = errors.New("quiet compaction refused")

// quietTarget is one session scheduled for quiet compaction, carrying the
// schedule-time facts the outcome note reports. Everything the dispatch
// goroutine needs is captured here: it must not read r.quietCfg, which
// applySettings can swap on the run-loop goroutine while it runs.
type quietTarget struct {
	sessionKey   string
	fillPct      int // fill as a percentage of the context window, at pick time
	thresholdPct int
	idle         time.Duration
}

// maybeQuietCompaction is the quiet-hours trigger: inside the configured
// window, collect the eligible sessions and compact them one after another in
// a goroutine. Off by default and an exact no-op when off — an empty window,
// or a time outside it, returns before ANY session, ledger or index lookup.
func (r *Runner) maybeQuietCompaction(ctx context.Context) {
	if r.agent == nil || r.quietCfg.Window == "" {
		return
	}
	win, err := config.ParseQuietWindow(r.quietCfg.Window)
	if err != nil {
		// Unreachable after config validation; kept fail-safe.
		r.log.Warnf("bad compaction_quiet_hours %q: %v", r.quietCfg.Window, err)
		return
	}
	now := r.now()
	if !win.Contains(now) {
		return
	}
	minIdle, ok := r.parseDuration("compaction_quiet_min_idle", r.quietCfg.MinIdle)
	if !ok {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip quiet compaction: %s", skip)
		}
	}()

	r.mu.Lock()
	running := r.quietCompactionRunning
	reflectionRunning := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	resetRunning := r.resetRunning
	keepaliveRunning := r.keepaliveRunning
	r.mu.Unlock()

	if running {
		skip = "already running"
		return
	}
	// Yield to the memory passes and reset (they mutate the same sessions,
	// and reset rotates the key) and to keepalive (warming a session the
	// compaction is about to shrink pays the very full-context write this
	// trigger exists to remove). The run loop orders all four BEFORE this
	// check's consumers; see the ORDER IS LOAD-BEARING comment in run().
	if reflectionRunning || consolidationRunning || resetRunning || keepaliveRunning {
		skip = "another periodic task running"
		return
	}

	targets, why := r.quietCompactionTargets(win, now, minIdle, r.quietCfg.Threshold)
	if len(targets) == 0 {
		skip = why
		return
	}

	r.mu.Lock()
	r.quietCompactionRunning = true
	r.mu.Unlock()

	r.log.Infof("firing quiet-hours compaction for agent %s (%d session(s))", r.agentID, len(targets))

	go func() {
		defer func() {
			r.mu.Lock()
			r.quietCompactionRunning = false
			r.mu.Unlock()
		}()
		for _, t := range targets {
			r.runQuietCompact(ctx, t, now)
		}
	}()
}

// now returns the runner's current time in the process timezone: read from
// the injectable clock (tests drive a fake; production the wall clock) and
// converted so "HH:MM" windows resolve in the one timezone foci has.
func (r *Runner) now() time.Time {
	c := r.clock
	if c == nil {
		c = clock.Real()
	}
	return c.Now().In(timeutil.Location())
}

// quietCompactionTargets returns the sessions to compact this tick. A
// candidate — the default session plus every open app chat, deduplicated —
// is kept only if every guard below holds; each skip logs one DEBUG line
// naming the session and the reason (keepaliveTargets style). why carries the
// overall reason when nothing is eligible.
func (r *Runner) quietCompactionTargets(win config.QuietWindow, now time.Time, minIdle time.Duration, threshold float64) (targets []quietTarget, why string) {
	why = "no idle session at or above the threshold"
	if r.sessionIndex == nil {
		return nil, "no session index"
	}
	occStart := win.OccurrenceStart(now)
	for _, sk := range r.quietCandidates() {
		t, skip := r.quietCandidate(sk, occStart, now, minIdle, threshold)
		if skip != "" {
			r.log.Debugf("quiet compaction: skip %s: %s", sk, skip)
			continue
		}
		targets = append(targets, t)
	}
	return targets, why
}

// quietCandidates returns the deduplicated candidate sessions for quiet
// compaction (#2218): the agent's default session plus every app-open chat —
// unconditionally, unlike keepalive's openSessionsFn, whose nil-ness encodes
// warm_open_app_chats. The trigger applies to ALL open sessions.
func (r *Runner) quietCandidates() []string {
	var candidates []string
	if sk := r.defaultParentKey(); sk != "" {
		candidates = append(candidates, sk)
	}
	if r.openChatSessionsFn != nil {
		candidates = append(candidates, r.openChatSessionsFn()...)
	}
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, sk := range candidates {
		if sk == "" || seen[sk] {
			continue
		}
		seen[sk] = true
		out = append(out, sk)
	}
	return out
}

// quietCandidate applies the per-session conditions (#2218, all must hold) to
// one candidate and returns either the schedule-time facts for the outcome
// note, or the skip reason. Order follows the requirement: cheap durable
// state first (index rows), agent calls last.
func (r *Runner) quietCandidate(sk string, occStart, now time.Time, minIdle time.Duration, threshold float64) (quietTarget, string) {
	// a. Not no_compact.
	if v, err := r.sessionIndex.GetSessionMetadata(sk, session.MetaKeyNoCompact); err == nil && v == "true" {
		return quietTarget{}, "no_compact set"
	}

	// b. Idle at least min_idle since the later of the last human activity
	// and the last turn end. A session with no recorded human activity is
	// never compacted — an unprompted action targets sessions somebody used.
	ua, ok := r.sessionIndex.LastUserActivity(sk)
	if !ok {
		return quietTarget{}, "no recorded user activity"
	}
	idleSince := ua
	if end := r.agent.LastTurnEnd(sk); end.After(idleSince) {
		idleSince = end
	}
	idle := now.Sub(idleSince)
	if idle < minIdle {
		return quietTarget{}, fmt.Sprintf("idle %s < %s", idle.Round(time.Second), minIdle)
	}

	// c. Usage at or above the threshold. An unknown fill (no ledger, or no
	// turn with a fill) or an unknown limit skips the session: compaction is
	// only justified by a measured, high context.
	fill, limit := r.agent.ContextUsage(sk)
	if limit <= 0 {
		return quietTarget{}, "unknown context limit"
	}
	if fill <= 0 {
		return quietTarget{}, "unknown context fill"
	}
	frac := float64(fill) / float64(limit)
	if frac < threshold {
		return quietTarget{}, fmt.Sprintf("context %d%% below threshold %d%%", int(frac*100), int(threshold*100))
	}

	// d. At most once per window occurrence — and, if the session was ever
	// quiet-compacted before, only after a human interacted with it since.
	// The second rule stops the stale ledger fill (it skips compaction turns,
	// so it still reports the pre-compaction size until the next real turn)
	// from compacting an untouched session again every night.
	if stamp, ok := r.quietStamp(sk); ok {
		if !stamp.Before(occStart) {
			return quietTarget{}, "already quiet-compacted in this window"
		}
		if !ua.After(stamp) {
			return quietTarget{}, "no human interaction since the last quiet compaction"
		}
	}

	// e. Endpoint not rate-limited (the shared gate).
	if skip := r.checkRateLimit(sk); skip != "" {
		return quietTarget{}, skip
	}

	// f. Never mid-turn and never with background work — the same guard set
	// Agent.QuietCompact rechecks immediately before compacting.
	if reason := r.agent.QuietCompactBlocked(sk); reason != "" {
		return quietTarget{}, reason
	}

	// g. The cache must still be warm (fail closed: no recorded touch, or an
	// unknown TTL, is not warm) — same rule keepalive warms by.
	if !r.cacheProvablyWarm(sk, now) {
		return quietTarget{}, "cache not provably warm"
	}

	return quietTarget{
		sessionKey:   sk,
		fillPct:      int(frac * 100),
		thresholdPct: int(threshold * 100),
		idle:         idle,
	}, ""
}

// quietStamp reads the session's last quiet-compaction attempt time from the
// session index (no index, no row, or an unparseable row = none recorded).
func (r *Runner) quietStamp(sk string) (time.Time, bool) {
	v, err := r.sessionIndex.GetSessionMetadata(sk, session.MetaKeyQuietCompactedAt)
	if err != nil || v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// runQuietCompact compacts one eligible session and records the outcome.
//
// Every attempt that PASSED the guards — success, a backend decline
// (delegator.ErrCompactionNoBoundary), or a failure — is stamped on the
// session (quiet_compacted_at), so a failed attempt is not retried every tick
// for the rest of the window; that stamp is what makes "at most once per
// window" hold. A refusal (the guards changed between pick and dispatch) is
// not an attempt and is not stamped. A decline is not an error: DEBUG. A
// failure is WARN.
func (r *Runner) runQuietCompact(ctx context.Context, t quietTarget, now time.Time) {
	err := r.agent.QuietCompact(ctx, t.sessionKey)
	if errors.Is(err, ErrQuietCompactionRefused) {
		r.log.Debugf("quiet compaction: %s refused at dispatch: %v", t.sessionKey, err)
		return
	}

	if r.sessionIndex != nil {
		if serr := r.sessionIndex.SetSessionMetadata(t.sessionKey, session.MetaKeyQuietCompactedAt, timeutil.FormatNano(now)); serr != nil {
			r.log.Warnf("quiet compaction: stamp %s: %v", t.sessionKey, serr)
		}
	}

	switch {
	case errors.Is(err, delegator.ErrCompactionNoBoundary):
		r.log.Debugf("quiet compaction: %s declined by the backend (no boundary) — not retried this window", t.sessionKey)
	case err != nil:
		r.log.Warnf("quiet compaction: %s failed: %v", t.sessionKey, err)
	default:
		// The note is the only sign of an unprompted action, so it is posted
		// regardless of compaction_notify (the ordinary compaction notices
		// inside the pipeline still follow it).
		note := fmt.Sprintf("🌙 Quiet-hours compaction: context was %d%% of the window (threshold %d%%), idle %dm.",
			t.fillPct, t.thresholdPct, int(t.idle.Minutes()))
		r.log.Infof("quiet-hours compaction: %s compacted (context %d%% of the window, threshold %d%%, idle %dm)",
			t.sessionKey, t.fillPct, t.thresholdPct, int(t.idle.Minutes()))
		if r.notifyQuietCompact != nil {
			r.notifyQuietCompact(t.sessionKey, note)
		}
	}
}

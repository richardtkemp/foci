package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/periodic"
	"foci/internal/platform"
)

// backgroundAgent adapts an agentInstance to periodic.BackgroundAgent — the
// single dependency the periodic.Runner drives. It encapsulates the wiring that
// previously lived as a wall of eight injected closures in setupPeriodic,
// including the L2 control-socket test overrides for HasActiveWork and CanFire.
type backgroundAgent struct {
	inst    *agentInstance
	connMgr platform.ConnectionManager
	agentID string
	// branch is buildBranchFunc's result: session branching (API) or
	// in-session/independent injection (delegated), plus the background memory hook.
	branch periodic.BranchFunc
}

func (b *backgroundAgent) Branch(branchType, parentKey, promptText string, noCompact bool) bool {
	return b.branch(branchType, parentKey, promptText, noCompact)
}

func (b *backgroundAgent) HasActiveWork() int {
	// Test-only override: if the L2 control socket has set a value (>= 0), use it
	// verbatim. The -1 sentinel means no override — fall through to the production
	// path (tmuxWatchCount, which is nil for delegated agents).
	if v := b.inst.testActiveWorkOverride.Load(); v >= 0 {
		return int(v)
	}
	if b.inst.tmuxWatchCount == nil {
		return 0
	}
	return b.inst.tmuxWatchCount()
}

func (b *backgroundAgent) DrainRateLimitQueue(ctx context.Context) {
	b.inst.ag.DrainRateLimitQueue(ctx)
}

func (b *backgroundAgent) IsTurnInFlight(parentBase string) bool {
	return b.inst.ag.IsTurnInFlight(parentBase)
}

func (b *backgroundAgent) SessionKey() string {
	return defaultSessionKeyFor(b.inst.ag, b.agentID)
}

func (b *backgroundAgent) CanFire(ctx context.Context, sessionKey string) (bool, string) {
	// Test-only override: if the L2 control socket has set a state, return it
	// verbatim. A nil pointer means no override — use the production rate-limit /
	// can_run_background check on the agent.
	if s := b.inst.testCanFireOverride.Load(); s != nil {
		return s.allowed, s.reason
	}
	return b.inst.ag.CanFireBackgroundOperation(ctx, sessionKey)
}

func (b *backgroundAgent) RateLimited(sessionKey string) (bool, string) {
	// Honour the same L2 control-socket override as CanFire: a locked
	// (allowed=false) state blocks every scheduler, so "not allowed" maps to
	// "rate-limited" here. A nil pointer means no override — use the real gate.
	if s := b.inst.testCanFireOverride.Load(); s != nil {
		return !s.allowed, s.reason
	}
	return b.inst.ag.SessionRateLimited(sessionKey)
}

func (b *backgroundAgent) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	if b.inst.ag.DelegatedManager == nil {
		return "", nil
	}
	return b.inst.ag.DelegatedManager.RunBatch(ctx, req)
}

func (b *backgroundAgent) ResetSession(ctx context.Context, sessionKey string) error {
	_, err := b.inst.ag.ResetSession(ctx, sessionKey)
	return err
}

func (b *backgroundAgent) CleanupEphemeralSessions(ctx context.Context, retentionDays int) int {
	return b.inst.ag.CleanupEphemeralSessions(ctx, retentionDays)
}

func (b *backgroundAgent) LastTurnEnd(sessionKey string) time.Time {
	return b.inst.ag.LastTurnEnd(sessionKey)
}

// ContextUsage returns the session's context fill and limit (#2218): fill
// from the cost ledger — every backend books there — and 0 (unknown) when no
// turn with a fill is recorded OR a compaction is booked after the latest
// fill (#2235: that fill is the pre-compaction size, and acting on it would
// compact a just-compacted session again). limit is the session's USABLE
// context: the token count at which foci auto-compacts it
// (CompactionLimitTokens), so the quiet threshold is a fraction of that, not
// of the model's whole window (Dick, 2026-10-08: on a 1M window the default
// curve compacts near 48%, and 370-450K sessions sat below "50%" all night).
// With auto-compaction off it falls back to the model's context window.
func (b *backgroundAgent) ContextUsage(sessionKey string) (fill, limit int) {
	limit = int(b.inst.ag.CompactionLimitTokens(sessionKey))
	if limit <= 0 {
		limit = b.inst.ag.SessionContextLimit(sessionKey)
	}
	if l := accounting.Live(); l != nil {
		if f, err := l.CurrentContextFill(sessionKey); err == nil {
			fill = f
		}
	}
	return fill, limit
}

func (b *backgroundAgent) QuietCompactBlocked(sessionKey string) string {
	return b.inst.ag.QuietCompactBlocked(sessionKey)
}

func (b *backgroundAgent) QuietCompact(ctx context.Context, sessionKey string) error {
	err := b.inst.ag.QuietCompact(ctx, sessionKey)
	var refused *agent.QuietCompactRefusedError
	if errors.As(err, &refused) {
		return fmt.Errorf("%w: %s", periodic.ErrQuietCompactionRefused, refused.Reason)
	}
	return err
}

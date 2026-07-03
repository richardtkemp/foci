package main

import (
	"context"

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
	// mana check on the agent.
	if s := b.inst.testCanFireOverride.Load(); s != nil {
		return s.allowed, s.reason
	}
	return b.inst.ag.CanFireBackgroundOperation(ctx, sessionKey)
}

func (b *backgroundAgent) RunOnce(ctx context.Context, prompt, systemPrompt string) (string, error) {
	if b.inst.ag.DelegatedManager == nil {
		return "", nil
	}
	return b.inst.ag.DelegatedManager.RunOnce(ctx, prompt, systemPrompt)
}

func (b *backgroundAgent) ResetSession(ctx context.Context, sessionKey string) error {
	err := b.inst.ag.ResetSession(ctx, sessionKey)
	return err
}

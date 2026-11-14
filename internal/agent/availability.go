package agent

import (
	"time"

	"foci/internal/delegator"
)

// Unavailable reports why this agent cannot run a turn now, for a sender that
// must be told (#2892): a usage hold or stop on its account, or a backend that
// needs a login (only an agent on the Claude Code login, UsesCCLogin, #3014).
// Both are waits: what is queued runs when they end. A dead backend is not
// here: it shows only when a turn fails.
func (a *Agent) Unavailable() (reason string, unavailable bool) {
	if blocked, why := a.UsageHoldBlocks(false); blocked {
		return why, true
	}
	if g := a.LoginGateOrDefault(); a.UsesCCLogin() && (g.Active() || g.Pending()) {
		return "its backend needs a new login", true
	}
	return "", false
}

// SessionLastActivity is when sk's backend last emitted a stream event, zero
// when it has no live backend or tracks no activity (an API agent).
// Non-creating.
func (a *Agent) SessionLastActivity(sk string) time.Time {
	if a.DelegatedManager == nil {
		return time.Time{}
	}
	mb, ok := a.DelegatedManager.getManaged(sk)
	if !ok {
		return time.Time{}
	}
	if ac, ok := delegator.As[delegator.ActivityChecker](mb.be); ok {
		return ac.LastActivity()
	}
	return time.Time{}
}

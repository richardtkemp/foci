package agent

import (
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/usagehold"
)

// The forced lift of the usage gates (#3043, Dick 2026-11-11: "/limit-reset
// cmd, force removes the gate. Also /usage to unblock IFF it finds available
// usage. Both ccstream only").
//
// Two gates can keep a Claude Code agent's work from running after the
// operator resets the account's limit:
//   - the agent's endpoint rate-limit gate (ratelimit_gate.go), closed by a
//     rejected rate_limit_event or a synthetic limit result; and
//   - the account-wide usage hold (usage_hold.go, internal/usagehold).
//
// Both clear only on their own clock or on a new event, and in a usage STOP
// nothing runs that could bring one. ResetLimits clears both, in memory and
// in the stored state, so the work runs again without a restart and a
// restart does not bring the gate back. The hold is account-wide: clearing it
// from one agent lifts it for every agent on the account.

// LimitReset reports what ResetLimits cleared.
type LimitReset struct {
	// GateUntil is the deadline the endpoint gate had (zero: it was open).
	GateUntil time.Time
	// HoldFrom is the usage-hold phase before the reset (Open: none).
	HoldFrom usagehold.Phase
	// HoldLiftAt is when that hold would have lifted on its own.
	HoldLiftAt time.Time
}

// Cleared reports whether the reset lifted anything.
func (r LimitReset) Cleared() bool {
	return !r.GateUntil.IsZero() || r.HoldFrom != usagehold.Open
}

// Summary is one chat line naming what the reset lifted.
func (r LimitReset) Summary() string {
	var parts []string
	if r.HoldFrom != usagehold.Open {
		parts = append(parts, fmt.Sprintf("the account's usage %s (was until %s)", r.HoldFrom, formatLift(r.HoldLiftAt)))
	}
	if !r.GateUntil.IsZero() {
		parts = append(parts, fmt.Sprintf("this agent's rate-limit gate (was until %s)", formatLift(r.GateUntil)))
	}
	if len(parts) == 0 {
		return "Nothing was blocked: no rate-limit gate or usage hold was in force."
	}
	return "Lifted " + strings.Join(parts, " and ") + ". Waiting work runs now."
}

// ResetLimits force-opens this agent's rate-limit gates and clears its
// account's usage hold, in memory and in the stored state. Held input and
// paused work proceed as at a natural lift: the hold's subscribers see the
// change to Open (usageHoldChanged wakes the inboxes, releaseUsageHeld runs
// the held input), and the next periodic tick replays the gate's queued work
// (DrainRateLimitQueue). why is logged.
func (a *Agent) ResetLimits(why string) LimitReset {
	var res LimitReset
	now := time.Now()

	// The gates: the default endpoint's (restored from its stored deadline if
	// this process has not built it yet) and every gate built so far.
	a.getOrCreateRateLimitGate(a.Endpoint)
	a.rateLimitGatesMu.RLock()
	gates := make(map[string]*RateLimitGate, len(a.rateLimitGates))
	for ep, g := range a.rateLimitGates {
		gates[ep] = g
	}
	a.rateLimitGatesMu.RUnlock()
	for ep, g := range gates {
		if limited, until := g.IsLimited(); limited && until.After(res.GateUntil) {
			res.GateUntil = until
		}
		g.Open()
		// Delete the stored deadline whether or not this process saw it
		// closed: a past deadline left in the store is harmless, a live one
		// would close the gate again at the next start.
		a.persistRateLimitDeadline(ep, time.Time{})
	}

	if acct := a.UsageAccount(); acct != "" {
		before := a.usageRegistry().State(acct, now)
		from, _ := a.usageRegistry().Clear(acct, now)
		res.HoldFrom = from
		res.HoldLiftAt = before.LiftAt
	}

	gate := "open"
	if !res.GateUntil.IsZero() {
		gate = "closed until " + res.GateUntil.Format(time.RFC3339)
	}
	a.logger().Infof("limit reset (%s): rate-limit gate was %s, usage hold was %s", why, gate, res.HoldFrom)
	return res
}

// UsageAvailable reports whether a usage read shows usage available on this
// agent's account: the read carries rate-limit windows, and each one it
// reports is under the usage-hold threshold (so none is reached).
func (a *Agent) UsageAvailable(info *delegator.UsageInfo) bool {
	return info.Available(a.usageRegistry().ThresholdPercent())
}

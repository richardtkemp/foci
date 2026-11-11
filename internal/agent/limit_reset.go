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
//
// A third thing outlives the reset (#3044): a turn whose backend is asleep in
// its own rate-limit retry (CC's api_retry, up to hours). The reset reaches
// every agent on the account (usagehold.Registry.SubscribeReset), and each
// opens its own gates and interrupts its sleeping turns, which resume with
// the hidden resume line.

// LimitReset reports what ResetLimits cleared.
type LimitReset struct {
	// GateUntil is the deadline the endpoint gate had (zero: it was open).
	GateUntil time.Time
	// HoldFrom is the usage-hold phase before the reset (Open: none).
	HoldFrom usagehold.Phase
	// HoldLiftAt is when that hold would have lifted on its own.
	HoldLiftAt time.Time
	// Woken counts this agent's turns that were asleep in a rate-limit retry
	// and were interrupted to resume (#3044).
	Woken int
}

// Cleared reports whether the reset lifted anything.
func (r LimitReset) Cleared() bool {
	return !r.GateUntil.IsZero() || r.HoldFrom != usagehold.Open || r.Woken > 0
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
	woken := ""
	if r.Woken > 0 {
		woken = fmt.Sprintf(" Resumed %d turn(s) that were waiting in a rate-limit retry.", r.Woken)
	}
	if len(parts) == 0 {
		if woken != "" {
			return strings.TrimSpace(woken)
		}
		return "Nothing was blocked: no rate-limit gate or usage hold was in force."
	}
	return "Lifted " + strings.Join(parts, " and ") + ". Waiting work runs now." + woken
}

// ResetLimits force-opens this agent's rate-limit gates and clears its
// account's usage hold, in memory and in the stored state. Held input and
// paused work proceed as at a natural lift: the hold's subscribers see the
// change to Open (usageHoldChanged wakes the inboxes, releaseUsageHeld runs
// the held input), and the next periodic tick replays the gate's queued work
// (DrainRateLimitQueue). A turn asleep in a rate-limit retry is interrupted
// and resumed (#3044, wakeRateLimitRetries). The clear reaches every agent on
// the account (onAccountLimitReset): each opens its own gates and wakes its
// own sleeping turns. why is logged.
func (a *Agent) ResetLimits(why string) LimitReset {
	var res LimitReset
	now := time.Now()
	if acct := a.UsageAccount(); acct != "" {
		before := a.usageRegistry().State(acct, now)
		// The other agents on the account react through their reset
		// subscription; this one does its part below, for the report.
		a.usageHold.resetting.Store(true)
		from, _ := a.usageRegistry().Clear(acct, now)
		a.usageHold.resetting.Store(false)
		res.HoldFrom = from
		res.HoldLiftAt = before.LiftAt
	}
	res.GateUntil, res.Woken = a.applyLimitReset()

	gate := "open"
	if !res.GateUntil.IsZero() {
		gate = "closed until " + res.GateUntil.Format(time.RFC3339)
	}
	a.logger().Infof("limit reset (%s): rate-limit gate was %s, usage hold was %s, %d turn(s) woken from a rate-limit retry", why, gate, res.HoldFrom, res.Woken)
	return res
}

// onAccountLimitReset is the agent's reset subscription: another agent's
// ResetLimits cleared the account (the account's limit was reset), so this
// one opens its gates and wakes its sleeping turns too.
func (a *Agent) onAccountLimitReset() {
	if a.usageHold.resetting.Load() {
		return
	}
	until, woken := a.applyLimitReset()
	if !until.IsZero() || woken > 0 {
		a.logger().Infof("limit reset on the account: rate-limit gate opened (was until %s), %d turn(s) woken from a rate-limit retry", formatLift(until), woken)
	}
}

// applyLimitReset is one agent's part of a limit reset: open every gate
// (returning the latest deadline it had) and wake the turns asleep in a
// rate-limit retry (returning how many).
func (a *Agent) applyLimitReset() (gateUntil time.Time, woken int) {
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
		if limited, until := g.IsLimited(); limited && until.After(gateUntil) {
			gateUntil = until
		}
		g.Open()
		// Delete the stored deadline whether or not this process saw it
		// closed: a past deadline left in the store is harmless, a live one
		// would close the gate again at the next start.
		a.persistRateLimitDeadline(ep, time.Time{})
	}
	return gateUntil, a.wakeRateLimitRetries()
}

// wakeRateLimitRetries interrupts every turn asleep in a rate-limit retry and
// resumes it with the hidden resume line, the way a hold start interrupts a
// turn (#3044, Dick 2026-11-11: "Stop only turns that are sleeping in a
// rate-limit retry, then send the hidden resume line"). The backend's own
// retry does not see foci's reset: CC slept ~2h in one on 2026-11-11.
func (a *Agent) wakeRateLimitRetries() int {
	sleeping := a.rateLimitRetrySessions()
	if len(sleeping) == 0 {
		return 0
	}
	return a.interruptTurns(func(key string) bool { return sleeping[key] }, true)
}

// noteBackendRetry records a retry the backend reports on sk (OnAPIRetry):
// a rate-limit one (category rate_limit or status 429) marks sk asleep until
// its delay ends; any other replaces the mark.
func (a *Agent) noteBackendRetry(sk string, r delegator.APIRetry) {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if r.Category != "rate_limit" && r.Status != 429 {
		delete(a.rateLimitRetry, sk)
		return
	}
	if a.rateLimitRetry == nil {
		a.rateLimitRetry = map[string]time.Time{}
	}
	a.rateLimitRetry[sk] = time.Now().Add(r.Delay)
}

// noteBackendProgress clears sk's retry mark: the backend produced output, so
// it is no longer asleep.
func (a *Agent) noteBackendProgress(sk string) {
	a.inFlightMu.Lock()
	if len(a.rateLimitRetry) > 0 {
		delete(a.rateLimitRetry, sk)
	}
	a.inFlightMu.Unlock()
}

// InRateLimitRetry reports whether sk's turn is asleep in a rate-limit retry
// now: its backend's last report was a rate-limit retry whose delay has not
// ended, with no output since and the turn still running.
func (a *Agent) InRateLimitRetry(sk string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	until, ok := a.rateLimitRetry[sk]
	return ok && time.Now().Before(until)
}

// rateLimitRetrySessions are the sessions InRateLimitRetry reports now.
func (a *Agent) rateLimitRetrySessions() map[string]bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	now := time.Now()
	out := map[string]bool{}
	for sk, until := range a.rateLimitRetry {
		if now.Before(until) {
			out[sk] = true
		}
	}
	return out
}

// UsageAvailable reports whether a usage read shows usage available on this
// agent's account: the read carries rate-limit windows, and each one it
// reports is under the usage-hold threshold (so none is reached).
func (a *Agent) UsageAvailable(info *delegator.UsageInfo) bool {
	return info.Available(a.usageRegistry().ThresholdPercent())
}

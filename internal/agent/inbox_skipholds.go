// inbox_skipholds.go — messages that skip the injection holds (#3114).
//
// Dick 2026-11-14: a message sent within one agent (a spawn to its parent or
// to another spawn, send_to_session between the agent's own sessions, a
// [SPAWN RESULT], [DEPTH RESULTS]) and an async tool result skip the holds a
// system injection waits for. Usage gates still apply, and so does a CC
// re-login. Such an injection carries InjectMeta.SkipHolds and its message in
// Envelope.Text. Rulings Q40-Q44 on the ticket:
//
//   - a running user turn or adopted autonomous run: steer into it;
//   - a running protected system turn: run right after it (it is never
//     steered, #2591); a running work turn (#3120) is steered into;
//   - an open foci_ask does not defer it (runInject);
//   - background work with no run in flight still parks it (#1068: a system
//     turn begun in that window swallows the coming autonomous run's output).
//     It steers into that run when the run starts (the held watch wakes on
//     the adoption edge, peerSteerTarget), or runs when the window closes or
//     the #2792 limit passes.
//
// All of the steering above is for a backend that steers natively
// (backendSteersMidTurn, Q45). On one that does not, the message is never
// steered: it runs right after the current turn, ahead of everything else
// queued (Envelope.jumpQueue), and no turn is aborted for it.
//
// Scheduled wakes, cron and foci send injections, webhooks and messages from
// other agents keep today's holds: nothing marks them SkipHolds.

package agent

import (
	"foci/internal/delegator"
	"foci/internal/platform"
)

// backendSteersMidTurn reports whether this agent's backend folds a message
// into a running turn natively: false only when its Spec declares
// delegator.CapMidTurnSteer No or n/a (opencode). An API agent (its
// tool-boundary steer buffer) and ccstream/codex steer. Dick 2026-11-14
// (Q45): a SkipHolds message is steered only there; on a backend that does
// not steer, no turn is aborted for one: the message runs right after the
// current turn, ahead of the queue (Envelope.jumpQueue).
//
// The one mid-turn steer check: trySteer (a typed or user-input message
// queues for the next turn, in arrival order, #3115), SteerTurn (spawn_send
// queues for the spawn's next turn) and peerSteerAllowed all key on it.
func (a *Agent) backendSteersMidTurn() bool {
	switch a.delegatedSpec().Support(delegator.CapMidTurnSteer).State() {
	case delegator.Unsupported, delegator.NotApplicableState:
		return false
	}
	return true
}

// peerSteerAllowed reports whether env is a SkipHolds injection that may fold
// into a running turn right now: the backend steers natively, it has text to
// fold, no usage hold holds it, and no CC re-login is running or pending (a
// dead credential cannot take a fold either).
func (a *Agent) peerSteerAllowed(env Envelope) bool {
	m := env.Inject
	if m == nil || !m.SkipHolds || env.Text == "" || !a.backendSteersMidTurn() {
		return false
	}
	if a.usageHoldsTrigger(env.SessionKey, m.Trigger, m.HoldPermit) {
		return false
	}
	if a.UsesCCLogin() {
		if g := a.loginGate(env.SessionKey); g.Active() || g.Pending() {
			return false
		}
	}
	return true
}

// steerPeerInject folds a SkipHolds injection into its session's running turn
// — the same steer block a typed message takes (trySteer), with steering
// forced on (the ruling is "steer in", whatever the agent's steer_mode), the
// system-turn and compaction carve-outs kept. It reports whether the message
// was consumed; on success it runs the injection's Steered hook in place of
// Run. False leaves the envelope to the caller, unchanged.
func (a *Agent) steerPeerInject(inb *sessionInbox, env Envelope) bool {
	if !a.peerSteerAllowed(env) {
		return false
	}
	env.Steer = platform.SteerAlways
	env.Automated = true
	if !a.trySteer(inb, env, a.sessionTurnActive(inb, env.SessionKey), a.IsCompacting(env.SessionKey)) {
		return false
	}
	a.logger().Infof("inbox: injection trigger=%s steered into the running turn sk=%s (#3114)", env.Inject.Trigger, env.SessionKey)
	if env.Inject.Steered != nil {
		env.Inject.Steered()
	}
	return true
}

// peerSteerTarget reports whether a SkipHolds injection the worker holds has a
// run to steer into now: a backend turn in flight that is not a protected turn
// and not a compaction. On the worker that is an adopted autonomous run (the
// worker drives every other turn itself). The held watch reads it, so a
// parked message is released the moment such a run starts. False after a
// failed steer (Envelope.peerSteerFailed), so a backend that refuses the fold
// cannot spin the worker.
func (a *Agent) peerSteerTarget(env Envelope) bool {
	if env.peerSteerFailed || a.DelegatedManager == nil || !a.peerSteerAllowed(env) {
		return false
	}
	sk := env.SessionKey
	return a.DelegatedManager.BackendTurnInFlight(sk) && !a.DelegatedManager.ProtectedTurnInFlight(sk) && !a.IsCompacting(sk)
}

// tryPeerSteerHeld steers a SkipHolds injection the worker is dispatching or
// releasing into the run peerSteerTarget names. It reports whether the
// message was consumed. A steer that was attempted and failed marks env
// (peerSteerFailed), so it then waits only for its own hold.
func (a *Agent) tryPeerSteerHeld(inb *sessionInbox, env *Envelope) bool {
	if !a.peerSteerTarget(*env) {
		return false
	}
	if a.steerPeerInject(inb, *env) {
		// One a usage hold parked and stored leaves the restart store, as
		// runInject takes it off when it runs (#2491).
		a.forgetUsageHeld(*env)
		return true
	}
	env.peerSteerFailed = true
	a.logger().Infof("inbox: injection trigger=%s could not steer into the running turn sk=%s; it waits for the turn to end (#3114)", env.Inject.Trigger, env.SessionKey)
	return false
}

// injectClass names the FIFO class heldSiblingPins keeps order within:
// keepalives, SkipHolds messages, and every other system injection.
func injectClass(m *InjectMeta) int {
	switch {
	case isKeepaliveTrigger(m.Trigger):
		return 1
	case m.SkipHolds:
		return 2
	default:
		return 0
	}
}

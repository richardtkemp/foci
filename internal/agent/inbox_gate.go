// inbox_gate.go — the pending-work gate for system injections (#1068/#1070,
// spec §4) and the dispatch path every injection takes out of the queue.
//
// One function decides "may a system inject run now" (injectHeld), one
// function moves an injection from the queue towards running (dispatchInject),
// and one predicate reports "is background work holding this session"
// (holdActive) for the gate, the remind tool's fired-wake list and the async
// /send 202 receipt alike — so the wait, the dispatch and the reporting cannot
// drift apart (#2233).

package agent

import (
	"context"
	"sync"
	"time"

	"foci/internal/log"
)

// injectGatePollInterval is the backstop tick for the inject gate's wait loop.
// The adoption edge broadcasts via InFlightWaitCh, but a pending-background-work
// transition (a tracked subagent/Bash completing) has no channel, so the wait
// polls backendAwaitingAutonomousRun at this cadence (ALARP: a poll beats new
// callback plumbing for a rare, bounded wait). Not configurable — a fixed 1s
// tick on an already-blocked wait. Tests shorten it per agent
// (Agent.injectGatePoll) so a pending-work transition is seen in milliseconds,
// through the same poll path (#2537).
const injectGatePollInterval = 1 * time.Second

// gatePollInterval is the poll backstop this agent's gate waits use.
func (a *Agent) gatePollInterval() time.Duration {
	if a.injectGatePoll > 0 {
		return a.injectGatePoll
	}
	return injectGatePollInterval
}

// backendAwaitingAutonomousRun is the nil-safe wrapper over
// DelegatedManager.BackendAwaitingAutonomousRun: false for API agents (no
// DelegatedManager) and for idle/non-tracking backends. Used by the inbox
// inject gate to hold system injects across the background-work window (spec §4).
func (a *Agent) backendAwaitingAutonomousRun(sk string) bool {
	if a.DelegatedManager == nil {
		return false
	}
	return a.DelegatedManager.BackendAwaitingAutonomousRun(sk)
}

// holdActive is THE "is background work holding this session" predicate: the
// backend's whole background-work window (backendAwaitingAutonomousRun), or a
// delivering autonomous run the session's own worker is NOT driving. The gate
// (waitInjectGate, injectHeld), the remind tool's fired-wake list and the
// async /send 202 receipt all consult it (#2233).
//
// A turn the worker itself is running never counts (the busy refinement): the
// worker is not idle, so nothing is parked behind it ON THIS GATE — user
// messages queue as normal turns, exactly as without background work. This
// matters only for reporting; the gate itself is evaluated by an idle worker,
// where busy is zero by construction.
func (a *Agent) holdActive(sk string) bool {
	return a.backendAwaitingAutonomousRun(sk) || a.adoptedRunActive(sk)
}

// adoptedRunActive is holdActive's adopted-run half: a delivering autonomous
// run is in flight and the session's own worker is NOT driving it. This is
// the only half that applies to a keepalive (see injectHeld).
func (a *Agent) adoptedRunActive(sk string) bool {
	return a.IsInFlightDelivering(sk) && a.workerIdle(sk)
}

// workerIdle reports whether sk's session worker is between dispatches. An
// unknown session has no worker, so it reads as idle.
func (a *Agent) workerIdle(sk string) bool {
	inb := a.lookupInbox(sk)
	return inb == nil || inb.busy.Load() == 0
}

// SystemInputHeld reports whether a system inject on sk would be held right
// now by background work — the shared visibility predicate for the remind
// tool's fired-wake reporting and the async /send 202 receipt (#2233). A turn
// the session's own worker is running (a user turn, or the fired wake's own
// system turn) does not count as a hold.
func (a *Agent) SystemInputHeld(sk string) bool {
	return a.holdActive(sk)
}

// injectGateApplies reports whether the pending-work gate (#1068/#1070, spec
// §4) applies to this injection at all. It does NOT apply to a user-input
// injection (an ask answer): the user's own action, dispatched with the user
// path and timing (#2233). Nor does it apply to a redelivery (#2362, Dick
// 2026-10-28): it re-sends a message the user already sent, which a dead
// backend never read, so it waits only for what a queued user message waits
// for (re-login, a non-delivering turn in flight, compaction) — never for
// background work, from either half of the hold.
//
// An inject that runs ON A BRANCH is exempt structurally, not by a field: the
// branch machinery runs its turn via HandleMessage on the branch's own key —
// handleDelegatedBranch's real-fork and RunIndependent arms for delegated
// agents, buildBranchFunc's branch-session path for API agents — so it never
// enters the parent's inbox and cannot touch the parent session's sink, which
// is the whole #1068 reason for the gate. Only the in-place shapes enqueue on
// the parent: RunInPlace and the fork-unavailable fallback, which inject with
// their plain trigger and keep the hold.
//
// A keepalive is NOT exempt here: it can park on the held list (behind an
// adopted delivering run, or behind parked keepalive siblings for FIFO) — its
// exemption is narrower and lives in injectHeld, which never lets pending
// work alone hold one.
//
// Everything else — foci send (sync and async), wakes, memory passes that run
// in place, webhooks, send_to_session, ask expiry and restore notices — keeps
// the hold.
func injectGateApplies(m *InjectMeta) bool {
	return m != nil && !m.UserInput && !isRedeliveryInject(m)
}

// isRedeliveryInject reports whether m is a redelivery (TriggerRedelivery):
// an input a dead backend never consumed, re-sent as a fresh turn (#2050).
func isRedeliveryInject(m *InjectMeta) bool {
	return m != nil && m.Trigger == TriggerRedelivery
}

// injectHeld is THE single decision "must this system inject wait now": the
// gate applies to it AND a hold reason that applies to IT is active. The hold
// has two halves (holdActive): the backend's whole background-work window,
// and an adopted delivering run. A keepalive is exempt from the FIRST only
// (#2233 ruling: a keepalive's whole purpose is keeping the prompt cache
// warm, so background work must never hold it) — but it parks behind an
// adopted run like every system inject, so it runs when the run ends rather
// than meeting the backend's turnActive refusal. A refusal the agent layer
// cannot see coming (the post-run grace, a run CC chains after this check)
// is RunInference's to absorb: it skips the keepalive (ErrKeepaliveSkipped)
// instead of retrying on the worker, so no keepalive can head-of-line-block
// user messages (#2233 review F1, N1). Re-evaluated right before every run, so the
// hold itself is unchanged wherever it is consulted. False during shutdown by
// design — the release path then hands the inject to runInject, whose drain
// gate refuses it (#2059), exactly as the blocking gate used to open on
// shutdown.
func (a *Agent) injectHeld(env Envelope) bool {
	if a.ShuttingDown() || !injectGateApplies(env.Inject) {
		return false
	}
	if isKeepaliveTrigger(env.Inject.Trigger) {
		return a.adoptedRunActive(env.SessionKey)
	}
	return a.holdActive(env.SessionKey)
}

// waitInjectGate blocks until sk is free of delivering work active or pending —
// an adopted autonomous run (IsInFlightDelivering, when the session worker is
// not itself busy) or the backend's whole background-work window
// (backendAwaitingAutonomousRun, spec §4). Running a gated inject during that
// window rebinds the shared session sink to the inject's NopSink and swallows
// the run's output (#1068). Returns false if ctx is cancelled while waiting
// (the caller should stop), true when the gate is open. The adoption and
// in-flight edges broadcast via InFlightWaitCh; a pending→run→clear transition
// has no channel, so it also polls at injectGatePollInterval.
//
// The worker never blocks here any more (#2233): the held-inject watcher
// (watchInjectGate) runs the per-injection entry (waitHeldGate) on its own
// goroutine, so a held inject cannot stop the session from serving later user
// messages.
func (a *Agent) waitInjectGate(ctx context.Context, sk string) bool {
	return a.gateWaitLoop(ctx, sk, "autonomous_or_pending", func() bool {
		return !a.ShuttingDown() && a.holdActive(sk)
	})
}

// waitHeldGate blocks until env's OWN hold clears — the half of the gate that
// applies to this injection (injectHeld): the full wait for a plain system
// inject (waitInjectGate — identical predicate for anything the gate parks),
// or just the adopted delivering run for a keepalive, so a parked keepalive
// runs the moment that run ends even while other background work is still
// outstanding (#2233 review F1).
func (a *Agent) waitHeldGate(ctx context.Context, env Envelope) bool {
	if env.Inject == nil || !isKeepaliveTrigger(env.Inject.Trigger) {
		return a.waitInjectGate(ctx, env.SessionKey)
	}
	return a.gateWaitLoop(ctx, env.SessionKey, "adopted_run", func() bool {
		return a.injectHeld(env)
	})
}

// gateWaitLoop is the one wait loop behind both entries: block until gated()
// clears, waking on the in-flight change channel, shutdown (which opens every
// gate — runInject then refuses the injection, #2059) and a poll backstop for
// the transitions nothing broadcasts.
func (a *Agent) gateWaitLoop(ctx context.Context, sk, reason string, gated func() bool) bool {
	if gated() {
		log.Extra("inbox", "gate_wait sk=%s reason=%s — holding injection until its hold clears (#1070/spec§4)", sk, reason)
	}
	for gated() {
		wait := a.InFlightWaitCh(sk)
		select {
		case <-ctx.Done():
			return false
		case <-wait:
			// Adoption / in-flight edge fired — re-check.
		case <-a.shutdown.done():
			// Draining — re-check (the gate opens).
		case <-time.After(a.gatePollInterval()):
			// Pending-work transitions have no broadcast — poll.
		}
	}
	return true
}

// gateWatch is the held-inject watcher: one gate wait per parked injection
// (waitHeldGate), each on its own goroutine so the worker stays free to serve
// the session channel while injects are held (#2233). open receives the FIRST
// completion (buffered and once-guarded, so every goroutine always exits);
// stop cancels the waits and releases them.
type gateWatch struct {
	open <-chan bool
	stop func()
}

// watchInjectGate starts a gateWatch over sk's held list on ctx. One wait runs
// per parked entry — the entry's OWN hold (waitHeldGate, keepalive half-split
// included) — and the first completion signals, so the worker wakes when ANY
// parked inject becomes releasable, not only the list's head: a keepalive
// parked behind a sibling held by pending work is released the moment its
// adopted run ends while the sibling stays parked (#2233 review F1).
//
// The result on open is a HINT, not a decision: an entry's hold can read open
// because the worker is momentarily busy (holdActive's busy refinement) while
// it is still active. The owner re-checks with injectHeld/releaseHeld after
// every signal and starts a fresh watch only after that re-check — the
// protocol that keeps the hint from spinning the loop.
func (a *Agent) watchInjectGate(ctx context.Context, held []Envelope) *gateWatch {
	wctx, cancel := context.WithCancel(ctx)
	ch := make(chan bool, 1)
	var signal sync.Once
	for _, env := range held {
		env := env
		go func() {
			if a.waitHeldGate(wctx, env) {
				signal.Do(func() { ch <- true })
			}
		}()
	}
	return &gateWatch{
		open: ch,
		stop: func() {
			cancel()
			// Drain the single result so the buffered send never outlives the
			// watch even when a wait was mid-return. A stale value is
			// unreachable: the watch is discarded after stop, and a re-armed
			// watch re-evaluates every entry's hold from scratch.
			select {
			case <-ch:
			default:
			}
		},
	}
}

// ensureHeldWatch makes sure the live watcher covers the CURRENT held list:
// it starts one if none is running (a fired-but-unconsumed watcher counts as
// live — its pending signal still wakes the worker's loop) and re-arms
// (stop + start) whenever an inject has parked past the generation the watch
// was armed for (heldGen). The re-arm is what keeps a newly parked entry's
// own hold waited on — a keepalive parking behind a pending-held sibling must
// wake when ITS adopted-run half clears, which the previous watch never
// covered (#2233 review F1). Stopping first loses nothing: a re-armed watch
// re-evaluates every entry immediately, so an entry that is still releasable
// signals at once. Called wherever the worker is when an inject parks.
// Worker-confined.
func (a *Agent) ensureHeldWatch(ctx context.Context, inb *sessionInbox) {
	if len(inb.held) == 0 {
		return
	}
	if inb.heldWatch != nil && inb.heldWatchGen == inb.heldGen {
		return
	}
	stopHeldWatch(inb)
	inb.heldWatch = a.watchInjectGate(ctx, inb.held)
	inb.heldWatchGen = inb.heldGen
}

// consumeHeldHint services one gate-watch signal inside waitTurnSlot's slot
// wait — the one watch-consuming select with no loop top to return to (the
// sessionWorker loop just stops its watch and iterates, letting the loop-top
// releaseHeld do this): the signal is only a HINT, so drop the stale watch,
// run held injects whose hold has truly cleared (releaseHeld re-checks the
// head right before it runs), and re-arm a watch for whatever stays parked —
// the protocol that keeps a spurious hint from spinning. Returns false when
// ctx ended mid-release (the caller must stop serving and unwind).
//
// waitTurnSlot MUST route its signals here (#2281's deadlock rule, second
// half): a turn holding a slot can be blocked in EnqueueInjectWait on this
// session's injection, so a held inject whose gate opens during the slot
// wait has to run THEN — inline, slot-free — not after the waiting batch
// finally gets its slot. Before it did, A(holding the last slot, blocked on
// a sync /send into B) + B(batch queued for a slot, inject parked) wedged
// forever: each waited for the other (#2233 review F1).
func (a *Agent) consumeHeldHint(ctx context.Context, inb *sessionInbox) bool {
	stopHeldWatch(inb)
	if !a.releaseHeld(ctx, inb) {
		return false
	}
	a.ensureHeldWatch(ctx, inb)
	return true
}

// stopHeldWatch tears the watcher down (draining its pending signal).
// Worker-confined; idempotent.
func stopHeldWatch(inb *sessionInbox) {
	if inb.heldWatch != nil {
		inb.heldWatch.stop()
		inb.heldWatch = nil
	}
}

// dispatchInject is the ONE path an injection takes when it leaves the queue —
// from the worker's dequeue (the channel or the head of its queue, #2362) and from
// waitTurnSlot's channel service. It returns false when ctx ended while
// waiting (the worker must stop; the envelope has been refused).
//
// Routing: a user-input injection (an ask answer, #2233) and a redelivery
// (#2362) wait only for what a user turn waits for (waitUserTurnGates:
// re-login, a non-delivering turn in flight, compaction) and are never held
// by the pending-work gate. A gated
// system inject parks on the inbox's held list when its OWN hold is active or
// a same-class sibling is already parked (heldSiblingPins — FIFO among
// injects held for the same reason must survive a gate that flickers open;
// a keepalive never queues behind a sibling parked by pending work alone,
// #2233 review F1). Anything else waits the re-login gate and runs.
//
// Injections never take a turn-limit slot (#2281) — a turn holding a slot can
// be blocked in EnqueueInjectWait on another session's injection, so counting
// them would deadlock at small limits.
func (a *Agent) dispatchInject(ctx context.Context, inb *sessionInbox, env Envelope) bool {
	if env.Inject.UserInput || isRedeliveryInject(env.Inject) {
		if !a.waitUserTurnGates(ctx, env.SessionKey) {
			a.refuseDroppedInject(env)
			return false
		}
		a.runInject(inb, env)
		return true
	}
	if injectGateApplies(env.Inject) && (a.injectHeld(env) || heldSiblingPins(inb.held, env)) {
		inb.held = append(inb.held, env)
		inb.heldGen++
		a.ensureHeldWatch(ctx, inb)
		return true
	}
	if !a.waitReloginGate(ctx, env.SessionKey) {
		a.refuseDroppedInject(env)
		return false
	}
	a.runInject(inb, env)
	return true
}

// heldSiblingPins reports whether an inject parked ahead of env (held: the
// held list at park time, or the entries a release walk has kept so far)
// pins env behind it. It is the ONE FIFO rule for both sides — dispatch
// (park) and releaseHeld (release) — so they cannot disagree about order
// (#2233 review F1: a release walk that consulted only injectHeld let B run
// past a same-class A kept a moment earlier, when the gate opened between
// the two checks). Only a SAME-CLASS sibling pins: two plain system
// injects wait on the same full hold, and two keepalives on the same
// adopted-run half, so queueing behind one's own class is what keeps FIFO
// among injects held for the same reason through a gate that flickers open
// between the sibling's park and env's arrival. A keepalive NEVER queues
// behind a plain sibling parked by pending work alone: that half of the hold
// does not apply to it, and queueing there would hold the cache-warming pass
// for the sibling's whole background-work window (#2233 review F1).
func heldSiblingPins(held []Envelope, env Envelope) bool {
	for _, h := range held {
		if isKeepaliveTrigger(h.Inject.Trigger) == isKeepaliveTrigger(env.Inject.Trigger) {
			return true
		}
	}
	return false
}

// releaseHeld runs held injects whose own hold has opened — at the top of
// every worker-loop iteration and from consumeHeldHint when a gate-watch
// signal arrives: the walk visits the list in arrival order and re-checks
// each entry right before it runs — its own hold (injectHeld) AND the same
// FIFO pin dispatch parks by (heldSiblingPins over the entries kept so far);
// an entry either one holds stays parked in place. Same-class entries
// therefore keep FIFO among themselves even when the gate opens mid-walk
// (an earlier entry kept, a later one of its class must stay behind it),
// while a keepalive whose
// adopted-run half cleared runs PAST a sibling still parked by pending work —
// exactly the exemption requirement 4 grants it (#2233 review F1). During
// shutdown injectHeld reads false, so each held inject is released into
// runInject, whose drain gate refuses it (#2059) — the same release the
// blocking gate used to perform.
//
// Returns false when ctx ended mid-release; nothing is refused here. Every
// caller unwinds to the sessionWorker's exit path, whose refuseHeld is the
// ONE owner of a held inject's refusal and fires it exactly once (#2233
// review F1: refusing the head here too double-fired EnqueueInjectWait's
// Refused — a close of a closed channel — and re-ran the wake refusal hook).
func (a *Agent) releaseHeld(ctx context.Context, inb *sessionInbox) bool {
	kept := make([]Envelope, 0, len(inb.held))
	ok := true
	for i, env := range inb.held {
		if a.injectHeld(env) || heldSiblingPins(kept, env) {
			kept = append(kept, env)
			continue
		}
		if !a.waitReloginGate(ctx, env.SessionKey) {
			// ctx ended mid-release: this entry and the rest stay on the
			// list for the worker's exit path — refuseHeld owns refusals.
			kept = append(kept, inb.held[i:]...)
			ok = false
			break
		}
		a.runInject(inb, env)
	}
	inb.held = kept
	if len(inb.held) == 0 {
		stopHeldWatch(inb)
	}
	return ok
}

// refuseHeld refuses every inject still on the held list — the worker is
// exiting (ctx ended or shutdown) and will never run them. It is the ONE
// owner of a held inject's refusal: releaseHeld leaves mid-release failures
// on the list precisely so this path fires each Refused exactly once (a
// second firing closes an already-closed channel in an EnqueueInjectWait
// caller, #2233 review F1). Without the refusal at all, that caller would
// wait out its own ctx for an envelope already dropped (#2059).
func (a *Agent) refuseHeld(inb *sessionInbox) {
	for _, env := range inb.held {
		a.refuseDroppedInject(env)
	}
	inb.held = nil
}

// refuseDroppedInject releases an injection's waiter when the worker must
// drop the envelope without running it — the inbox ctx ended while the
// injection was held in a gate. Refused exists for exactly this (#2059): an
// EnqueueInjectWait caller is released with ErrShuttingDown instead of
// waiting out its own ctx. User envelopes dropped at shutdown have no waiter
// and no equivalent hook; they are just dropped. No-op when Refused is unset.
func (a *Agent) refuseDroppedInject(env Envelope) {
	if env.Inject != nil && env.Inject.Refused != nil {
		env.Inject.Refused()
	}
}

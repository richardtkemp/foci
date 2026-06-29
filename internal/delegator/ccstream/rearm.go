package ccstream

import (
	"time"

	"foci/internal/delegator"
)

// reArmForContinuation keeps the current turn open for another ask() cycle
// instead of completing it. It re-initialises turn state via beginTurn with the
// SAME TurnEvents, so:
//   - OnTurnComplete is NOT fired (the caller returns early), which keeps the
//     agent-level in-flight refcount held — it releases only when
//     OrchestrateFullTurn returns after OnTurnComplete fires; and
//   - the same delivering sink carries the next ask()'s output.
//
// If followUp is non-empty it is sent as a fresh user message — the pre-answer
// re-dispatch case, which needs an explicit new ask(). If followUp is empty the
// caller has already written the continuation to CC's stdin (a folded
// steer/user inject) and we only re-arm.
//
// Returns true if the turn was re-armed and the caller MUST return early.
// Returns false only when a non-empty followUp failed to send: the turn is
// cancelled and the caller should fall through to normal completion so the
// first-round result is still delivered.
func (b *Backend) reArmForContinuation(turn *delegator.TurnEvents, followUp string) bool {
	b.beginTurn(turn, false) // re-arm: preserve shadow-turn signals of the live logical turn
	if followUp != "" {
		if err := b.writer.SendUser(followUp); err != nil {
			b.logger().Errorf("re-arm continuation: send user: %v", err)
			b.cancelTurn()
			return false
		}
	}
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	// Restart the idle clock; the continuation is an active turn, not done.
	b.touchActivity()
	return true
}

// markFoldedInject records that a mid-turn steer was just written to CC's
// stdin. CC emits an immediate (abort) result for the write and then re-inits
// and produces the real reply as a SEPARATE result. OnResult uses foldPending
// to re-arm the turn across the abort result so the reply lands in a live,
// delivering turn rather than an untracked shadow turn (#813).
//
// A set-once BOOL, deliberately NOT a counter: 2+ steers landing in the same
// inter-result gap are folded by CC into ONE re-init / ONE shadow result, so a
// counter over-counted and awaited phantom results (→ 45s watchdog). foldPending
// only has to gate the single abort result into a re-arm; how many shadow
// results actually follow is then read from CC's own `system init` stream
// (continuationExpected), not inferred from steer count. (Plain in-flight
// follow-ups are a candidate to share this — see the SourceUser branch in
// Inject — once their fold behaviour is verified.)
func (b *Backend) markFoldedInject() {
	b.turnMu.Lock()
	b.foldPending = true
	b.turnMu.Unlock()
}

// unmarkFoldedInject reverses markFoldedInject when the stdin write failed
// (no result will come for a message CC never received).
func (b *Backend) unmarkFoldedInject() {
	b.turnMu.Lock()
	b.foldPending = false
	b.turnMu.Unlock()
}

// defaultReArmWatchdogBound is how long a steer-driven re-armed turn waits in
// SILENCE (no CC stream activity) before the watchdog force-completes it. The
// happy path never reaches it — CC's shadow reply produces stream activity and
// then a second OnResult that completes the turn normally, which disarms the
// watchdog. The watchdog only fires when a folded steer produces NO shadow
// reply at all: it converts a potential turn that would otherwise hang until
// the orchestrator's 24h streamIdleTimeout into a bounded release. It is
// activity-aware (reschedules while CC is working) so a re-armed turn that is
// legitimately busy — e.g. waiting on a tool permission, which emits nothing in
// pipe mode — is NOT prematurely completed.
const defaultReArmWatchdogBound = 45 * time.Second

func (b *Backend) watchdogBound() time.Duration {
	if b.reArmWatchdogBound > 0 {
		return b.reArmWatchdogBound
	}
	return defaultReArmWatchdogBound
}

// outstandingPrompts returns the number of prompts (tool permissions,
// AskUserQuestion sequences, MCP elicitations) currently awaiting a user
// response, or 0 if none/unset. Used only by xtra:ccstream instrumentation: a
// re-armed turn blocked on a human emits no activity in pipe mode, so a
// watchdog fire or inflated rearm->result latency must be attributable to a
// pending prompt vs a genuinely slow/absent shadow reply before the latency
// distribution can inform the watchdog-bound decision (#813).
func (b *Backend) outstandingPrompts() int {
	if b.outstanding == nil {
		return 0
	}
	return b.outstanding.Len()
}

// armReArmWatchdog starts (or restarts) the re-arm safety net for the current
// turn generation. Called from OnResult immediately after a folded-steer
// re-arm. See defaultReArmWatchdogBound.
func (b *Backend) armReArmWatchdog() {
	b.turnMu.Lock()
	gen := b.turnGen
	b.reArmAt = time.Now()
	b.awaitingShadow = true
	if b.watchdog != nil {
		b.watchdog.Stop()
	}
	b.watchdog = time.AfterFunc(b.watchdogBound(), func() { b.watchdogTick(gen) })
	b.turnMu.Unlock()
}

// watchdogTick is the timer callback for the re-arm safety net. It no-ops if
// the turn it was armed for has already moved on (a newer turn began, or a
// completer claimed it). If CC has shown activity within the bound it reschedules
// (the turn is alive — keep waiting). Only sustained silence force-completes the
// turn, delivering the stashed round-1 result so a folded steer that produced no
// shadow reply still releases cleanly instead of hanging (#813).
func (b *Backend) watchdogTick(gen int) {
	// Snapshot outstanding-prompt count BEFORE taking turnMu: outstandingPrompts
	// acquires the registry lock, and not nesting it inside turnMu keeps the two
	// locks order-independent.
	pending := b.outstandingPrompts()

	b.turnMu.Lock()
	if b.completing || b.turnGen != gen || !b.turnActive {
		b.turnMu.Unlock()
		return // superseded by a real completion, a new turn, or already claimed
	}
	// A turn blocked on a human (tool permission, AskUserQuestion, MCP
	// elicitation) emits no stream events in pipe mode, so LastActivity goes
	// stale and the idle check below would force-complete a turn that is alive
	// and waiting correctly — not stalled. Reschedule instead: when the prompt
	// resolves, activity resumes and the turn either completes (disarming this
	// watchdog) or a later tick fires on genuine post-resolution silence. This
	// mirrors a non-folded turn's behaviour at a permission prompt (#813).
	if pending > 0 {
		b.watchdog = time.AfterFunc(b.watchdogBound(), func() { b.watchdogTick(gen) })
		b.turnMu.Unlock()
		b.logger().Extra("steer_shadow event=watchdog_defer reason=outstanding_prompt outstanding_prompts=%d", pending)
		return
	}
	if idle := time.Since(b.LastActivity()); idle < b.watchdogBound() {
		// CC is still active (recent stream events) — wait out the remainder.
		b.watchdog = time.AfterFunc(b.watchdogBound()-idle, func() { b.watchdogTick(gen) })
		b.turnMu.Unlock()
		return
	}
	// Sustained silence: claim and force-complete.
	b.completing = true
	turn := b.turnEvents
	resultCh := b.turnResultCh
	held := b.heldResult
	shadowReArmAt := b.reArmAt
	chainDepth := b.reArmDepth
	b.turnEvents = nil
	b.turnActive = false
	b.awaitingShadow = false
	b.foldPending = false
	b.continuationExpected = false
	b.sawFirstResult = false
	b.heldResult = nil
	b.reArmDepth = 0 // chain terminated by watchdog force-complete
	b.watchdog = nil
	b.turnMu.Unlock()

	if held == nil {
		held = &delegator.TurnResult{}
	}
	b.logger().Warnf("re-arm watchdog: folded steer produced no shadow reply within %s; force-completing turn to release it (#813)", b.watchdogBound())
	b.logger().Extra("steer_shadow event=watchdog outcome=no_shadow depth=%d rearm_to_fire=%s outstanding_prompts=%d", chainDepth, time.Since(shadowReArmAt).Round(time.Millisecond), b.outstandingPrompts())
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(held)
	}
	if b.typingFunc != nil {
		b.typingFunc(false)
	}
	if resultCh != nil {
		select {
		case resultCh <- &ResultMessage{Subtype: "rearm_watchdog_release"}:
		default:
		}
	}
}

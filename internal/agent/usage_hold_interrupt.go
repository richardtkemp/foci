package agent

import (
	"context"
	"errors"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/usagehold"
)

// Work running when a usage hold starts (#2491 requirement 3, stage c).
//
// When the account goes from open to hold or stop, interruptForUsageHold
// sorts every running turn of this agent:
//   - a turn registered through UsageHoldTurn (a background spawn's step, a
//     background-work (todo) turn) is interrupted; its owner sees the
//     interruption, pauses, and resumes at lift with the hidden resume line;
//   - the memory passes (reflection, consolidation, reset, compaction memory,
//     the batch purposes), keepalives and remind fires run to completion
//     (Dick 2026-11-02: "Allow the background turns to complete when the gate
//     hits, so do not resume them. Except for the background work on to-dos");
//   - an unregistered spawn or batch turn, or an unregistered background
//     turn, is stopped without a resume (its subagent-like work ends);
//   - every other turn is a chat turn: it is interrupted (the backend's
//     interrupt, as /stop sends, which also stops its subagents; background
//     shell commands go on) and its session gets the hidden resume line,
//     parked at the head of its held input so that at lift the session runs
//     ONE turn: the resume line, then the held messages in arrival order.
//
// A turn starting later, or a usage event during a remind fire, interrupts
// nothing: only the change from open interrupts.

// ErrUsageHoldInterrupted is the cause a UsageHoldTurn context is cancelled
// with when a usage hold starts.
var ErrUsageHoldInterrupted = errors.New("interrupted by the usage hold")

// usageHoldRunToCompletion are the triggers a hold start leaves running.
var usageHoldRunToCompletion = map[string]bool{
	TriggerKeepalive:                      true,
	"scheduled_wake":                      true,
	"reflection":                          true,
	delegator.BatchPurposeConsolidation:   true,
	"compaction_memory":                   true,
	"session_end_memory":                  true,
	"compaction-resume":                   true,
	delegator.BatchPurposeNudgeExtraction: true,
	delegator.BatchPurposeSummary:         true,
	delegator.BatchPurposePromptDiff:      true,
}

// usageHoldStopOnly are the triggers a hold start stops without a resume.
var usageHoldStopOnly = map[string]bool{
	"background":                         true, // only when no owner registered it (UsageHoldTurn)
	delegator.BatchPurposeSpawnRaw:       true,
	delegator.BatchPurposeSpawnCharacter: true,
	delegator.BatchPurposeSpawnExplore:   true,
}

// holdTurn is one turn a hold start interrupts through its context.
type holdTurn struct {
	key    string
	kind   string // "spawn" or "background": its entry in the paused set
	cancel context.CancelCauseFunc
	hit    bool // under usageHold.mu
}

// UsageHoldTurn registers a turn on key that a usage hold starting while it
// runs interrupts: the returned context is cancelled (cause
// ErrUsageHoldInterrupted), the backend turn on key is stopped, and key enters
// the paused set as kind at that moment (so the hold's compaction and
// keepalive see it at once). done ends the registration and reports whether
// the hold interrupted the turn; the caller then pauses (WaitUsageLift), or
// takes key out of the paused set when it will not, and resumes with
// UsageResumeLine. Used by background spawns and background-work turns.
func (a *Agent) UsageHoldTurn(ctx context.Context, key, kind string) (context.Context, func() (interrupted bool)) {
	tctx, cancel := context.WithCancelCause(ctx)
	ht := &holdTurn{key: key, kind: kind, cancel: cancel}
	a.usageHold.mu.Lock()
	if a.usageHold.turns == nil {
		a.usageHold.turns = map[*holdTurn]bool{}
	}
	a.usageHold.turns[ht] = true
	a.usageHold.mu.Unlock()
	return tctx, func() bool {
		a.usageHold.mu.Lock()
		delete(a.usageHold.turns, ht)
		hit := ht.hit
		a.usageHold.mu.Unlock()
		cancel(nil)
		return hit
	}
}

// UsageHoldActive reports whether the agent's account holds or stops now.
func (a *Agent) UsageHoldActive() bool { return a.usageHoldPhase() != usagehold.Open }

// WaitUsageLift blocks until the agent's account is open, or ctx ends
// (false).
func (a *Agent) WaitUsageLift(ctx context.Context) bool {
	for a.UsageHoldActive() {
		select {
		case <-ctx.Done():
			return false
		case <-a.usageHoldChanged():
		case <-time.After(a.gatePollInterval()):
			// Backstop: Tick (the keepalive tick) expires a window at its
			// reset; poll so a reset with no event still lifts this wait.
			a.usageRegistry().Tick(time.Now())
		}
	}
	return true
}

// UsageHoldPause records key as paused by the hold (kind "spawn" or
// "background") while on, so the keepalive warm set can include it (#2491
// requirement 9); off removes it.
func (a *Agent) UsageHoldPause(key, kind string, on bool) {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	if !on {
		delete(a.usageHold.paused, key)
		return
	}
	if a.usageHold.paused == nil {
		a.usageHold.paused = map[string]string{}
	}
	a.usageHold.paused[key] = kind
}

// UsageHoldPaused returns the sessions the hold paused, by kind.
func (a *Agent) UsageHoldPaused() map[string]string {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	out := make(map[string]string, len(a.usageHold.paused))
	for k, v := range a.usageHold.paused {
		out[k] = v
	}
	return out
}

// interruptForUsageHold interrupts the work running at a hold start (see the
// file comment). It runs in the goroutine of the phase change and is quick:
// it cancels contexts, sets resume marks and records the paused sessions
// before the agent's other reactions (the hold's compaction) run; only the
// backend interrupts, which write to a backend, go on in the background.
func (a *Agent) interruptForUsageHold() {
	var stops []string
	a.usageHold.mu.Lock()
	registered := map[string]bool{}
	var hit []*holdTurn
	for ht := range a.usageHold.turns {
		ht.hit = true
		registered[ht.key] = true
		hit = append(hit, ht)
		if a.usageHold.paused == nil {
			a.usageHold.paused = map[string]string{}
		}
		a.usageHold.paused[ht.key] = ht.kind
	}
	a.usageHold.mu.Unlock()
	for _, ht := range hit {
		a.logger().Infof("usage hold: interrupting %s %s (it pauses and resumes at lift)", ht.kind, ht.key)
		ht.cancel(ErrUsageHoldInterrupted)
		stops = append(stops, ht.key)
	}

	done := map[string]bool{}
	chat := func(key, what string) {
		done[key] = true
		a.logger().Infof("usage hold: interrupting the %s turn on %s; it resumes at lift", what, key)
		a.parkUsageResume(key)
		a.cancelInboxTurn(key)
		stops = append(stops, key)
	}
	// A user batch the inbox is running is a chat turn, whether or not it has
	// reached the turn registry yet.
	for _, key := range a.inboxTurnsRunning() {
		if !registered[key] && !a.IsCompacting(key) {
			chat(key, "user")
		}
	}
	for _, d := range a.ProcessingDetails() {
		key := d.SessionKey
		if key == "" || registered[key] || done[key] || usageHoldRunToCompletion[d.Trigger] || a.IsCompacting(key) {
			continue
		}
		if _, isSpawn := tools.SpawnOfSession(key); isSpawn || usageHoldStopOnly[d.Trigger] {
			done[key] = true
			a.logger().Infof("usage hold: stopping %s turn on %s (no resume)", d.Trigger, key)
			stops = append(stops, key)
			continue
		}
		chat(key, d.Trigger)
	}
	if len(stops) > 0 {
		go func() {
			for _, key := range stops {
				a.stopBackendTurn(key)
			}
		}()
	}
}

// inboxTurnsRunning lists the sessions whose inbox worker runs a user batch.
func (a *Agent) inboxTurnsRunning() []string {
	a.inboxesMu.Lock()
	inbs := make([]*sessionInbox, 0, len(a.inboxes))
	for _, inb := range a.inboxes {
		inbs = append(inbs, inb)
	}
	a.inboxesMu.Unlock()
	var out []string
	for _, inb := range inbs {
		inb.cancelMu.Lock()
		running := inb.turnCancel != nil
		inb.cancelMu.Unlock()
		if running {
			out = append(out, inb.sk)
		}
	}
	return out
}

// stopBackendTurn interrupts the backend turn on key, as /stop does.
func (a *Agent) stopBackendTurn(key string) {
	if a.DelegatedManager == nil {
		return
	}
	if err := a.DelegatedManager.StopSession(context.Background(), key); err != nil {
		a.logger().Debugf("usage hold: interrupt %s: %v", key, err)
	}
}

// cancelInboxTurn cancels the user batch the inbox worker of key is running,
// if any. Unlike CancelSession it never drops a batch waiting for a turn
// slot: that one waits for the lift instead.
func (a *Agent) cancelInboxTurn(key string) {
	a.inboxesMu.Lock()
	inb, ok := a.inboxes[key]
	a.inboxesMu.Unlock()
	if !ok {
		return
	}
	inb.cancelMu.Lock()
	cancel := inb.turnCancel
	inb.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// parkUsageResume marks key's turn as interrupted: its inbox worker parks
// the hidden resume line at the head of its held input (takeUsageResume) and
// stores it, so a restart keeps it too.
func (a *Agent) parkUsageResume(key string) {
	inb := a.pinInbox(key)
	inb.usageResume.Store(true)
	inb.evictMu.RUnlock()
	select {
	case inb.usageKick <- struct{}{}:
	default:
	}
}

// takeUsageResume turns the session's interrupted mark into its parked resume
// line. Worker-confined.
func (a *Agent) takeUsageResume(inb *sessionInbox) {
	if !inb.usageResume.Swap(false) {
		return
	}
	if a.UsageHeldDriver == nil {
		a.logger().Warnf("usage hold: no driver for the resume line of %s — it will not resume", inb.sk)
		return
	}
	a.parkUsageHeld(inb, usageResumeEnvelope(inb.sk, a.UsageHeldDriver(inb.sk)))
}

// usageResumeEnvelope is the hidden resume line of sk, as a user message
// driven by driver (Dick 2026-11-02: "simulate a user message saying \"resume
// what you were doing\""): it takes a user turn's trigger and via from
// driver's connection. Automated keeps it out of the typed-answer captures,
// SteerNever out of any fold.
func usageResumeEnvelope(sk string, driver Driver) Envelope {
	return Envelope{
		SessionKey:  sk,
		Text:        UsageResumeLine,
		ChatID:      session.ChatIDFromKey(sk),
		ReceivedAt:  time.Now(),
		Steer:       platform.SteerNever,
		Automated:   true,
		Driver:      driver,
		usageResume: true,
	}
}

// WaitTurnIdle blocks until no turn is in flight on sk, or ctx ends (false).
// It wakes on the in-flight signal itself (InFlightWaitCh), not on a timer.
func (a *Agent) WaitTurnIdle(ctx context.Context, sk string) bool {
	for {
		ch := a.InFlightWaitCh(sk)
		if !a.IsTurnInFlight(sk) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

package agent

import (
	"time"

	"foci/internal/config"
)

// Stay-busy inputs (#3112). The app tells a device, per conversation, whether
// the agent is waiting on something other than the user; the hub decides it
// (internal/app/waiting.go). The parts that live in the agent are read here:
// the threshold, the session's pending async results, and its input held by
// the usage hold. Each is read-only: nothing here changes how the notifier,
// the inbox or the usage hold behave.

// StayBusyWaitThreshold is behavior.stay_busy_wait_threshold, read live from
// the resolved snapshot (a config edit applies at the next read), else the
// default for a directly constructed agent.
func (a *Agent) StayBusyWaitThreshold() time.Duration {
	if a.LiveConfigFn != nil {
		return a.LiveConfigFn().Behavior.StayBusyWaitThresholdDuration()
	}
	return config.DefaultStayBusyWaitThreshold
}

// AsyncPendingSince returns when the session's most recently started pending
// async tool result began, and false when none is pending.
func (a *Agent) AsyncPendingSince(sessionKey string) (time.Time, bool) {
	return a.AsyncNotifier.YoungestPending(sessionKey)
}

// UsageHeldInput reports whether the usage hold holds input of the session
// now: a user message (or the resume line of a turn the hold cut) parked
// until the lift, or a system injection the hold parked.
func (a *Agent) UsageHeldInput(sessionKey string) bool {
	a.usageHold.mirrorMu.Lock()
	defer a.usageHold.mirrorMu.Unlock()
	return a.usageHold.heldBySession[sessionKey] > 0
}

// UsageHeldInputLift reports whether the usage hold holds input of
// sessionKey, and when its account lifts (zero = unknown) (#3256): the app
// shows such a chat as held, not as idle.
func (a *Agent) UsageHeldInputLift(sessionKey string) (held bool, liftAt time.Time) {
	if !a.UsageHeldInput(sessionKey) {
		return false, time.Time{}
	}
	return true, a.UsageHoldLiftAt(sessionKey)
}

// SetOnWaitInputsChanged registers fn to be called with a session key when
// one of that session's stay-busy inputs held here changes: its pending
// async results, or whether the usage hold holds its input. nil detaches.
func (a *Agent) SetOnWaitInputsChanged(fn func(sessionKey string)) {
	if fn == nil {
		a.waitInputsChanged.Store(nil)
	} else {
		a.waitInputsChanged.Store(&fn)
	}
	a.AsyncNotifier.SetOnChange(fn)
}

// syncUsageHeldMirror records how much of inb's input the usage hold holds,
// for UsageHeldInput, and reports a change of "any held" to the registered
// hook. Worker-confined callers (it reads inb's worker-owned lists).
func (a *Agent) syncUsageHeldMirror(inb *sessionInbox) {
	n := inb.usageHeldLen()
	var userInjects int32
	for _, env := range inb.held {
		// A user injection on the held list is there for the usage hold
		// alone (#3195).
		if userInject(env.Inject) {
			userInjects++
		}
		if env.usageHeldID != "" || userInject(env.Inject) {
			n++
		}
	}
	inb.userInjectsHeld.Store(userInjects)
	a.usageHold.mirrorMu.Lock()
	before := a.usageHold.heldBySession[inb.sk] > 0
	if n == 0 {
		delete(a.usageHold.heldBySession, inb.sk)
	} else {
		if a.usageHold.heldBySession == nil {
			a.usageHold.heldBySession = map[string]int{}
		}
		a.usageHold.heldBySession[inb.sk] = n
	}
	a.usageHold.mirrorMu.Unlock()
	if before != (n > 0) {
		if fn := a.waitInputsChanged.Load(); fn != nil {
			(*fn)(inb.sk)
		}
	}
}

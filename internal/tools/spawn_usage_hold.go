package tools

import (
	"context"
	"time"

	"foci/internal/turnevent"
)

// A background spawn in a usage hold (#2491 requirement 3, holdout H9).
//
// A step the hold interrupts pauses: the spawn stays "running" to its parent
// (no [SPAWN RESULT]), and its branch joins the keepalive warm set. At lift a spawn whose cache is still warm goes on with the
// hidden resume line; one whose cache went cold is not resumed (Dick
// 2026-11-02: "It was paused. There is nothing to resume.") and ends with one
// [SPAWN RESULT] saying so, so its parent does not wait for ever.
//
// What counts at lift is work in flight on the spawn's branch (#3143, Dick
// 2026-11-16, Q1: "only in-flight work counts"). A run that began on the
// branch while the spawn was paused (its backend woke by itself, Q3: "just
// allow it"; or an injection ran) takes the spawn over and reports its
// result. A turn in flight or input queued on the branch resumes the spawn
// even when cold: that work runs anyway. Only an idle cold spawn ends, and
// its process is closed (background work in it included).

// UsageResumeLine is the hidden input of a turn the usage hold interrupted,
// at lift (Dick 2026-11-02: "simulate a user message saying \"resume what you
// were doing\" - do not show this in the clients").
const UsageResumeLine = "resume what you were doing"

// spawnHoldColdText ends the [SPAWN RESULT] of a paused spawn gone cold.
const spawnHoldColdText = "was stopped by the usage hold and not resumed: its cache went cold while it was paused, so there is nothing to resume. Start a new spawn to go on."

// spawnStep is how one step of a background spawn ended.
type spawnStep struct {
	// resume: the hold paused the step and lifted with the cache warm; run
	// texts next.
	resume bool
	texts  []string
	err    error
	result string
	// cold: the hold paused the step and the cache went cold; the spawn ends.
	cold bool
	// adriftAnswer, with cold: the answer of a run that began and ended on
	// the branch while the spawn was paused.
	adriftAnswer string
	// handedOff: the hold paused the step, and at lift a run that began on
	// the branch meanwhile took the spawn over (#3143).
	handedOff *spawnAutonomousSink
}

// SpawnLiveWorkReporter is the optional agent side of the lift check
// (#3143): whether a session has work in flight, a turn running or input
// queued (held input included). *agent.Agent implements it.
type SpawnLiveWorkReporter interface {
	SessionLiveWork(sessionKey string) bool
}

// spawnHoldColdResult is the [SPAWN RESULT] of a paused spawn gone cold;
// answer is what a run on its branch ended with while it was paused.
func spawnHoldColdResult(head, answer string) string {
	msg := head + " " + spawnHoldColdText
	if answer != "" {
		msg += "\n\nWhile it was paused, its backend ran by itself (work it started in the background finished) and ended with:\n\n" + answer
	}
	return msg
}

// runSpawnStep runs texts on branchKey once, pausing across a usage hold (see
// the file comment). A step never starts while the account holds: one
// restored or resumed then waits for the lift first.
func (s *Spawner) runSpawnStep(agent SpawnAgent, pauser UsageHoldPauser, runCtx, spawnCtx context.Context, parent, id, branchKey string, texts []string) spawnStep {
	if pauser != nil && pauser.UsageHoldActive(branchKey) && !s.waitHoldLift(pauser, spawnCtx, parent, id, branchKey) {
		return spawnStep{err: context.Cause(spawnCtx)}
	}
	turnCtx, holdDone := runCtx, func() bool { return false }
	if pauser != nil {
		turnCtx, holdDone = pauser.UsageHoldTurn(runCtx, branchKey, "spawn")
	}
	tr, obs := s.deps.Tracker, s.deps.Observer
	buf := turnevent.NewBufferSink()
	sink := &spawnTextSink{BufferSink: buf, obs: obs, tracker: tr, parent: parent, id: id}
	// A stop cancels spawnCtx; a delegated turn does not watch it,
	// so interrupt the backend turn on the branch then (#2821).
	stopInterrupt := func() bool { return false }
	if in, ok := agent.(SpawnTurnInterrupter); ok {
		stopInterrupt = context.AfterFunc(spawnCtx, func() {
			spawnLog.Infof("spawn=%s branch=%s ended (%v): interrupting its turn", id, branchKey, context.Cause(spawnCtx))
			if err := in.InterruptTurn(branchKey); err != nil {
				spawnLog.Infof("spawn=%s branch=%s interrupt: %v", id, branchKey, err)
			}
		})
	}
	err := agent.HandleMessage(turnevent.WithSink(turnCtx, sink), branchKey, texts, nil)
	stopInterrupt()
	if err == nil && spawnCtx.Err() != nil {
		// The interrupted turn ended without an error of its own: the run
		// was still cut short, not completed.
		err = context.Cause(spawnCtx)
	}
	step := spawnStep{err: err, result: buf.FinalText()}
	if !holdDone() {
		return step
	}
	if spawnCtx.Err() != nil {
		pauser.UsageHoldPause(branchKey, "spawn", false) // stopped, not paused
		return step
	}
	if !s.waitHoldLift(pauser, spawnCtx, parent, id, branchKey) {
		return spawnStep{err: context.Cause(spawnCtx)}
	}
	if r := tr.handOff(parent, id); r != nil {
		spawnLog.Infof("spawn=%s branch=%s paused by the usage hold: a run began on its branch meanwhile and goes on", id, branchKey)
		return spawnStep{handedOff: r}
	}
	if s.cacheCold(agent, branchKey) {
		if !s.liveWork(agent, branchKey) {
			spawnLog.Infof("spawn=%s branch=%s paused by the usage hold went cold — not resumed", id, branchKey)
			return spawnStep{cold: true, adriftAnswer: tr.takeAdriftAnswer(parent, id)}
		}
		spawnLog.Infof("spawn=%s branch=%s paused by the usage hold went cold, but work is in flight on its branch (a turn or queued input) — resumed", id, branchKey)
	}
	spawnLog.Infof("spawn=%s branch=%s resumes after the usage hold", id, branchKey)
	tr.appendLog(parent, id, "message", "(resumed after the usage hold)")
	return spawnStep{resume: true, texts: append([]string{UsageResumeLine}, tr.drainPending(parent, id)...)}
}

// waitHoldLift pauses the spawn until the hold lifts, keeping its branch in
// the paused set meanwhile. False when a stop or shutdown ends spawnCtx first.
func (s *Spawner) waitHoldLift(pauser UsageHoldPauser, spawnCtx context.Context, parent, id, branchKey string) bool {
	spawnLog.Infof("spawn=%s branch=%s paused by the usage hold", id, branchKey)
	s.deps.Tracker.appendLog(parent, id, "message", "(paused by the usage hold)")
	pauser.UsageHoldPause(branchKey, "spawn", true)
	defer pauser.UsageHoldPause(branchKey, "spawn", false)
	return pauser.WaitUsageLift(spawnCtx, branchKey)
}

// liveWork reports whether branchKey has work in flight (a turn, queued
// input), by the agent's reading. An agent that cannot tell reports none.
func (s *Spawner) liveWork(agent SpawnAgent, branchKey string) bool {
	rep, ok := agent.(SpawnLiveWorkReporter)
	return ok && rep.SessionLiveWork(branchKey)
}

// cacheCold reports whether branchKey's prompt cache has expired, by the same
// reading spawn_send's cold rule uses. An agent that reports no cache reads
// as warm.
func (s *Spawner) cacheCold(agent SpawnAgent, branchKey string) bool {
	rep, ok := agent.(SpawnCacheReporter)
	if !ok {
		return false
	}
	now := time.Now()
	return !rep.CacheExpiry(branchKey, now).After(now)
}

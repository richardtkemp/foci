package tools

import (
	"sync"
	"time"
)

// A background spawn whose turn ends while work it started in the background
// still runs (#2728). On a delegated backend that work is a background shell
// command or a subagent (the backend wakes the spawn's branch when it ends,
// #2729); on any agent it is foci's own background work on the branch (a
// background tool call; not a spawn of its own, which #3163 lets run on and
// wake it when it reports). The text the turn ended with is
// interim ("waiting for the suite"), not the spawn's answer.
//
// Dick 2026-11-21 12:18 ("Keep it running"): "No [SPAWN RESULT] yet: the
// spawn stays 'running' in the app and in spawn_status until its background
// work ends and it hands back for real. Its interim text is not delivered as
// a result."
//
// So such a spawn stays running, with its turn's text kept in a spawnAwait,
// and a watcher (Spawner.awaitBackground) waits for the work:
//   - the backend wakes on the branch (SpawnBranchTurn): that run takes the
//     spawn over (claimFromWait) and reports its result, or waits again;
//   - a spawn_send or a report from its own spawn: a new turn takes it over
//     (restart, takeWake), as for a finished spawn;
//   - spawn_stop or the user's stop: the branch's backend is closed at once,
//     which ends the work, and the result says it was stopped;
//   - the work ends and nothing woke the spawn (its process closed, the
//     work was killed): the kept text is delivered, with a note saying the
//     work ended with no answer after it (Dick 2026-11-01 16:23: an orphaned
//     result "gets a closing result from foci saying the work ended with no
//     answer").
// A foci restart ends the work; the record is still running, so the next
// process resumes the spawn (#2589), which answers then.

// spawnAwaitPoll is how often the watcher re-checks the branch's background
// work. Background-work transitions have no channel (as for the deferred
// compaction bounce, defaultDeferredBouncePoll).
const spawnAwaitPoll = 2 * time.Second

// spawnAwaitEndedNote follows the kept text of a spawn whose background work
// ended without it answering again.
const spawnAwaitEndedNote = "(foci: this spawn ended its turn while work it had started in the background still ran. " +
	"That work has ended and the spawn did not answer again, so the text above, written before the work ended, is its last.)"

// spawnAwait is a spawn's wait on its background work, held in the tracker
// (trackedSpawn.awaiting) under its lock.
type spawnAwait struct {
	answer string // the text its turn ended with
	work   string // the work it waits on, as last seen

	stopCh   chan struct{} // closed by stop
	stopOnce sync.Once
	done     chan struct{} // closed once a run takes the spawn over
}

func newSpawnAwait(answer, work string) *spawnAwait {
	return &spawnAwait{answer: answer, work: work, stopCh: make(chan struct{}), done: make(chan struct{})}
}

// stop is the spawn's cancel while it waits: spawn_stop and the user's stop
// call it under the tracker's lock, so it only signals the watcher.
func (w *spawnAwait) stop() { w.stopOnce.Do(func() { close(w.stopCh) }) }

// takeAwaitLocked ends s's wait on its background work, when it has one, for
// a run that takes the spawn over; reports whether it had one. Caller holds
// the tracker's lock.
func (s *trackedSpawn) takeAwaitLocked() bool {
	w := s.awaiting
	if w == nil {
		return false
	}
	s.awaiting = nil
	close(w.done)
	return true
}

// hasLiveChildren reports whether a spawn started from branchKey still runs
// or holds a result it has not delivered.
func (t *SpawnTracker) hasLiveChildren(branchKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveChildrenLocked(branchKey)
}

// setAwaitWork records the work parent's spawn id waits on, while w is its
// wait.
func (t *SpawnTracker) setAwaitWork(parent, id string, w *spawnAwait, work string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil && s.awaiting == w {
		w.work = work
	}
}

// endAwait ends w, parent's spawn id's wait, and marks the spawn finished as
// finishOrNext does, reporting whether the spawn was stopped. ok is false
// when w is no longer its wait (a run took the spawn over, or it is gone),
// or foci is shutting down: the record then stays running for the next
// process to resume. busy (ok false) says a report from one of its own
// spawns is on its way to resume it (wakeSpawn waits for a slot before it
// takes the wait over): the wait goes on.
func (t *SpawnTracker) endAwait(parent, id string, w *spawnAwait) (ok, stopped, busy bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || s.awaiting != w || t.shuttingDown {
		return false, false, false
	}
	if !s.stopped && s.waking {
		return false, false, true
	}
	s.awaiting = nil
	s.pending = nil // none: queue refuses a waiting spawn
	s.running = false
	s.reporting = true
	s.ended = time.Now()
	s.adriftAnswer = ""
	t.pruneFinished(parent)
	return true, s.stopped, false
}

// backgroundWork describes the work running in the background on running's
// branch, or "" when there is none: the backend's (SpawnDeps.BackgroundWork)
// and foci's own pending background results for the branch.
func (s *Spawner) backgroundWork(running RunningSpawn) string {
	if running.BranchKey == "" || running.Direct {
		return ""
	}
	h := s.hostOf(running)
	if h == nil {
		return "" // its agent is gone, and its work with it
	}
	var parts []string
	if h.deps.BackgroundWork != nil {
		if w := h.deps.BackgroundWork(running.BranchKey); w != "" {
			parts = append(parts, w)
		}
	}
	// foci's pending results for the branch also count its own spawns,
	// which #3163 lets run on after it ends (they wake it when they
	// report): only pending results with no spawn of its own live are
	// foci background work (a background tool call). With both, the tool
	// call is not told apart and does not hold the result.
	if h.deps.Notifier.HasPending(running.BranchKey) && !s.deps.Tracker.hasLiveChildren(running.BranchKey) {
		parts = append(parts, "a foci background tool call")
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return parts[0] + " | " + parts[1]
}

// startAwait starts the wait of parent's spawn running on its background work
// (#2728), after finishRun kept the spawn running in w. The spawner is marked
// as waiting on the spawn's result until the wait ends.
func (s *Spawner) startAwait(parent string, running RunningSpawn, w *spawnAwait) {
	spawnLog.Infof("spawn=%s branch=%s parent=%s: its turn ended while work it started in the background runs (%s); its result waits for that work", running.ID, running.BranchKey, parent, w.work)
	s.deps.Tracker.appendLog(parent, running.ID, "message", "(its turn ended while work it started in the background runs: "+w.work+"; its result waits for that work to end)")
	s.deps.Notifier.MarkPending(parent)
	go s.awaitBackground(parent, running, w)
}

// awaitBackground is the watcher of w: see the top of this file.
func (s *Spawner) awaitBackground(parent string, running RunningSpawn, w *spawnAwait) {
	defer s.deps.Notifier.MarkDone(parent)
	poll := s.awaitPoll
	if poll <= 0 {
		poll = spawnAwaitPoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.stopCh:
			// Stopping a waiting spawn stops its work (Dick 2026-11-01).
			s.closeBranchNow(running)
			s.finishAwait(parent, running, w)
			return
		case <-t.C:
			if work := s.backgroundWork(running); work != "" {
				s.deps.Tracker.setAwaitWork(parent, running.ID, w, work)
				continue
			}
			if !s.finishAwait(parent, running, w) {
				return
			}
		}
	}
}

// finishAwait ends parent's spawn's wait w and delivers its result: stopped,
// or the text its turn ended with and spawnAwaitEndedNote. Nothing when a
// run took the spawn over meanwhile. It reports whether the wait goes on
// (endAwait's busy).
func (s *Spawner) finishAwait(parent string, running RunningSpawn, w *spawnAwait) (goesOn bool) {
	tr, id := s.deps.Tracker, running.ID
	ok, stopped, busy := tr.endAwait(parent, id, w)
	if !ok {
		return busy
	}
	if stopped {
		tr.setOutcome(parent, id, "stopped")
	} else {
		tr.setOutcome(parent, id, "completed")
		spawnLog.Infof("spawn=%s branch=%s: its background work ended and it did not answer again; its last text is its result", id, running.BranchKey)
		tr.appendLog(parent, id, "message", "(its background work ended with no answer after it)")
	}
	if s.deps.Observer != nil {
		s.deps.Observer.SpawnEnded(parent, id)
	}
	answer := ""
	if !stopped {
		answer = w.answer
		if answer == "" {
			answer = "(empty response)"
		}
		answer += "\n\n" + spawnAwaitEndedNote
	}
	s.deliver(parent, id, s.resultMessage(parent, id, running.BranchKey, stopped, nil, answer, 0))
	s.startWake(parent, id)
	if !stopped {
		s.closeBranch(running)
	}
	return false
}

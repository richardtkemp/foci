package tools

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/session"
	"foci/internal/turnevent"
)

// A spawn's own turns on its branch are foci's: runBackground runs them and
// collects their text for the [SPAWN RESULT]. A delegated backend (Claude
// Code) can also start a run on the branch by itself, after that turn ended:
// a background task the spawn started (a Bash command, a subagent) finished,
// and the backend wakes to react to it. The agent adopts such a run as an
// autonomous turn (Agent.OpenAutonomousTurn), whose sink would deliver its
// text to the session's chat. A spawn's branch has no chat of its own, so the
// app put it in the human's default conversation (#2729). The spawn's spawner
// is the one waiting for that text.

// spawnWokeNote is the activity-log and app-entry line for a run the spawn's
// backend started by itself.
const spawnWokeNote = "(woke on its own: work it started in the background finished)"

// SpawnBranchTurn claims an autonomous run on a background spawn's branch
// (#2729). ok is false when branchKey is not a tracked background spawn's
// branch: the run is the agent's to deliver as usual. Otherwise the run must
// not reach any chat; sink is where its events go. stop interrupts the run
// (spawn_stop and /stop call it while the run is the spawn's).
//
// The sink reports the run to the spawn's owner exactly as a resumed run: on
// TurnStart it marks the spawn running again, and on TurnComplete it ends
// that run and delivers one [SPAWN RESULT] with the run's answer. When the
// spawn is already running (its own run, which this run then belongs to), the
// run's text goes to its activity log only.
func SpawnBranchTurn(branchKey string, stop func()) (sink turnevent.Sink, ok bool) {
	host := spawnerFor(session.AgentIDFromAnyKey(branchKey))
	if host == nil {
		return nil, false
	}
	parent, running, found := host.deps.Tracker.ForBranch(branchKey)
	if !found || running.Direct {
		return nil, false
	}
	// The spawner that tracks the spawn delivers its results: the caller's
	// own for a spawn run as another agent (#2662).
	s := spawnerFor(session.AgentIDFromAnyKey(parent))
	if s == nil {
		s = host
	}
	return s.autonomousSink(parent, running, stop), true
}

// autonomousSink is SpawnBranchTurn's sink for a spawn of s.
func (s *Spawner) autonomousSink(parent string, running RunningSpawn, stop func()) *spawnAutonomousSink {
	tr := s.deps.Tracker
	return &spawnAutonomousSink{
		spawnTextSink: &spawnTextSink{BufferSink: turnevent.NewBufferSink(), obs: s.deps.Observer, tracker: tr, parent: parent, id: running.ID},
		s:             s,
		running:       running,
		stop:          stop,
	}
}

// spawnAutonomousSink is the sink of a run a spawn's backend started by
// itself, or of any other delivering turn on the spawn's branch that the
// spawn did not start (an injection, #3143). It never delivers to a platform.
type spawnAutonomousSink struct {
	*spawnTextSink
	s       *Spawner
	running RunningSpawn
	stop    func()
	// owned: this run reports the spawn's result. TurnStart found the spawn
	// finished and marked it running for this run, or the spawn's own run
	// ended while this one went and handed the spawn over (#3143). Written
	// under the tracker's lock.
	owned atomic.Bool

	// pendMu orders markPending and markDone, so the spawner's pending mark
	// this run holds is taken at most once and always given back.
	pendMu   sync.Mutex
	pending  bool
	finished bool
}

func (a *spawnAutonomousSink) Emit(ctx context.Context, ev turnevent.Event) {
	if _, ok := ev.(turnevent.TurnStart); ok {
		a.claim()
	}
	a.spawnTextSink.Emit(ctx, ev)
	if tc, ok := ev.(turnevent.TurnComplete); ok {
		a.finish(tc)
	}
}

// stopFunc is the run's stop, never nil.
func (a *spawnAutonomousSink) stopFunc() func() {
	if a.stop == nil {
		return func() {}
	}
	return a.stop
}

// markPending marks the spawner as waiting on this run's result, unless the
// run has already finished.
func (a *spawnAutonomousSink) markPending() {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	if a.pending || a.finished {
		return
	}
	a.pending = true
	a.s.deps.Notifier.MarkPending(a.parent)
}

// markDone gives back the mark markPending took, once the run has finished.
func (a *spawnAutonomousSink) markDone() {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	a.finished = true
	if a.pending {
		a.pending = false
		a.s.deps.Notifier.MarkDone(a.parent)
	}
}

// claim marks the spawn running for this run, when it is not running already.
// When it is, the run goes adrift: it may take the spawn over when the
// spawn's own run ends first (#3143).
func (a *spawnAutonomousSink) claim() {
	s, tr, parent, id := a.s, a.s.deps.Tracker, a.parent, a.running.ID
	switch tr.claimRun(parent, id, a) {
	case claimAdrift:
		spawnLog.Infof("spawn=%s branch=%s: a run it did not start began on its branch while the spawn runs; the run's text goes to its activity log", id, a.running.BranchKey)
		tr.appendLog(parent, id, "message", spawnWokeNote)
		return
	case claimFromWait:
		// #2728: the spawn waited on its background work, and is still
		// shown running; this run reports its result.
		spawnLog.Infof("spawn=%s branch=%s parent=%s: its backend woke on its background work; the run reports the spawn's result", id, a.running.BranchKey, parent)
		tr.appendLog(parent, id, "message", spawnWokeNote)
		a.markPending()
		if s.deps.Observer != nil {
			s.deps.Observer.SpawnPrompt(parent, id, spawnWokeNote)
		}
		return
	}
	spawnLog.Infof("spawn=%s branch=%s parent=%s: a run it did not start began on its branch; it reports to the spawner", id, a.running.BranchKey, parent)
	tr.appendLog(parent, id, "message", spawnWokeNote)
	a.markPending()
	if s.deps.Observer != nil {
		shown := a.running
		if info, ok := tr.spawnInfo(parent, id); ok {
			shown = info // names the model, when an earlier run learned it (#2879)
		}
		s.deps.Observer.SpawnStarted(parent, shown, spawnWokeNote)
	}
}

// tookOver is called for a run the spawn was handed over to (#3143): the
// spawner waits on its result now.
func (a *spawnAutonomousSink) tookOver() {
	spawnLog.Infof("spawn=%s branch=%s: its run ended while a run it did not start goes on its branch; that run takes the spawn over and reports its result", a.running.ID, a.running.BranchKey)
	a.tracker.appendLog(a.parent, a.running.ID, "message", "(a run that began on its branch took the spawn over)")
	a.markPending()
}

// finish ends the run this sink claimed and delivers its [SPAWN RESULT]. When
// spawn_send messages are waiting that the run did not take, they run as a
// follow-up turn on the branch, which reports one more result.
func (a *spawnAutonomousSink) finish(tc turnevent.TurnComplete) {
	s, tr, parent, id, branchKey := a.s, a.s.deps.Tracker, a.parent, a.running.ID, a.running.BranchKey
	if !tr.releaseRun(parent, id, a, a.adriftAnswer()) {
		return
	}
	defer a.markDone()
	work := ""
	if tc.Err == nil {
		work = s.backgroundWork(a.running)
	}
	end := tr.finishRun(parent, id, tc.Err != nil, work, a.answer())
	if end.awaiting != nil {
		// It ended its turn again with work still running (#2728).
		s.startAwait(parent, a.running, end.awaiting)
		return
	}
	if end.interrupted {
		// foci is shutting down: the persisted record stays running and the
		// next process resumes the spawn (#2589).
		return
	}
	if end.handedOff != nil {
		// Another run on the branch goes on and reports the next result.
		if !promptReaped(tc.Err) {
			s.deliver(parent, id, s.resultMessage(parent, id, branchKey, false, tc.Err, a.answer(), 0))
		}
		end.handedOff.tookOver()
		return
	}
	if !end.done {
		s.deliver(parent, id, s.resultMessage(parent, id, branchKey, false, nil, a.answer(), 0))
		go s.followUp(parent, a.running, end.next)
		return
	}
	switch {
	case end.stopped:
		tr.setOutcome(parent, id, "stopped")
	case tc.Err != nil:
		tr.setOutcome(parent, id, "failed")
		tr.appendLog(parent, id, "error", tc.Err.Error())
	default:
		tr.setOutcome(parent, id, "completed")
	}
	if s.deps.Observer != nil {
		s.deps.Observer.SpawnEnded(parent, id)
	}
	if !end.stopped && promptReaped(tc.Err) {
		s.endQuietly(parent, id, branchKey)
		return
	}
	s.deliver(parent, id, s.resultMessage(parent, id, branchKey, end.stopped, tc.Err, a.answer(), end.dropped))
}

// claimHow is how claimRun took a run on a spawn's branch.
type claimHow int

const (
	claimAdrift   claimHow = iota // the spawn runs (or is unknown): r is adrift, not its owner
	claimFinished                 // the spawn was finished: marked running, r owns it
	claimFromWait                 // the spawn waited on its background work (#2728): r owns it, the wait ends
)

// claimRun marks parent's spawn id running for r's run when it is finished,
// or hands it a spawn that waits on its background work (#2728); r owns it
// then. It adds r to its adrift runs when it runs already.
func (t *SpawnTracker) claimRun(parent, id string, r *spawnAutonomousSink) claimHow {
	if t == nil {
		return claimAdrift
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return claimAdrift
	}
	if s.running && s.awaiting != nil && !s.stopped {
		s.takeAwaitLocked()
		s.cancel = r.stopFunc()
		r.owned.Store(true)
		t.mu.Unlock()
		return claimFromWait
	}
	if s.running {
		s.adrift = append(s.adrift, r)
		t.mu.Unlock()
		return claimAdrift
	}
	s.running, s.stopped, s.cancel = true, false, r.stopFunc()
	s.ended, s.outcome = time.Time{}, ""
	r.owned.Store(true)
	t.mu.Unlock()
	t.persist(parent, id)
	return claimFinished
}

// releaseRun is called when r's run ends. It reports whether r owns the
// spawn; one that does not leaves its adrift runs, and its answer is kept
// (adriftAnswer).
func (t *SpawnTracker) releaseRun(parent, id string, r *spawnAutonomousSink, answer string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.owned.Load() {
		return true
	}
	s := t.spawns[parent][id]
	if s == nil {
		return false
	}
	s.adrift = slices.DeleteFunc(s.adrift, func(x *spawnAutonomousSink) bool { return x == r })
	if answer != "" {
		s.adriftAnswer = answer
	}
	return false
}

// handOff hands parent's running spawn id over to its oldest adrift run, if
// it has one and was not stopped, and returns that run (nil: none).
func (t *SpawnTracker) handOff(parent, id string) *spawnAutonomousSink {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	var r *spawnAutonomousSink
	if s != nil && s.running {
		r = s.handOffLocked()
	}
	t.mu.Unlock()
	return r
}

// handOffLocked makes the oldest adrift run the spawn's owner, when the
// spawn was not stopped: the spawn's stop then stops that run. Caller holds
// the tracker's lock.
func (s *trackedSpawn) handOffLocked() *spawnAutonomousSink {
	if s.stopped || len(s.adrift) == 0 {
		return nil
	}
	r := s.adrift[0]
	s.adrift = s.adrift[1:]
	r.owned.Store(true)
	s.cancel = r.stopFunc()
	s.adriftAnswer = ""
	return r
}

// takeAdriftAnswer returns and clears the answer of the latest adrift run of
// parent's spawn id that ended unowned.
func (t *SpawnTracker) takeAdriftAnswer(parent, id string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return ""
	}
	out := s.adriftAnswer
	s.adriftAnswer = ""
	return out
}

// errSpawnFollowUp fails a follow-up turn that cannot start.
var errSpawnFollowUp = errors.New("the follow-up turn for its waiting messages could not start: agent not available")

// followUp runs texts, parent messages an autonomous run did not take, as a
// new turn on the spawn's branch, as runBackground runs them after one of its
// own turns. The spawn is still marked running.
func (s *Spawner) followUp(parent string, running RunningSpawn, texts []string) {
	tr, id := s.deps.Tracker, running.ID
	spawnCtx, cancel := s.runContext(context.Background(), running, true)
	tr.setCancel(parent, id, cancel)
	agent := s.agentOf(running)
	err := errSpawnFollowUp
	if agent != nil && s.deps.Notifier != nil {
		err = s.slots.acquire(spawnCtx) // a stop while it waits ends the wait
	}
	if err != nil {
		cancel()
		end := tr.finishOrNext(parent, id, true)
		if end.handedOff != nil {
			s.deliver(parent, id, s.resultMessage(parent, id, running.BranchKey, false, err, "", len(texts)))
			end.handedOff.tookOver()
			return
		}
		stopped := end.stopped
		if stopped {
			tr.setOutcome(parent, id, "stopped")
		} else {
			tr.setOutcome(parent, id, "failed")
			tr.appendLog(parent, id, "error", err.Error())
		}
		if s.deps.Observer != nil {
			s.deps.Observer.SpawnEnded(parent, id)
		}
		s.deliver(parent, id, s.resultMessage(parent, id, running.BranchKey, stopped, err, "", len(texts)))
		return
	}
	for _, t := range texts {
		tr.appendLog(parent, id, "message", "(follow-up turn) "+t)
	}
	_, _ = s.runBackground(context.Background(), agent, parent, running, texts, spawnCtx, cancel, "")
}

package tools

import (
	"context"
	"errors"
	"sync/atomic"

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
// itself. It never delivers to a platform.
type spawnAutonomousSink struct {
	*spawnTextSink
	s       *Spawner
	running RunningSpawn
	stop    func()
	// owned: TurnStart found the spawn finished and marked it running for
	// this run, so this run reports its result.
	owned atomic.Bool
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

// claim marks the spawn running for this run, when it is not running already.
func (a *spawnAutonomousSink) claim() {
	s, tr, parent, id := a.s, a.s.deps.Tracker, a.parent, a.running.ID
	stop := a.stop
	if stop == nil {
		stop = func() {}
	}
	if !tr.restart(parent, id, stop) {
		spawnLog.Infof("spawn=%s branch=%s: its backend started a run while the spawn runs; the run's text goes to its activity log", id, a.running.BranchKey)
		tr.appendLog(parent, id, "message", spawnWokeNote)
		return
	}
	a.owned.Store(true)
	spawnLog.Infof("spawn=%s branch=%s parent=%s: its backend started a run by itself; it reports to the spawner", id, a.running.BranchKey, parent)
	tr.appendLog(parent, id, "message", spawnWokeNote)
	s.deps.Notifier.MarkPending(parent)
	if s.deps.Observer != nil {
		shown := a.running
		if info, ok := tr.spawnInfo(parent, id); ok {
			shown = info // names the model, when an earlier run learned it (#2879)
		}
		s.deps.Observer.SpawnStarted(parent, shown, spawnWokeNote)
	}
}

// finish ends the run this sink claimed and delivers its [SPAWN RESULT]. When
// spawn_send messages are waiting that the run did not take, they run as a
// follow-up turn on the branch, which reports one more result.
func (a *spawnAutonomousSink) finish(tc turnevent.TurnComplete) {
	if !a.owned.Load() {
		return
	}
	s, tr, parent, id, branchKey := a.s, a.s.deps.Tracker, a.parent, a.running.ID, a.running.BranchKey
	defer s.deps.Notifier.MarkDone(parent)
	next, done, stopped, interrupted, dropped := tr.finishOrNext(parent, id, tc.Err != nil)
	if interrupted {
		// foci is shutting down: the persisted record stays running and the
		// next process resumes the spawn (#2589).
		return
	}
	if !done {
		s.deliver(parent, id, s.resultMessage(parent, id, branchKey, false, nil, a.FinalText(), 0))
		go s.followUp(parent, a.running, next)
		return
	}
	switch {
	case stopped:
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
	s.deliver(parent, id, s.resultMessage(parent, id, branchKey, stopped, tc.Err, a.FinalText(), dropped))
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
		_, _, stopped, _, _ := tr.finishOrNext(parent, id, true)
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

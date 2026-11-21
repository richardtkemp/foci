package tools

// A spawn's result or message to a parent that is itself a background spawn
// (#3163, Dick 2026-11-18 13:13: "wake the parent").
//
// A background spawn's session is a branch that only its run reads. A turn
// injected there runs after the run's turn, or after the run has ended, and
// its output has no chat to go to ("no connection ... response not
// delivered"), so whatever a child sent was lost. While the parent's run went
// on, those injections also queued in the inbox behind the run's protected
// turn, and a burst of children overflowed it (inboxChanSize).
//
// So a child's [SPAWN RESULT] or send_to_spawner message to a background
// spawn's branch is that spawn's own input, as a spawn_send is:
//   - its run goes on: folded into its running turn where the backend can
//     (SpawnSteerer), else queued for its next step (the tracker's pending
//     list, which has no cap and is persisted), which runs as a follow-up
//     turn before its result;
//   - its run has ended: it is resumed on its branch with the text, and its
//     next answer goes to ITS spawner as a new [SPAWN RESULT]. The texts
//     wait in its wake queue, which is persisted with its record (Dick
//     2026-11-18: "Persist the queue"), so a restart resumes it with them;
//   - it was stopped (spawn_stop or the user's stop): it is not woken; the
//     text goes on to the stopped spawn's own spawner, with a note naming the
//     child and the stopped spawn (Dick 2026-11-18 14:53: "Don't wake; pass
//     up").
// Many children reporting after the parent ended wake it once: the first
// resumes it, and the rest arrive while it runs, so they fold or queue into
// that run and its one result.
//
// A finished spawn stays known, and persisted, while any of its own spawns
// runs or holds a result it has not delivered (Dick 2026-11-18 14:53, "Fix in
// #3163"): pruneFinished skips it, and record keeps its row.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/session"
)

// spawnWakeNote heads the input a spawn is resumed with when a spawn it
// started reports after its run ended.
const spawnWakeNote = "[SYSTEM: RESUMED BY YOUR SPAWN] Your run had ended, and a spawn you started has since sent you what follows, " +
	"so you are resumed with it. Act on it as your task needs. Your final answer goes to your spawner as a new result."

// spawnWakeRetries bounds how long (in 10ms passes) a wake waits for a spawn
// being stopped to end its run.
const spawnWakeRetries = 100

// maxPassUp bounds how many stopped spawns a child's report passes up
// through: a spawn tree is at most a few levels deep.
const maxPassUp = 16

// childReport is a child spawn's [SPAWN RESULT] or send_to_spawner message
// on its way to its spawner. It is persisted in the spawner spawn's wake
// queue (spawnRecord.Wake).
type childReport struct {
	// Kind is "result" or "message".
	Kind string `json:"kind"`
	// Child names the child: its label and id.
	Child string `json:"child"`
	// Text is the report as a spawn takes it as input.
	Text string `json:"text"`
	// Chat is a message's form for a chat session (formatSpawnerMessage);
	// a result goes as Text.
	Chat string `json:"chat,omitempty"`
	// From, Raw and ChildName make a message's AgentOrigin (the app chit).
	From      string `json:"from,omitempty"`
	Raw       string `json:"raw,omitempty"`
	ChildName string `json:"child_name,omitempty"`
	// Via holds the notes of the stopped spawns it passed up through,
	// newest first; it heads what is delivered.
	Via string `json:"via,omitempty"`
	// taken, when set, is called once the report is safe with its
	// receiver: a result's hook that clears it from the child's record.
	taken func()
}

// input is the report as a spawn takes it.
func (r childReport) input() string { return r.Via + r.Text }

// done calls the report's taken hook, once.
func (r *childReport) done() {
	if r.taken != nil {
		r.taken()
		r.taken = nil
	}
}

// resultReport is the report of parent's spawn id's result msg.
func (s *Spawner) resultReport(parent, id, msg string) childReport {
	info, _ := s.deps.Tracker.spawnInfo(parent, id)
	tr := s.deps.Tracker
	return childReport{Kind: "result", Child: fmt.Sprintf("%s (%s)", info.Label(), id), Text: msg,
		taken: func() { tr.clearResult(parent, id, msg) }}
}

// messageReport is the report of a send_to_spawner message from the spawn
// info, whose session is from.
func messageReport(info RunningSpawn, direct bool, from, text string) childReport {
	return childReport{Kind: "message", Child: fmt.Sprintf("%s (%s)", info.Label(), info.ID),
		Text: formatSpawnerMessageSteer(info, direct, text), Chat: formatSpawnerMessage(info, direct, text),
		From: from, Raw: text, ChildName: spawnOrigin(info, from, text).ChildName}
}

// deliverChild delivers r to owner: past any stopped spawn (to that spawn's
// own spawner), then to a background spawn as its input (feedSpawn), or
// else to the session as before.
func (s *Spawner) deliverChild(owner string, r childReport) {
	for range maxPassUp {
		up, note, ok := s.passUpStopped(owner, r)
		if !ok {
			break
		}
		spawnLog.Infof("child %s %s to %s, a stopped spawn: passed up to %s", r.Child, r.Kind, owner, up)
		r.Via = note + "\n\n" + r.Via
		owner = up
	}
	if s.toSpawnSession(owner, r) {
		return
	}
	s.deliverToSession(owner, r)
}

// deliverToSession is the delivery to an owner that is not a tracked
// background spawn (a chat, a depth node).
func (s *Spawner) deliverToSession(owner string, r childReport) {
	n := s.deps.Notifier
	if r.Kind == "message" {
		origin := NewAgentOrigin(r.From, r.Raw)
		origin.ChildName = r.ChildName
		origin.SteerText = r.input()
		n.InjectAgentMessage(owner, r.Via+r.Chat, "", origin)
		r.done()
		return
	}
	n.InjectToAgentThen(owner, r.input(), "async_notify", func(delivered bool) {
		if delivered {
			r.done()
			return
		}
		spawnLog.Infof("child %s result not taken by %s (refused) — kept for the next process", r.Child, owner)
	})
}

// passUpStopped reports, when owner is the branch of a background spawn that
// was stopped, that spawn's own owner and the note that heads r there.
func (s *Spawner) passUpStopped(owner string, r childReport) (up, note string, ok bool) {
	gp, info, found := s.deps.Tracker.ForBranch(owner)
	if !found || info.Direct {
		return "", "", false
	}
	o := s.spawnerOf(gp)
	if o == nil || !o.deps.Tracker.isStopped(gp, info.ID) {
		return "", "", false
	}
	note = fmt.Sprintf("[FROM A SPAWN OF YOUR STOPPED SPAWN] Your spawn %s (%s) was stopped, so a spawn it had started, %s, sends you its %s instead:",
		info.Label(), info.ID, r.Child, r.Kind)
	return o.route(gp).Owner, note, true
}

// spawnerOf is the Spawner that tracks the spawns started from parent: this
// one, or, for a spawn run as another agent, its caller's.
func (s *Spawner) spawnerOf(parent string) *Spawner {
	if a := session.AgentIDFromKey(parent); a != s.deps.AgentID {
		return spawnerFor(a)
	}
	return s
}

// toSpawnSession hands r to the background spawn whose branch is sessionKey,
// as its own input (see the file comment). It reports false when sessionKey
// is not a background spawn's branch that this process still tracks: the
// caller then delivers as before.
func (s *Spawner) toSpawnSession(sessionKey string, r childReport) bool {
	gp, info, ok := s.deps.Tracker.ForBranch(sessionKey)
	if !ok || info.Direct {
		return false
	}
	owner := s.spawnerOf(gp)
	if owner == nil {
		return false
	}
	return owner.feedSpawn(gp, info, r)
}

// feedSpawn gives r to parent's background spawn info: folded into its
// running turn, queued for its next step, or, when its run has ended, by
// resuming it. False when the spawn or its agent is gone.
func (s *Spawner) feedSpawn(parent string, info RunningSpawn, r childReport) bool {
	agent := s.agentOf(info)
	if agent == nil || s.deps.Notifier == nil {
		return false
	}
	tr := s.deps.Tracker
	running, waking, ok := tr.wakeState(parent, info.ID)
	if !ok {
		return false
	}
	if running && !waking && s.feedRunning(agent, parent, info, r.input()) {
		r.done()
		return true
	}
	start, ok := tr.queueWake(parent, info.ID, r)
	if !ok {
		return false
	}
	// The wake queue is persisted: the report is safe with the spawn.
	r.done()
	if start {
		// A slot may take a while, and the caller (a child's result
		// delivery) still holds one of its own: the resume waits on its own
		// goroutine. What arrives meanwhile joins the same resume, in order.
		go s.wakeSpawn(agent, parent, info)
	}
	return true
}

// feedRunning folds text into parent's running spawn's turn, or queues it for
// its next step. False when the spawn is no longer running.
func (s *Spawner) feedRunning(agent SpawnAgent, parent string, info RunningSpawn, text string) bool {
	tr := s.deps.Tracker
	if st, ok := agent.(SpawnSteerer); ok && st.SteerTurn(context.Background(), info.BranchKey, text) {
		tr.appendLog(parent, info.ID, "message", "(from its spawn, steered into the running turn) "+text)
		spawnLog.Infof("spawn=%s branch=%s took a message from its own spawn into its running turn (%d chars)", info.ID, info.BranchKey, len(text))
		return true
	}
	if tr.queue(parent, info.ID, text) {
		spawnLog.Infof("spawn=%s branch=%s queued a message from its own spawn for its next step (%d chars)", info.ID, info.BranchKey, len(text))
		return true
	}
	return false
}

// startWake starts the resume of parent's finished spawn id with the wake
// queue a restored record brought (RestoreSpawns), unless one is under way.
func (s *Spawner) startWake(parent, id string) {
	info, ok := s.deps.Tracker.spawnInfo(parent, id)
	if !ok || !s.deps.Tracker.claimWake(parent, id) {
		return
	}
	agent := s.agentOf(info)
	if agent == nil {
		n := s.deps.Tracker.dropWake(parent, id)
		spawnLog.Errorf("spawn=%s branch=%s: %d message(s) from its own spawns lost: its agent is not available", id, info.BranchKey, n)
		return
	}
	go s.wakeSpawn(agent, parent, info)
}

// wakeSpawn resumes parent's finished spawn with what its spawns sent it
// (the tracker's wake queue, spawnWakeNote heading it); queues it into the
// run when a spawn_send started it meanwhile; or passes it up when the spawn
// was stopped meanwhile.
func (s *Spawner) wakeSpawn(agent SpawnAgent, parent string, info RunningSpawn) {
	tr := s.deps.Tracker
	if err := s.slots.acquire(context.Background()); err != nil {
		n := tr.dropWake(parent, info.ID)
		spawnLog.Errorf("spawn=%s branch=%s: %d message(s) from its own spawns lost: no spawn slot: %v", info.ID, info.BranchKey, n, err)
		return
	}
	for range spawnWakeRetries {
		spawnCtx, cancel := s.runContext(context.Background(), info, true)
		reports, how := tr.takeWake(parent, info.ID, cancel)
		switch how {
		case wakeGone:
			cancel()
			s.slots.release()
			spawnLog.Errorf("spawn=%s branch=%s parent=%s: its record is gone, so what its own spawns sent it is lost", info.ID, info.BranchKey, parent)
			return
		case wakeQueued:
			cancel()
			s.slots.release()
			spawnLog.Infof("spawn=%s branch=%s was running again: %d message(s) from its own spawns queued for its next step", info.ID, info.BranchKey, len(reports))
			return
		case wakeStopped:
			cancel()
			s.slots.release()
			for _, r := range reports {
				s.deliverChild(info.BranchKey, r)
			}
			return
		case wakeResumed, wakeResumedFromWait:
			texts := make([]string, len(reports))
			for i, r := range reports {
				texts[i] = r.input()
				tr.appendLog(parent, info.ID, "message", "(resumed by its spawn with) "+texts[i])
			}
			spawnLog.Infof("wake spawn=%s branch=%s parent=%s: %d message(s) from its own spawns arrived after its run ended",
				info.ID, info.BranchKey, parent, len(texts))
			if s.deps.Observer != nil {
				if how == wakeResumedFromWait {
					s.deps.Observer.SpawnPrompt(parent, info.ID, strings.Join(texts, "\n\n"))
				} else {
					s.deps.Observer.SpawnStarted(parent, info, strings.Join(texts, "\n\n"))
				}
			}
			texts[0] = spawnWakeNote + "\n\n" + texts[0]
			// runBackground releases the slot when the run's result is out.
			_, _ = s.runBackground(context.Background(), agent, parent, info, texts, spawnCtx, cancel, "")
			return
		}
		// Being stopped: its run has not ended yet.
		cancel()
		time.Sleep(10 * time.Millisecond)
	}
	s.slots.release()
	spawnLog.Warnf("spawn=%s branch=%s parent=%s: its stop did not end its run in time; its spawns' reports stay queued for the next wake",
		info.ID, info.BranchKey, parent)
	tr.unclaimWake(parent, info.ID)
}

// --- tracker side ---

// wakeState reports whether parent's spawn id runs (and is not being
// stopped), and whether a wake for it is under way. ok is false when the
// tracker no longer knows it.
func (t *SpawnTracker) wakeState(parent, id string) (running, waking, ok bool) {
	if t == nil {
		return false, false, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil {
		return false, false, false
	}
	// A spawn waiting on its background work (#2728) takes a report as a
	// finished one does: a resume, which ends the wait.
	return s.running && !s.stopped && s.awaiting == nil, s.waking, true
}

// isStopped reports whether parent's spawn id was stopped (spawn_stop or the
// user's stop) and has not been run again since.
func (t *SpawnTracker) isStopped(parent, id string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	return s != nil && s.stopped
}

// queueWake adds r to the wake queue of parent's spawn id, persisted, and
// reports whether the caller must start the wake (none is under way yet).
func (t *SpawnTracker) queueWake(parent, id string, r childReport) (start, ok bool) {
	if t == nil {
		return false, false
	}
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return false, false
	}
	r.taken = nil
	s.wake = append(s.wake, r)
	start = !s.waking
	s.waking = true
	t.mu.Unlock()
	t.persist(parent, id)
	return start, true
}

// claimWake marks a wake under way for parent's spawn id when its wake queue
// holds reports and none is; false otherwise.
func (t *SpawnTracker) claimWake(parent, id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.spawns[parent][id]
	if s == nil || s.waking || len(s.wake) == 0 {
		return false
	}
	s.waking = true
	return true
}

// unclaimWake ends a wake under way without taking its queue, for a later
// report to start again.
func (t *SpawnTracker) unclaimWake(parent, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.spawns[parent][id]; s != nil {
		s.waking = false
	}
}

// wakeHow is what takeWake did.
type wakeHow int

const (
	wakeRetry           wakeHow = iota // being stopped: try again once its run ends
	wakeResumed                        // marked running for the resume run; reports are its input
	wakeResumedFromWait                // as wakeResumed, for a spawn that waited on its background work (#2728): still shown running
	wakeQueued                         // already running: reports went to its pending queue
	wakeStopped                        // stopped and ended: reports go to its own spawner
	wakeGone                           // the tracker no longer knows it
)

// takeWake ends the wake under way for parent's spawn id in one step: a
// finished spawn is marked running again with cancel (as restart does) and
// its wake queue returned as the resume's input; a running one has the queue
// moved to its pending messages; a stopped one returns it to pass up.
func (t *SpawnTracker) takeWake(parent, id string, cancel context.CancelFunc) (reports []childReport, how wakeHow) {
	t.mu.Lock()
	s := t.spawns[parent][id]
	if s == nil {
		t.mu.Unlock()
		return nil, wakeGone
	}
	if s.running && s.stopped {
		t.mu.Unlock()
		return nil, wakeRetry
	}
	reports = s.wake
	s.wake, s.waking = nil, false
	switch {
	case s.running && s.awaiting == nil:
		for _, r := range reports {
			s.pending = append(s.pending, r.input())
		}
		how = wakeQueued
	case s.stopped:
		how = wakeStopped
	default:
		how = wakeResumed
		if s.takeAwaitLocked() {
			how = wakeResumedFromWait
		}
		s.running, s.stopped, s.cancel, s.reporting = true, false, cancel, false
		s.ended, s.outcome = time.Time{}, ""
	}
	t.mu.Unlock()
	t.persist(parent, id)
	return reports, how
}

// dropWake forgets the wake queue of parent's spawn id, returning how many
// reports it held.
func (t *SpawnTracker) dropWake(parent, id string) int {
	t.mu.Lock()
	n := 0
	if s := t.spawns[parent][id]; s != nil {
		n = len(s.wake)
		s.wake, s.waking = nil, false
	}
	t.mu.Unlock()
	t.persist(parent, id)
	return n
}

// liveChildrenLocked reports whether a spawn started from branchKey still
// runs or holds a result it has not delivered. Caller holds t.mu.
func (t *SpawnTracker) liveChildrenLocked(branchKey string) bool {
	if branchKey == "" {
		return false
	}
	for _, c := range t.spawns[branchKey] {
		if c.running || c.result != "" {
			return true
		}
	}
	return false
}

// spawnOfBranchLocked finds the spawn whose branch is branchKey. Caller
// holds t.mu.
func (t *SpawnTracker) spawnOfBranchLocked(branchKey string) (parent, id string, ok bool) {
	if branchKey == "" {
		return "", "", false
	}
	for p, m := range t.spawns {
		for i, s := range m {
			if s.info.BranchKey == branchKey {
				return p, i, true
			}
		}
	}
	return "", "", false
}

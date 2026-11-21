package tools

import (
	"context"
	"fmt"
	"strings"
)

// spawnRestartNotice opens a resumed spawn's first turn after a restart: why
// it is running again, and its task, in case its backend lost the context.
func spawnRestartNotice(prompt string) string {
	msg := "[SYSTEM: FOCI RESTARTED] foci restarted while you were working, so your last turn was cut off. " +
		"Carry on with your task from where you left off; check what you already did before redoing any of it. " +
		"Your final answer goes to your parent session as before."
	if prompt != "" {
		msg += "\n\nYour task:\n\n" + prompt
	}
	return msg
}

// deliver hands a run's [SPAWN RESULT] to the spawn's owner: the parent that
// started it, or the depth node it has been handed to (#2624), and holds it
// while a reported leaf's report has not reached its parent's fan-in. The
// result stays persisted until the owner's turn has taken it, so one refused
// by a shutdown (or lost in a crash) is delivered, or held again, by the next
// process (#2589).
func (s *Spawner) deliver(parent, id, msg string) {
	r := s.deliveryRoute(parent)
	msg = s.deps.Tracker.pushResult(parent, id, msg, r.Hold)
	if r.Hold {
		spawnLog.Infof("spawn=%s result held: its parent %s has reported and its report has not reached the fan-in yet", id, parent)
		return
	}
	s.inject(parent, id, msg, r)
}

// spawnRestoreReport collects, per parent session, what RestoreSpawns did, for
// the one chat notice each parent's user gets.
type spawnRestoreReport struct {
	resumed []string
	lost    []string
}

// RestoreSpawns picks up this agent's spawns the last process left behind
// (#2589): a result the parent never took is delivered; a spawn that was
// running is resumed on its branch with a restart notice, or, when it cannot
// be, reported to its parent as lost. Each parent's user is told in one chat
// line which spawns were resumed and which were lost.
//
// Call it once per agent at startup, after the platforms are up (the resumed
// runs and the deliveries need chats) and BEFORE the agent's
// RestoreUndelivered: it takes its branches' unconsumed inputs into their
// resume turns. It returns at once; the resumed runs start on a goroutine as
// slots free up (max_concurrent_spawns).
func (s *Spawner) RestoreSpawns() {
	p := s.takeRestorePlan()
	if p == nil {
		p = s.adoptSpawns()
	}
	if p == nil {
		return
	}
	// Every agent's spawns are known by now (AdoptSpawns ran for each
	// before any RestoreSpawns): a result for a parent spawn another agent
	// tracks finds it, and wakes it, whatever order the agents restore in
	// (#3163).
	for _, d := range p.deliveries {
		d()
	}
	for _, rec := range p.wakes {
		spawnLog.Infof("spawn=%s resuming with %d report(s) from its own spawns the last process had queued", rec.ID, len(rec.Wake))
		s.startWake(rec.Parent, rec.ID)
	}
	for _, parent := range p.parents {
		s.noticeRestore(parent, p.reports[parent])
	}
	s.startResumes(p.resumes)
}

// spawnRestorePlan is what AdoptSpawns found in this agent's persisted spawns
// and RestoreSpawns then does: its deliveries, wakes, notices and resumes.
type spawnRestorePlan struct {
	deliveries []func()
	wakes      []spawnRecord
	reports    map[string]*spawnRestoreReport
	parents    []string
	resumes    []pendingResume
}

type pendingResume struct {
	rec    spawnRecord
	host   *Spawner
	agent  SpawnAgent
	base   context.Context //nolint:containedctx // the resumed run's base, made at adoption
	stop   context.CancelFunc
	notice string
}

// AdoptSpawns registers this agent's persisted spawns in the tracker, and
// delivers and resumes nothing: RestoreSpawns does that. The gateway runs
// AdoptSpawns for every agent before RestoreSpawns for any (#3163), so a
// child spawn's undelivered result, restored by one agent, finds its parent
// spawn, whose record another agent holds (a spawn run as another agent:
// its row is under its caller's agent, its children's under its own).
// RestoreSpawns without a prior AdoptSpawns adopts first.
func (s *Spawner) AdoptSpawns() {
	p := s.adoptSpawns()
	s.restoreMu.Lock()
	s.restorePlan = p
	s.restoreMu.Unlock()
}

func (s *Spawner) takeRestorePlan() *spawnRestorePlan {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	p := s.restorePlan
	s.restorePlan = nil
	return p
}

// adoptSpawns adopts this agent's persisted spawns and returns what is to be
// done with them, or nil when there is nothing.
func (s *Spawner) adoptSpawns() *spawnRestorePlan {
	tr := s.deps.Tracker
	recs := tr.loadRecords(s.deps.AgentID)
	if len(recs) == 0 {
		return nil
	}
	if s.deps.Notifier == nil {
		spawnLog.Errorf("agent=%s %d persisted spawn(s) but no background delivery — left in place", s.deps.AgentID, len(recs))
		return nil
	}
	p := &spawnRestorePlan{reports: map[string]*spawnRestoreReport{}}
	report := func(parent string) *spawnRestoreReport {
		if r := p.reports[parent]; r != nil {
			return r
		}
		r := &spawnRestoreReport{}
		p.reports[parent] = r
		p.parents = append(p.parents, parent)
		return r
	}
	deliver := func(parent, id, msg string) {
		p.deliveries = append(p.deliveries, func() { s.deliver(parent, id, msg) })
	}
	for _, rec := range recs {
		if !rec.Running {
			tr.adopt(rec, false, "completed", func() {})
			if rec.Result != "" {
				spawnLog.Infof("spawn=%s delivering the result the last process could not", rec.ID)
				deliver(rec.Parent, rec.ID, rec.Result)
			}
			// A finished spawn kept for its own spawns (#3163): reports
			// that were waiting to wake it resume it in RestoreSpawns.
			if len(rec.Wake) > 0 {
				p.wakes = append(p.wakes, rec)
			}
			continue
		}
		if rec.Stopped {
			// Stopped before the restart, its run cut before it ended
			// (#3226): not resumed. Its parent gets the stopped result its
			// run would have sent; no restart notice names it.
			tr.adopt(rec, false, "stopped", func() {})
			spawnLog.Infof("spawn=%s was stopped before the restart — reporting it stopped, not resuming it", rec.ID)
			p.deliveries = append(p.deliveries, func() {
				// Its app chit was left open by the last process (#3127).
				if s.deps.Observer != nil {
					s.deps.Observer.SpawnEnded(rec.Parent, rec.ID)
				}
				if rec.Result != "" {
					s.deliver(rec.Parent, rec.ID, rec.Result)
				}
				s.deliver(rec.Parent, rec.ID, keptMessagesHead(rec.ID, tr.takeKept(rec.Parent, rec.ID))+fmt.Sprintf(
					"%s was stopped by %s.", spawnResultHead(recInfo(rec).Label(), rec.BranchKey, rec.ID), tr.stopper(rec.Parent, rec.ID)))
			})
			continue
		}
		label := fmt.Sprintf("%s (%s)", rec.ID, recInfo(rec).Label())
		// The agent the spawn runs as: this one, or another for a
		// cross-agent spawn (#2662).
		host := s.hostOf(recInfo(rec))
		var agent SpawnAgent
		var reason string
		if rec.OneShot != nil {
			// An API one-shot (#2979) runs on the host's loop, not an agent's turn.
			reason = cannotResumeOneShot(host, rec)
		} else {
			if host != nil {
				agent = host.agentFn()
			}
			reason = s.cannotResume(host, agent, rec)
		}
		if reason != "" {
			tr.adopt(rec, false, "lost", func() {})
			spawnLog.Warnf("spawn=%s branch=%s cannot be resumed after restart: %s", rec.ID, rec.BranchKey, reason)
			p.deliveries = append(p.deliveries, func() {
				// Its app chit was left open by the last process, and no
				// later run will close it (#3127): end it here.
				if s.deps.Observer != nil {
					s.deps.Observer.SpawnEnded(rec.Parent, rec.ID)
				}
				if rec.Result != "" {
					s.deliver(rec.Parent, rec.ID, rec.Result)
				}
				// The messages it kept for its spawner head the report, as
				// they would have headed its result (#2677).
				s.deliver(rec.Parent, rec.ID, keptMessagesHead(rec.ID, tr.takeKept(rec.Parent, rec.ID))+fmt.Sprintf(
					"%s was lost in a foci restart and could not be resumed: %s. Its task was:\n\n%s",
					spawnResultHead(recInfo(rec).Label(), rec.BranchKey, rec.ID), reason, rec.Prompt))
			})
			report(rec.Parent).lost = append(report(rec.Parent).lost, label+": "+reason)
			continue
		}
		if rec.OneShot == nil && host.deps.TakeUndelivered != nil {
			// What its backend never consumed joins the parent messages
			// it had queued: the adopted record keeps them all until the
			// resumed run takes them, so a second restart while it waits
			// for a slot does not lose them (#3071).
			rec.Pending = append(host.deps.TakeUndelivered(rec.BranchKey), rec.Pending...)
		}
		base, stop := context.WithCancel(context.Background())
		tr.adopt(rec, true, "", stop)
		if rec.Result != "" {
			deliver(rec.Parent, rec.ID, rec.Result)
		}
		report(rec.Parent).resumed = append(report(rec.Parent).resumed, label)
		if rec.OneShot != nil {
			p.resumes = append(p.resumes, pendingResume{rec: rec, host: host, base: base, stop: stop})
			continue
		}
		p.resumes = append(p.resumes, pendingResume{rec: rec, host: host, agent: agent, base: base, stop: stop,
			notice: host.withSteerNotice(spawnRestartNotice(rec.Prompt))})
	}
	return p
}

// startResumes runs the restored spawns' resumes, in order, as slots free up.
func (s *Spawner) startResumes(resumes []pendingResume) {
	if len(resumes) == 0 {
		return
	}
	go func() {
		for _, r := range resumes {
			if r.rec.OneShot != nil {
				// Not in a slot: a direct spawn never took one.
				go s.resumeOneShotAfterRestart(r.host, r.rec, r.base, r.stop)
				continue
			}
			s.resumeAfterRestart(r.host, r.agent, r.rec, r.base, r.stop, r.notice)
		}
	}()
}

// cannotResume says why a running spawn cannot be resumed, or "" when it can.
// host is the Spawner of the agent the spawn runs as (nil: that agent is
// gone).
func (s *Spawner) cannotResume(host *Spawner, agent SpawnAgent, rec spawnRecord) string {
	switch {
	case host == nil || agent == nil:
		return "its agent is not available"
	case rec.Resumes >= maxSpawnRestartResumes:
		return fmt.Sprintf("restarts had already interrupted it %d times", rec.Resumes)
	case host.deps.CanResume != nil && !host.deps.CanResume(rec.BranchKey):
		return "its branch session has no saved conversation to continue"
	}
	return ""
}

// noticeRestore tells the parent's user which of its spawns a restart resumed
// and which it lost, in one chat line that runs no turn.
func (s *Spawner) noticeRestore(parent string, r *spawnRestoreReport) {
	var b strings.Builder
	b.WriteString("↻ foci restarted.")
	if len(r.resumed) > 0 {
		fmt.Fprintf(&b, " Resumed %d background spawn(s): %s.", len(r.resumed), strings.Join(r.resumed, "; "))
	}
	if len(r.lost) > 0 {
		fmt.Fprintf(&b, " Could not resume %d (its parent has been told): %s.", len(r.lost), strings.Join(r.lost, "; "))
	}
	owner := s.route(parent).Owner
	spawnLog.Infof("session=%s %s", owner, b.String())
	if s.deps.Notice != nil {
		s.deps.Notice(owner, b.String())
	}
}

// resumeAfterRestart runs a restored spawn again once a slot frees up, with
// notice (the restart notice, with its task), then its pending inputs:
// whatever its backend never consumed, then the parent messages that were
// waiting, then any spawn_send that arrived while it waited. They stay in its
// persisted record until it takes them here, once it has a slot (#3071). Its
// stop handle is stop (base's cancel) from the start, so /stop reaches it
// while it waits for a slot.
func (s *Spawner) resumeAfterRestart(host *Spawner, agent SpawnAgent, rec spawnRecord, base context.Context, stop context.CancelFunc, notice string) {
	tr := s.deps.Tracker
	_ = s.slots.acquire(context.Background()) // never fails: a /stop meanwhile cancels base, so the run ends at once
	texts := append([]string{notice}, tr.drainPending(rec.Parent, rec.ID)...)
	running := recInfo(rec)
	if running.Character && host.deps.PrepareCharacterBranch != nil {
		// Its launch settings were in the last process's memory (#2643).
		host.deps.PrepareCharacterBranch(rec.BranchKey, rec.SpawnModel)
	}
	spawnCtx, cancelRun := s.runContext(base, running, false)
	cancel := func() { cancelRun(); stop() }
	tr.setCancel(rec.Parent, rec.ID, cancel)

	tr.appendLog(rec.Parent, rec.ID, "message", "(resumed after a foci restart)")
	spawnLog.Infof("resume spawn=%s branch=%s parent=%s after restart (resume %d) inputs=%d",
		rec.ID, rec.BranchKey, rec.Parent, rec.Resumes+1, len(texts))

	if s.deps.Observer != nil {
		shown := running
		// The prefix goes in front of its name, which the app shows.
		shown.Name = "(resumed after restart) " + running.Label()
		s.deps.Observer.SpawnStarted(rec.Parent, shown, texts[0])
	}
	_, _ = s.runBackground(context.Background(), agent, rec.Parent, running, texts, spawnCtx, cancel, "")
}

// recInfo is the RunningSpawn a persisted record describes.
func recInfo(rec spawnRecord) RunningSpawn {
	mode := spawnModeClone
	if rec.Character {
		mode = spawnModeCharacter
	}
	if rec.OneShot != nil {
		// Still a one-shot (#2979): it takes no spawn_send and has no
		// branch to resume on, though its result now comes as a
		// [SPAWN RESULT].
		mode = rec.OneShot.Mode
	}
	return RunningSpawn{
		ID: rec.ID, BranchKey: rec.BranchKey, Name: rec.Name, Description: rec.Description,
		Model: rec.Model, Started: rec.Started,
		Character: rec.Character, SpawnModel: rec.SpawnModel,
		Mode: mode, Agent: rec.Agent, SpawnerUnattended: rec.SpawnerUnattended,
		Direct: rec.OneShot != nil,
		// A record written before depths (#3061) is a spawn all the same:
		// spawns could not nest then, so it is a spawn of a depth-0 session.
		Depth: max(rec.Depth, 1),
	}
}

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
	r := s.route(parent)
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
	tr := s.deps.Tracker
	recs := tr.loadRecords(s.deps.AgentID)
	if len(recs) == 0 {
		return
	}
	if s.deps.Notifier == nil {
		spawnLog.Errorf("agent=%s %d persisted spawn(s) but no background delivery — left in place", s.deps.AgentID, len(recs))
		return
	}
	agent := s.agentFn()

	reports := map[string]*spawnRestoreReport{}
	var parents []string
	report := func(parent string) *spawnRestoreReport {
		if r := reports[parent]; r != nil {
			return r
		}
		r := &spawnRestoreReport{}
		reports[parent] = r
		parents = append(parents, parent)
		return r
	}

	type pendingResume struct {
		rec   spawnRecord
		base  context.Context
		stop  context.CancelFunc
		texts []string
	}
	var resumes []pendingResume
	for _, rec := range recs {
		if !rec.Running {
			tr.adopt(rec, false, "completed", func() {})
			spawnLog.Infof("spawn=%s delivering the result the last process could not", rec.ID)
			s.deliver(rec.Parent, rec.ID, rec.Result)
			continue
		}
		label := fmt.Sprintf("%s (%s)", rec.ID, rec.Description)
		if reason := s.cannotResume(agent, rec); reason != "" {
			tr.adopt(rec, false, "lost", func() {})
			spawnLog.Warnf("spawn=%s branch=%s cannot be resumed after restart: %s", rec.ID, rec.BranchKey, reason)
			if rec.Result != "" {
				s.deliver(rec.Parent, rec.ID, rec.Result)
			}
			s.deliver(rec.Parent, rec.ID, fmt.Sprintf(
				"[SPAWN RESULT] Branch %s (spawn %s) was lost in a foci restart and could not be resumed: %s. Its task was:\n\n%s",
				rec.BranchKey, rec.ID, reason, rec.Prompt))
			report(rec.Parent).lost = append(report(rec.Parent).lost, label+": "+reason)
			continue
		}
		base, stop := context.WithCancel(context.Background())
		tr.adopt(rec, true, "", stop)
		if rec.Result != "" {
			s.deliver(rec.Parent, rec.ID, rec.Result)
		}
		report(rec.Parent).resumed = append(report(rec.Parent).resumed, label)
		texts := []string{spawnRestartNotice(rec.Prompt)}
		if s.deps.TakeUndelivered != nil {
			texts = append(texts, s.deps.TakeUndelivered(rec.BranchKey)...)
		}
		texts = append(texts, rec.Pending...)
		resumes = append(resumes, pendingResume{rec: rec, base: base, stop: stop, texts: texts})
	}

	for _, parent := range parents {
		s.noticeRestore(parent, reports[parent])
	}
	if len(resumes) == 0 {
		return
	}
	go func() {
		for _, r := range resumes {
			s.resumeAfterRestart(agent, r.rec, r.base, r.stop, r.texts)
		}
	}()
}

// cannotResume says why a running spawn cannot be resumed, or "" when it can.
func (s *Spawner) cannotResume(agent SpawnAgent, rec spawnRecord) string {
	switch {
	case agent == nil:
		return "its agent is not available"
	case rec.Resumes >= maxSpawnRestartResumes:
		return fmt.Sprintf("restarts had already interrupted it %d times", rec.Resumes)
	case s.deps.CanResume != nil && !s.deps.CanResume(rec.BranchKey):
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
// texts: a restart notice (with its task), then whatever its backend never
// consumed, then the parent messages that were waiting. Its stop handle is
// stop (base's cancel) from the start, so /stop reaches it while it waits for
// a slot.
func (s *Spawner) resumeAfterRestart(agent SpawnAgent, rec spawnRecord, base context.Context, stop context.CancelFunc, texts []string) {
	tr := s.deps.Tracker
	s.sem <- struct{}{}
	spawnCtx, cancelRun := buildSpawnContext(base, rec.Timeout, rec.BranchKey, false)
	cancel := func() { cancelRun(); stop() }
	tr.setCancel(rec.Parent, rec.ID, cancel)

	tr.appendLog(rec.Parent, rec.ID, "message", "(resumed after a foci restart)")
	spawnLog.Infof("resume spawn=%s branch=%s parent=%s after restart (resume %d) timeout=%s inputs=%d",
		rec.ID, rec.BranchKey, rec.Parent, rec.Resumes+1, rec.Timeout, len(texts))

	running := RunningSpawn{
		ID: rec.ID, BranchKey: rec.BranchKey, Description: rec.Description,
		Model: rec.Model, Started: rec.Started,
	}
	if s.deps.Observer != nil {
		shown := running
		shown.Description = "(resumed after restart) " + running.Description
		s.deps.Observer.SpawnStarted(rec.Parent, shown, texts[0])
	}
	_, _ = s.runBackground(context.Background(), agent, rec.Parent, running, texts, spawnCtx, cancel, "")
}

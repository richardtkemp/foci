package periodic


import (
	"fmt"
	"time"

	"foci/internal/skills"

	"foci/shared/prompts"
)


func (r *Runner) maybeReflection() {
	if !r.reflectCfg.IntervalEnabled || r.agent == nil {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip reflection: %s", skip)
		}
	}()

	interval, ok := r.parseDuration("reflection interval", r.reflectCfg.Interval)
	if !ok {
		return
	}

	now := time.Now()

	r.mu.Lock()
	lastReflection := r.lastReflection
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	resetRunning := r.resetRunning
	r.mu.Unlock()

	nextFire := lastReflection.Add(interval)
	if running {
		skip = "already running"
		return
	}
	// Memory-mutating passes are mutually exclusive: reflection, consolidation
	// and reset all form/curate memory on the same session, so none may run
	// while another is in flight (consolidation and reset already defer to the
	// others; this makes reflection defer too).
	if consolidationRunning {
		skip = "consolidation running"
		return
	}
	if resetRunning {
		skip = "reset running"
		return
	}
	if now.Before(nextFire) {
		skip = fmt.Sprintf("too soon (next at %s)", nextFire.Format("15:04:05"))
		return
	}

	if sinceLastInteraction > interval {
		skip = fmt.Sprintf("idle %s > interval %s", sinceLastInteraction.Round(time.Second), interval)
		return
	}

	// Delegated agents fork a real backend branch off each due session when the
	// backend supports it (BranchForkBackend), falling back to an in-place turn
	// only when it can't fork. The fork briefly quiesces the parent to clone its
	// transcript, so still wait for the user to be quiet before firing.
	if r.isDelegatedAgent {
		quietPeriod, qOk := r.parseDuration("backend_quiet_period", r.reflectCfg.BackendQuietPeriod)
		if qOk && sinceLastInteraction < quietPeriod {
			skip = fmt.Sprintf("backend: user active (idle %s < quiet %s)", sinceLastInteraction.Round(time.Second), quietPeriod)
			return
		}
	}

	// Query DB for sessions with activity since their last reflection.
	if r.sessionIndex == nil {
		skip = "no session index"
		return
	}
	keys, err := r.sessionIndex.SessionsNeedingReflection(r.agentID)
	if err != nil {
		skip = fmt.Sprintf("query sessions: %v", err)
		return
	}
	if len(keys) == 0 {
		skip = "no sessions need reflection"
		return
	}

	// Filter out sessions with a turn currently in flight. For delegated
	// agents reflection injects into the main session — firing while the
	// user's turn is mid-flight queues the reflection prompt as a SourceUser
	// follow-up, which is the wrong source attribution and the wrong timing.
	// Defer to the next 30s tick. (TODO #760)
	filtered := keys[:0]
	busy, limited := 0, 0
	for _, k := range keys {
		if r.agent.IsTurnInFlight(k) {
			busy++
			continue
		}
		// Shared rate-limit gate, per specific session (no can_run_background).
		if lim, _ := r.agent.RateLimited(k); lim {
			limited++
			continue
		}
		filtered = append(filtered, k)
	}
	keys = filtered
	if busy > 0 {
		r.log.Debugf("reflection: deferred %d session(s) with in-flight turns", busy)
	}
	if limited > 0 {
		r.log.Debugf("reflection: deferred %d rate-limited session(s)", limited)
	}
	if len(keys) == 0 {
		skip = "no ready sessions (in-flight or rate-limited)"
		return
	}

	promptText := prompts.ResolvePrompt(r.reflectCfg.IntervalPrompt, "reflection.md", prompts.Reflection(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.reflectionRunning = true
	r.mu.Unlock()

	r.log.Infof("firing reflection pass for agent %s (%d sessions)", r.agentID, len(keys))

	notifySkills := r.reflectCfg.NotifyOnSkillCreation && len(r.skillDirs) > 0 && r.notifySkillChange != nil

	go func() {
		defer func() {
			r.mu.Lock()
			r.reflectionRunning = false
			r.lastReflection = time.Now()
			r.mu.Unlock()
		}()
		// Snapshot before each reflection branch and diff after it, so a skill
		// create/update is attributed to the session that was reflected (the
		// branch's parent) and the notification goes there — not to keys[0].
		var prev skills.SkillSnapshot
		if notifySkills {
			prev = skills.Snapshot(r.skillDirs)
		}
		for _, key := range keys {
			t := time.Now()
			ran := r.agent.Branch("reflection", key, promptText, true)
			if ran {
				r.sessionIndex.StampReflection(key, t)
			}
			if notifySkills && ran {
				after := skills.Snapshot(r.skillDirs)
				if msg := skills.FormatChanges(skills.Diff(prev, after)); msg != "" {
					r.notifySkillChange(key, msg)
				}
				prev = after
			}
		}
	}()
}

// ReflectSessionIfDue fires a single reflection branch for sessionKey iff it is
// due by the same "activity since last reflection" rule the periodic pass uses,
// then stamps it. Used for the final reflection when an app session is archived
// (#app-binding-restore) — wired into the app hub via app.SetReflectOnArchive.
// No-op if the runner has no agent/index, the session isn't due, or no reflection
// prompt resolves.
func (r *Runner) ReflectSessionIfDue(sessionKey string) {
	if r == nil || r.agent == nil || r.sessionIndex == nil {
		return
	}
	if !r.sessionIndex.SessionNeedsReflection(sessionKey) {
		return
	}
	promptText := prompts.ResolvePrompt(r.reflectCfg.IntervalPrompt, "reflection.md", prompts.Reflection(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	var skillBefore skills.SkillSnapshot
	if r.reflectCfg.NotifyOnSkillCreation && len(r.skillDirs) > 0 && r.notifySkillChange != nil {
		skillBefore = skills.Snapshot(r.skillDirs)
	}

	t := time.Now()
	if r.agent.Branch("reflection", sessionKey, promptText, true) {
		r.sessionIndex.StampReflection(sessionKey, t)
	}

	if skillBefore != nil {
		after := skills.Snapshot(r.skillDirs)
		changes := skills.Diff(skillBefore, after)
		if msg := skills.FormatChanges(changes); msg != "" {
			r.notifySkillChange(sessionKey, msg)
		}
	}
}


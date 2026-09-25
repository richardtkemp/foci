package periodic

import "time"

// LastUserActivity is the single answer to "when did a human last interact
// with this agent?" for every agent-scoped idle/active check: the reflection,
// consolidation, background-work and reset-idle-guard gates here, and the
// warning dispatchers' active/inactive cadence (wired in periodic_setup.go).
//
// The durable source of truth is session_index.last_user_activity_at (agent
// max via SessionIndex.LastUserActivityForAgent). The turn path writes it on
// every interactive human turn (telegram/discord/app/voice), it excludes
// wake/cron/keepalive/background/memory turns, and it survives a restart. It
// is read live on every call, not seeded once, so a restart can never make an
// idle agent look active (#2023: the old boot-seeded in-memory timestamp made
// every agent look active for its first hour after boot).
//
// The in-process receipt stamp (NotifyInteraction) is folded in as a max: it
// covers human input that reaches no turn and so never touches the DB — a
// slash command, or a message still queued behind an in-flight turn. It is
// zero at boot, so it can only ever make an agent look MORE recently active
// than the DB says, and only for interactions this process actually saw.
//
// ok is false when no human interaction is recorded at all.
func (r *Runner) LastUserActivity() (time.Time, bool) {
	r.mu.Lock()
	last := r.lastInteraction
	r.mu.Unlock()
	if r.sessionIndex != nil {
		if t, ok := r.sessionIndex.LastUserActivityForAgent(r.agentID); ok && t.After(last) {
			last = t
		}
	}
	return last, !last.IsZero()
}

// sinceUserActivity is how long the agent's human has been idle, for the
// scheduler gates. An agent with NO recorded interaction falls back to boot
// time — the pre-#2023 behaviour, kept deliberately: a brand-new or never-used
// agent behaves as before (reflection/consolidation may run in its first
// window; background work and a scheduled reset wait out their idle windows)
// rather than being treated as idle since the epoch. Must not be called with
// r.mu held (LastUserActivity takes it).
func (r *Runner) sinceUserActivity() time.Duration {
	if t, ok := r.LastUserActivity(); ok {
		return time.Since(t)
	}
	return time.Since(r.bootedAt)
}

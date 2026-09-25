package periodic

import "time"

// Keys of the runner's restart-surviving timers in agent_metadata (#2026).
// Each is restored by New() and saved when its run completes, through the one
// shared helper (session.PersistedTime). Saving on completion rather than on
// fire is deliberate: a run cut short by the restart itself left no mark, so
// the new process retries it instead of skipping it.
const (
	timerReflection       = "reflection_last"
	timerConsolidation    = "consolidation_last"
	timerReset            = "reset_last"
	timerBackgroundEnded  = "background_last_ended"
	timerEphemeralCleanup = "ephemeral_cleanup_last"
)

// restoreTimers overwrites each timer's boot default with its persisted value,
// when there is one. Called once from New().
func (r *Runner) restoreTimers() {
	for key, dst := range map[string]*time.Time{
		timerReflection:       &r.lastReflection,
		timerConsolidation:    &r.lastConsolidation,
		timerReset:            &r.lastReset,
		timerBackgroundEnded:  &r.lastBackgroundEnded,
		timerEphemeralCleanup: &r.lastEphemeralCleanup,
	} {
		if t, ok := r.sessionIndex.PersistedTime(r.agentID, key).Load(); ok {
			*dst = t
		}
	}
}

// saveTimer persists one timer. A failure is logged, not fatal: the in-memory
// value still governs this process; only the next restart loses it.
func (r *Runner) saveTimer(key string, t time.Time) {
	if err := r.sessionIndex.PersistedTime(r.agentID, key).Save(t); err != nil {
		r.log.Warnf("persist %s: %v", key, err)
	}
}

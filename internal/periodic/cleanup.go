package periodic


import (
	"context"
	"fmt"
	"time"


	"foci/internal/timeutil"
)


func (r *Runner) maybeEphemeralCleanup(ctx context.Context) {
	if r.ephemeralRetentionDays <= 0 || r.agent == nil {
		return
	}
	now := timeutil.Now()
	r.mu.Lock()
	if r.ephemeralCleanupRunning {
		r.mu.Unlock()
		return
	}
	if !r.lastEphemeralCleanup.IsZero() && now.Sub(r.lastEphemeralCleanup) < 24*time.Hour {
		r.mu.Unlock()
		return
	}
	r.lastEphemeralCleanup = now
	r.ephemeralCleanupRunning = true
	r.mu.Unlock()

	// Run the GC off the tick goroutine so a slow transcript sweep never
	// stalls keepalive/background timing; ephemeralCleanupRunning guards
	// against a second run overlapping the first.
	go func() {
		defer func() {
			r.mu.Lock()
			r.ephemeralCleanupRunning = false
			r.mu.Unlock()
		}()
		if n := r.agent.CleanupEphemeralSessions(ctx, r.ephemeralRetentionDays); n > 0 {
			r.log.Infof("ephemeral cleanup: deleted %d stale transcript(s) older than %dd", n, r.ephemeralRetentionDays)
		}
	}()
}

// maybeReset fires the scheduled daily session reset (reset_time). A soft reset
// runs memory formation then rotates the session key — identical to a manual
// /reset (see Agent.ResetSession, via BackgroundAgent.ResetSession). Disabled
// when reset_time is empty. Skips if the user interacted within reset_idle_guard,
// mirroring the `foci command --if-inactive` crontab this replaces, so a live
// conversation is never wiped out from under the user.
func (r *Runner) maybeReset(ctx context.Context) {
	if r.maintCfg.ResetTime == "" || r.agent == nil {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip reset: %s", skip)
		}
	}()

	sched, ok := parseSchedule(r.maintCfg.ResetTime)
	if !ok {
		r.log.Warnf("bad reset_time %q (want HH:MM or a duration like 24h)", r.maintCfg.ResetTime)
		return
	}

	now := timeutil.Now()

	r.mu.Lock()
	lastReset := r.lastReset
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.resetRunning
	reflectionRunning := r.reflectionRunning
	consolidationRunning := r.consolidationRunning
	r.mu.Unlock()

	nextFire := sched.nextFire(lastReset, now.Location())
	if running {
		skip = "already running"
		return
	}
	if reflectionRunning || consolidationRunning {
		skip = "memory task running"
		return
	}
	if now.Before(nextFire) {
		skip = fmt.Sprintf("too soon (next at %s)", nextFire.Format("2006-01-02 15:04:05"))
		return
	}

	// Inactivity guard: skip if the user was active within the guard window.
	// A zero/invalid guard disables the check (always fire at the scheduled time).
	if guard, gok := r.parseDuration("reset_idle_guard", r.maintCfg.ResetIdleGuard); gok && guard > 0 && sinceLastInteraction < guard {
		skip = fmt.Sprintf("user active %s ago (< guard %s)", sinceLastInteraction.Round(time.Second), guard)
		return
	}

	// Shared rate-limit gate on the specific parent session. Skipping here
	// during a cap is deliberate: reset rotates the session key AND forms
	// memory, and the downstream memory pass is itself rate-limited — firing
	// anyway would rotate the key while losing the memory formation.
	parentKey, skip := r.readyGatedParent(r.checkRateLimit)
	if skip != "" {
		return
	}

	r.mu.Lock()
	r.resetRunning = true
	r.lastReset = now
	r.mu.Unlock()

	r.log.Infof("firing scheduled session reset for agent %s (session %s)", r.agentID, parentKey)

	go func() {
		defer func() {
			r.mu.Lock()
			r.resetRunning = false
			r.mu.Unlock()
			if r.sessionIndex != nil {
				if err := r.sessionIndex.SetAgentMetadata(r.agentID, "reset_last", timeutil.Format(timeutil.Now())); err != nil {
					r.log.Warnf("persist reset timestamp: %v", err)
				}
			}
		}()
		if err := r.agent.ResetSession(ctx, parentKey); err != nil {
			r.log.Warnf("scheduled reset failed for %s: %v", parentKey, err)
			return
		}
		r.log.Infof("scheduled session reset complete for agent %s", r.agentID)
	}()
}

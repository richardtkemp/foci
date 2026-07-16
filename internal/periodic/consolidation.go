package periodic


import (
	"context"
	"fmt"
	"time"


	"foci/internal/timeutil"
	"foci/shared/prompts"
)


func (r *Runner) maybeConsolidation() {
	if !r.maintCfg.ConsolidationEnabled || r.agent == nil {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip consolidation: %s", skip)
		}
	}()

	sched, ok := parseSchedule(r.maintCfg.ConsolidationTime)
	if !ok {
		r.log.Warnf("bad consolidation_time %q (want HH:MM or a duration like 20h)", r.maintCfg.ConsolidationTime)
		return
	}

	now := timeutil.Now()

	r.mu.Lock()
	lastConsolidation := r.lastConsolidation
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.consolidationRunning
	reflectionRunning := r.reflectionRunning
	resetRunning := r.resetRunning
	r.mu.Unlock()

	nextFire := sched.nextFire(lastConsolidation, now.Location())
	if running {
		skip = "already running"
		return
	}
	if reflectionRunning {
		skip = "reflection running"
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

	if maxIdle, ok := r.parseDuration("consolidation_max_idle", r.maintCfg.ConsolidationMaxIdle); ok && maxIdle > 0 && sinceLastInteraction > maxIdle {
		skip = fmt.Sprintf("idle %s > %s", sinceLastInteraction.Round(time.Second), maxIdle)
		return
	}

	// Shared rate-limit gate on the specific parent session (no can_run_background).
	parentKey, skip := r.readyGatedParent(r.checkRateLimit)
	if skip != "" {
		return
	}

	promptText := prompts.ResolvePrompt(r.maintCfg.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.consolidationRunning = true
	r.lastConsolidation = now
	r.mu.Unlock()

	r.log.Infof("firing memory consolidation for agent %s", r.agentID)

	go func() {
		defer func() {
			r.mu.Lock()
			r.consolidationRunning = false
			r.mu.Unlock()
			if r.sessionIndex != nil {
				if err := r.sessionIndex.SetAgentMetadata(r.agentID, "consolidation_last", timeutil.Format(timeutil.Now())); err != nil {
					r.log.Warnf("persist consolidation timestamp: %v", err)
				}
			}
		}()
		if r.isDelegatedAgent {
			// Backend: one-shot via claude --print with full character as system prompt.
			resp, err := r.agent.RunOnce(context.Background(), promptText, "")
			if err != nil {
				r.log.Warnf("consolidation RunOnce failed: %v", err)
				return
			}
			_ = resp // consolidation writes to files directly via tools
			r.log.Infof("consolidation RunOnce complete")
		} else {
			r.agent.Branch("consolidation", parentKey, promptText, true)
		}
	}()
}

// maybeEphemeralCleanup runs the daily GC of stale ephemeral (branch/fork)
// backend transcript files. Fires at most once per 24h (and shortly after
// boot). Disabled when ephemeral_retention_days is 0. Files only — session_index
// rows are left intact.

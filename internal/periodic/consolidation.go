package periodic

import (
	"context"
	"fmt"
	"time"

	"foci/internal/delegator"
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

	sinceLastInteraction := r.sinceUserActivity()
	r.mu.Lock()
	lastConsolidation := r.lastConsolidation
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
			// Persist the same fire time the in-memory schedule uses, so a
			// restart computes exactly the next fire this process would have.
			r.saveTimer(timerConsolidation, now)
		}()
		if r.isDelegatedAgent {
			// Backend: a batch run on the agent's own backend — an ordinary
			// turn on an ephemeral child of the parent session, so its spend
			// is recorded like any turn (#1962) — with the character files as
			// the system prompt, the same corpus a branch session inherits on
			// the API path (#1310).
			sys := ""
			if r.characterSystemPromptFunc != nil {
				sys = r.characterSystemPromptFunc()
			}
			resp, err := r.agent.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt:          promptText,
				SystemPrompt:    sys,
				OwnerSessionKey: parentKey,
				Purpose:         delegator.BatchPurposeConsolidation,
			})
			if err != nil {
				r.log.Warnf("consolidation batch failed: %v", err)
				return
			}
			_ = resp // consolidation writes to files directly via tools
			r.log.Infof("consolidation batch complete")
		} else {
			r.agent.Branch("consolidation", parentKey, promptText, true)
		}
	}()
}

// maybeEphemeralCleanup runs the daily GC of stale ephemeral (branch/fork)
// backend transcript files. Fires at most once per 24h, across restarts too:
// the last run is persisted, so boot only triggers it when a day has passed
// (or it never ran). Disabled when ephemeral_retention_days is 0. Files only — session_index
// rows are left intact.

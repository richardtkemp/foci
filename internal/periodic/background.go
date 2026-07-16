package periodic


import (
	"context"
	"fmt"
	"time"


	"foci/shared/prompts"
)


func (r *Runner) maybeBackgroundWork(ctx context.Context) {
	if !r.bgCfg.Enabled || r.agent == nil {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip background: %s", skip)
		}
	}()

	interval, ok := r.parseDuration("background interval", r.bgCfg.Interval)
	if !ok {
		return
	}

	r.mu.Lock()
	elapsed := time.Since(r.lastInteraction)
	running := r.backgroundRunning
	sinceLastBgEnd := time.Since(r.lastBackgroundEnded)
	lastBgEndZero := r.lastBackgroundEnded.IsZero()
	r.mu.Unlock()

	if running {
		skip = "already running"
		return
	}

	if n := r.agent.HasActiveWork(); n > 0 {
		skip = fmt.Sprintf("%d active tmux watches", n)
		return
	}

	if elapsed < interval {
		skip = fmt.Sprintf("idle %s < interval %s", elapsed.Round(time.Second), interval)
		return
	}

	// Enforce cooldown: don't start a new background session sooner than the
	// configured interval after the previous one ended. This prevents rapid
	// self-chaining where each completed session immediately triggers the next,
	// accumulating orphaned child processes (e.g. coding agents in tmux).
	if !lastBgEndZero && sinceLastBgEnd < interval {
		skip = fmt.Sprintf("cooldown %s < interval %s", sinceLastBgEnd.Round(time.Second), interval)
		return
	}

	// Check for open background-tagged todos
	if r.todoStore != nil {
		count, err := r.todoStore.CountOpenByTag(r.agentID, "background")
		if err != nil {
			r.log.Warnf("count background todos: %v", err)
			return
		}
		if count == 0 {
			skip = "no open background todos"
			return
		}
	}

	// Background work is the ONLY scheduler that runs the full gate (rate limit
	// + can_run_background script).
	parentKey, skip := r.readyGatedParent(func(sk string) string { return r.checkCanFire(ctx, sk) })
	if skip != "" {
		return
	}

	promptText := prompts.ResolvePrompt(r.bgCfg.Prompt, "background.md", prompts.Background(), r.promptSearchDirs...)

	r.mu.Lock()
	r.backgroundRunning = true
	r.mu.Unlock()

	// Background work branches off parentKey; TouchRootCacheForBranch stamps the
	// parent's last_cache_touch at branch creation, so the per-session keepalive
	// gate sees a fresh touch and won't double-warm right after — no separate
	// debounce needed here.
	r.log.Infof("firing background work for agent %s (idle %s)", r.agentID, elapsed.Round(time.Second))

	go func() {
		defer func() {
			r.mu.Lock()
			r.backgroundRunning = false
			r.lastBackgroundEnded = time.Now()
			r.mu.Unlock()
		}()
		r.agent.Branch("background", parentKey, promptText, true)
	}()
}


// Package keepalive provides cache keepalive, background work, and memory formation timers.
//
// Four mechanisms run as a single goroutine with ~30s ticks:
//
//   - Keepalive: fires when the cache hasn't been warmed within the configured interval.
//     Creates a lightweight branch session to keep the Anthropic cache prefix alive.
//
//   - Background work: fires when the user has been idle for the configured interval
//     AND there are open background-tagged todos AND the manamometer says we can afford it.
//     Creates a branch session that picks up the next background task.
//
//   - Memory formation: fires periodically to capture conversation memories to daily files.
//
//   - Memory consolidation: fires on a longer interval to curate MEMORY.md from daily files.
package keepalive

import (
	"context"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/mana"
	"foci/internal/memory"
	"foci/prompts"
	"foci/internal/state"
	"foci/internal/warnings"
)

const (
	tickInterval = 30 * time.Second
)

// parseDuration parses a duration string, returning the parsed duration or logging a warning on error.
func (r *Runner) parseDuration(field string, s string) (time.Duration, bool) {
	d, err := time.ParseDuration(s)
	if err != nil {
		r.log.Warnf("bad %s %q: %v", field, s, err)
		return 0, false
	}
	return d, true
}

// BranchFunc is called to dispatch a branch session. It receives the branch type
// ("keepalive" or "background"), the prompt text, and whether to skip compaction.
// It must handle branch creation and agent execution internally.
type BranchFunc func(branchType, promptText string, noCompact bool)

// Runner manages keepalive, background work, and memory formation timers for an agent.
type Runner struct {
	log               *log.ComponentLogger
	agentID           string
	kaCfg             config.KeepaliveConfig
	bgCfg             config.BackgroundConfig
	mfCfg             config.MemoryFormationConfig
	promptSearchDirs  []string
	todoStore         *memory.TodoStore
	stateStore        *state.Store
	branchFn          BranchFunc
	manaMonitor       *mana.Monitor
	warningDispatcher *warnings.Dispatcher
	drainFn           func() // called each tick to drain rate-limit queues

	mu                    sync.Mutex
	lastCacheWarmed       time.Time
	lastInteraction       time.Time
	keepaliveRunning      bool
	backgroundRunning     bool
	lastBackgroundEnded   time.Time // when the last background session finished

	// Memory formation state
	lastMemoryFormation    time.Time
	lastConsolidation      time.Time
	memoryFormationRunning bool
	consolidationRunning   bool

	cancel context.CancelFunc
	done   chan struct{}
}

// RunnerConfig holds all the dependencies for creating a Runner.
type RunnerConfig struct {
	AgentID           string
	Keepalive         config.KeepaliveConfig
	Background        config.BackgroundConfig
	MemoryFormation   config.MemoryFormationConfig
	PromptSearchDirs  []string // directories to search for prompt files (agent workspace, shared)
	TodoStore         *memory.TodoStore
	StateStore        *state.Store
	BranchFunc        BranchFunc
	ManaMonitor       *mana.Monitor
	WarningDispatcher *warnings.Dispatcher
	DrainFn           func() // called each tick to drain rate-limit queues; nil = skip
}

// New creates a runner. Call Start() to begin the timer loop.
func New(cfg RunnerConfig) *Runner {
	now := time.Now()
	r := &Runner{
		log:               log.NewComponentLogger("keepalive:" + cfg.AgentID),
		agentID:           cfg.AgentID,
		kaCfg:             cfg.Keepalive,
		bgCfg:             cfg.Background,
		mfCfg:             cfg.MemoryFormation,
		promptSearchDirs:  cfg.PromptSearchDirs,
		todoStore:         cfg.TodoStore,
		stateStore:        cfg.StateStore,
		branchFn:          cfg.BranchFunc,
		manaMonitor:       cfg.ManaMonitor,
		warningDispatcher: cfg.WarningDispatcher,
		drainFn:           cfg.DrainFn,
		lastCacheWarmed:   now,
		lastInteraction:   now,
		lastMemoryFormation: now,
		done:              make(chan struct{}),
	}
	// Restore consolidation timestamp from persistent state
	if cfg.StateStore != nil {
		var ts time.Time
		if cfg.StateStore.Get("consolidation_last:"+cfg.AgentID, &ts) {
			r.lastConsolidation = ts
		}
	}
	return r
}

// Start begins the timer loop in a goroutine.
func (r *Runner) Start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	go r.run(ctx)
}

// Stop gracefully stops the timer loop and waits for it to exit.
func (r *Runner) Stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
	}
}

// NotifyCacheWarmed records that the cache was just warmed (API call happened).
func (r *Runner) NotifyCacheWarmed() {
	r.mu.Lock()
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()
}

// NotifyInteraction records user interaction (message received or background branch completed).
func (r *Runner) NotifyInteraction() {
	r.mu.Lock()
	r.lastInteraction = time.Now()
	r.mu.Unlock()
}

func (r *Runner) run(ctx context.Context) {
	defer close(r.done)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.drainFn != nil {
				r.drainFn()
			}
			r.maybeKeepalive(ctx)
			r.maybeBackgroundWork(ctx)
			r.maybeMemoryFormation()
			r.maybeConsolidation()
			if r.warningDispatcher != nil {
				r.warningDispatcher.MaybeFire()
			}
		}
	}
}

func (r *Runner) maybeKeepalive(ctx context.Context) { // nolint:unparam
	if !r.kaCfg.Enabled {
		return
	}

	interval, ok := r.parseDuration("keepalive interval", r.kaCfg.Interval)
	if !ok {
		return
	}

	r.mu.Lock()
	elapsed := time.Since(r.lastCacheWarmed)
	running := r.keepaliveRunning
	r.mu.Unlock()

	if elapsed < interval || running {
		return
	}

	promptText := prompts.ResolvePrompt(r.kaCfg.Prompt, "keepalive.md", prompts.Keepalive(), r.promptSearchDirs...)

	r.mu.Lock()
	r.keepaliveRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	r.log.Infof("firing keepalive for agent %s (cache age %s)", r.agentID, elapsed.Round(time.Second))

	go func() {
		defer func() {
			r.mu.Lock()
			r.keepaliveRunning = false
			r.mu.Unlock()
		}()
		r.branchFn("keepalive", promptText, true)
	}()
}

func (r *Runner) maybeBackgroundWork(ctx context.Context) {
	if !r.bgCfg.Enabled {
		return
	}

	interval, ok := r.parseDuration("background interval", r.bgCfg.Interval)
	if !ok {
		return
	}

	r.mu.Lock()
	elapsed := time.Since(r.lastInteraction)
	running := r.backgroundRunning
	sinceLastBgEnd := time.Since(r.lastBackgroundEnded)
	r.mu.Unlock()

	if running {
		return
	}

	if elapsed < interval {
		return
	}

	// Enforce cooldown: don't start a new background session sooner than the
	// configured interval after the previous one ended. This prevents rapid
	// self-chaining where each completed session immediately triggers the next,
	// accumulating orphaned child processes (e.g. coding agents in tmux).
	if !r.lastBackgroundEnded.IsZero() && sinceLastBgEnd < interval {
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
			return
		}
	}

	// Check mana
	if r.manaMonitor != nil {
		investInterval, _ := r.parseDuration("invest interval", r.bgCfg.InvestInterval)
		if investInterval == 0 {
			investInterval = 30 * time.Minute // default fallback
		}
		if !r.manaMonitor.IsGoodFor(ctx, investInterval) {
			return
		}
	}

	promptText := prompts.ResolvePrompt(r.bgCfg.Prompt, "background.md", prompts.Background(), r.promptSearchDirs...)

	r.mu.Lock()
	r.backgroundRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	r.log.Infof("firing background work for agent %s (idle %s)", r.agentID, elapsed.Round(time.Second))

	go func() {
		defer func() {
			r.mu.Lock()
			r.backgroundRunning = false
			r.lastBackgroundEnded = time.Now()
			r.mu.Unlock()
		}()
		r.branchFn("background", promptText, true)
	}()
}

func (r *Runner) maybeMemoryFormation() {
	if r.mfCfg.IntervalEnabled != nil && !*r.mfCfg.IntervalEnabled {
		return
	}

	interval, ok := r.parseDuration("memory formation interval", r.mfCfg.Interval)
	if !ok {
		return
	}

	now := time.Now()

	r.mu.Lock()
	lastFormation := r.lastMemoryFormation
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.memoryFormationRunning
	hasActivity := r.lastInteraction.After(r.lastMemoryFormation)
	r.mu.Unlock()

	nextFire := lastFormation.Truncate(interval).Add(interval)
	if now.Before(nextFire) || running || !hasActivity {
		return
	}

	if sinceLastInteraction > interval {
		return
	}

	promptText := prompts.ResolvePrompt(r.mfCfg.IntervalPrompt, "memory-formation.md", prompts.MemoryFormation(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.memoryFormationRunning = true
	r.lastMemoryFormation = now
	r.mu.Unlock()

	r.log.Infof("firing memory formation for agent %s", r.agentID)

	go func() {
		defer func() {
			r.mu.Lock()
			r.memoryFormationRunning = false
			r.mu.Unlock()
		}()
		r.branchFn("memory-formation", promptText, true)
	}()
}

func (r *Runner) maybeConsolidation() {
	if r.mfCfg.ConsolidationEnabled != nil && !*r.mfCfg.ConsolidationEnabled {
		return
	}

	interval, ok := r.parseDuration("consolidation interval", r.mfCfg.ConsolidationInterval)
	if !ok {
		return
	}

	now := time.Now()

	r.mu.Lock()
	lastConsolidation := r.lastConsolidation
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.consolidationRunning
	r.mu.Unlock()

	nextFire := lastConsolidation.Truncate(interval).Add(interval)
	if now.Before(nextFire) || running {
		return
	}

	if sinceLastInteraction > time.Hour {
		return
	}

	promptText := prompts.ResolvePrompt(r.mfCfg.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation(), r.promptSearchDirs...)
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
			if r.stateStore != nil {
				if err := r.stateStore.Set("consolidation_last:"+r.agentID, time.Now()); err != nil {
					r.log.Warnf("persist consolidation timestamp: %v", err)
				}
			}
		}()
		r.branchFn("consolidation", promptText, true)
	}()
}

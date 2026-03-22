// Package periodic provides cache keepalive, background work, and memory formation timers.
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
package periodic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"

	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/warnings"
	"foci/shared/prompts"
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
	log                *log.ComponentLogger
	agentID            string
	client             provider.Client // for checking caching availability at runtime
	cachingOverride    *bool           // nil=use client.IsCachingAvailable(), non-nil=override
	kaCfg              config.ResolvedKeepalive
	bgCfg              config.ResolvedBackground
	mfCfg              config.ResolvedMemoryFormation
	manaInvestInterval string // mana invest interval (Go duration string)
	promptSearchDirs   []string
	todoStore          *memory.TodoStore
	sessionIndex       *session.SessionIndex
	branchFn           BranchFunc

	warningDispatcher      *warnings.Dispatcher
	chatWarningDispatcher  *warnings.Dispatcher
	hasActiveWorkFn    func() int // external check for async work (e.g. tmux watches); returns count, 0 = none
	drainFn            func() // called each tick to drain rate-limit queues

	// Session-aware availability checking
	sessionKeyFn func() string                                                   // returns the session key these operations will run on
	canFireFn    func(ctx context.Context, sessionKey string) (bool, string) // checks if background operations can fire

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
	AgentID            string
	Client             provider.Client // for checking caching availability at runtime
	CachingOverride    *bool           // nil=use client.IsCachingAvailable(), non-nil=override (for OpenAI/DeepSeek)
	Keepalive          config.ResolvedKeepalive
	Background         config.ResolvedBackground
	MemoryFormation    config.ResolvedMemoryFormation
	ManaInvestInterval string                // mana invest interval (Go duration string, default: "30m")
	PromptSearchDirs   []string              // directories to search for prompt files (agent workspace, shared)
	TodoStore          *memory.TodoStore
	SessionIndex       *session.SessionIndex
	BranchFunc         BranchFunc

	WarningDispatcher      *warnings.Dispatcher
	ChatWarningDispatcher  *warnings.Dispatcher
	HasActiveWorkFn    func() int // external check for async work (e.g. tmux watches); returns count, 0 = none
	DrainFn            func() // called each tick to drain rate-limit queues; nil = skip

	// Session-aware availability checking
	SessionKeyFunc func() string                                                   // returns the session key these operations will run on
	CanFireFunc    func(ctx context.Context, sessionKey string) (bool, string) // checks if background operations can fire
}

// New creates a runner. Call Start() to begin the timer loop.
func New(cfg RunnerConfig) *Runner {
	now := time.Now()
	r := &Runner{
		log:                log.NewComponentLogger("keepalive:" + cfg.AgentID),
		agentID:            cfg.AgentID,
		client:             cfg.Client,
		cachingOverride:    cfg.CachingOverride,
		kaCfg:              cfg.Keepalive,
		bgCfg:              cfg.Background,
		mfCfg:              cfg.MemoryFormation,
		manaInvestInterval: cfg.ManaInvestInterval,
		promptSearchDirs:   cfg.PromptSearchDirs,
		todoStore:          cfg.TodoStore,
		sessionIndex:       cfg.SessionIndex,
		branchFn:           cfg.BranchFunc,

		warningDispatcher:      cfg.WarningDispatcher,
		chatWarningDispatcher:  cfg.ChatWarningDispatcher,
		hasActiveWorkFn:    cfg.HasActiveWorkFn,
		drainFn:            cfg.DrainFn,
		sessionKeyFn:       cfg.SessionKeyFunc,
		canFireFn:          cfg.CanFireFunc,
		lastCacheWarmed:    now,
		lastInteraction:    now,
		lastMemoryFormation: now,
		done:               make(chan struct{}),
	}
	// Restore consolidation timestamp from persistent state
	if cfg.SessionIndex != nil {
		if raw, err := cfg.SessionIndex.GetAgentMetadata(cfg.AgentID, "consolidation_last"); err == nil && raw != "" {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				r.lastConsolidation = ts
			}
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

// NotifyTurnEnd flushes any warnings that were deferred during the agent turn.
func (r *Runner) NotifyTurnEnd() {
	if r.warningDispatcher != nil {
		r.warningDispatcher.FlushPending()
	}
	if r.chatWarningDispatcher != nil {
		r.chatWarningDispatcher.FlushPending()
	}
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
			if r.chatWarningDispatcher != nil {
				r.chatWarningDispatcher.MaybeFire()
			}
		}
	}
}

func (r *Runner) maybeKeepalive(ctx context.Context) { // nolint:unparam
	if !r.kaCfg.Enabled {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip keepalive: %s", skip)
		}
	}()

	// Check if caching is still available.
	// cachingOverride allows models with auto-detected caching (OpenAI, DeepSeek) to
	// bypass the client.IsCachingAvailable() check which may return false for non-Anthropic clients.
	cachingAvailable := true
	if r.cachingOverride != nil {
		cachingAvailable = *r.cachingOverride
	} else if r.client != nil {
		cachingAvailable = r.client.IsCachingAvailable()
	}
	if !cachingAvailable {
		skip = "caching not available"
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

	if running {
		skip = "already running"
		return
	}
	if elapsed < interval {
		skip = fmt.Sprintf("cache age %s < interval %s", elapsed.Round(time.Second), interval)
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

	if r.hasActiveWorkFn != nil {
		if n := r.hasActiveWorkFn(); n > 0 {
			skip = fmt.Sprintf("%d active tmux watches", n)
			return
		}
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

	// Check availability (rate limit + mana)
	if r.sessionKeyFn != nil && r.canFireFn != nil {
		sessionKey := r.sessionKeyFn()
		canFire, reason := r.canFireFn(ctx, sessionKey)
		if !canFire {
			skip = reason
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
	if !r.mfCfg.IntervalEnabled {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip memory-formation: %s", skip)
		}
	}()

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
	if running {
		skip = "already running"
		return
	}
	if now.Before(nextFire) {
		skip = fmt.Sprintf("too soon (next at %s)", nextFire.Format("15:04:05"))
		return
	}
	if !hasActivity {
		skip = "no activity since last formation"
		return
	}

	if sinceLastInteraction > interval {
		skip = fmt.Sprintf("idle %s > interval %s", sinceLastInteraction.Round(time.Second), interval)
		return
	}

	// Check availability (rate limit + mana)
	if r.sessionKeyFn != nil && r.canFireFn != nil {
		sessionKey := r.sessionKeyFn()
		canFire, reason := r.canFireFn(context.Background(), sessionKey)
		if !canFire {
			skip = reason
			return
		}
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
	if !r.mfCfg.ConsolidationEnabled {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip consolidation: %s", skip)
		}
	}()

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
	if running {
		skip = "already running"
		return
	}
	if now.Before(nextFire) {
		skip = fmt.Sprintf("too soon (next at %s)", nextFire.Format("15:04:05"))
		return
	}

	if sinceLastInteraction > time.Hour {
		skip = fmt.Sprintf("idle %s > 1h", sinceLastInteraction.Round(time.Second))
		return
	}

	// Check availability (rate limit + mana)
	if r.sessionKeyFn != nil && r.canFireFn != nil {
		sessionKey := r.sessionKeyFn()
		canFire, reason := r.canFireFn(context.Background(), sessionKey)
		if !canFire {
			skip = reason
			return
		}
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
			if r.sessionIndex != nil {
				if err := r.sessionIndex.SetAgentMetadata(r.agentID, "consolidation_last", time.Now().Format(time.RFC3339)); err != nil {
					r.log.Warnf("persist consolidation timestamp: %v", err)
				}
			}
		}()
		r.branchFn("consolidation", promptText, true)
	}()
}

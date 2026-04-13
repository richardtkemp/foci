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
	"foci/internal/timeutil"
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
// ("keepalive", "background", "reflection", "consolidation"), the parent
// session key to branch from, the prompt text, and whether to skip compaction.
// Returns true if the branch was created and the agent turn completed successfully.
type BranchFunc func(branchType, parentKey, promptText string, noCompact bool) bool

// Runner manages keepalive, background work, and reflection timers for an agent.
type Runner struct {
	log                *log.ComponentLogger
	agentID            string
	client             provider.Client // for checking caching availability at runtime
	cachingOverride    *bool           // nil=use client.IsCachingAvailable(), non-nil=override
	kaCfg              config.ResolvedKeepalive
	bgCfg              config.ResolvedBackground
	reflectCfg         config.ResolvedReflection
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
	sessionKeyFn   func() string                                                   // returns the session key these operations will run on
	canFireFn      func(ctx context.Context, sessionKey string) (bool, string) // checks if background operations can fire
	isDelegatedAgent bool // reflection needs quiet period in delegated mode
	runOnceFn      func(ctx context.Context, prompt, systemPrompt string) (string, error)

	mu                    sync.Mutex
	lastCacheWarmed       time.Time
	lastInteraction       time.Time
	keepaliveRunning      bool
	backgroundRunning     bool
	lastBackgroundEnded   time.Time // when the last background session finished

	// Reflection state
	lastReflection       time.Time
	lastConsolidation    time.Time
	reflectionRunning    bool
	consolidationRunning bool

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
	Reflection         config.ResolvedReflection
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

	// IsDelegatedAgent indicates this agent uses a delegated transport (CC).
	// When true, reflection requires a quiet period (no recent user
	// activity) because reflection runs IN the live session, not as a branch.
	IsDelegatedAgent bool

	// RunOnceFunc executes a one-shot prompt via claude --print.
	// If set, used for consolidation (and other independent tasks) instead of
	// BranchFunc, avoiding interactive session overhead and platform delivery.
	// The systemPrompt parameter is passed via --system-prompt (empty = default).
	RunOnceFunc func(ctx context.Context, prompt, systemPrompt string) (string, error)
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
		reflectCfg:         cfg.Reflection,
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
		isDelegatedAgent:   cfg.IsDelegatedAgent,
		runOnceFn:          cfg.RunOnceFunc,
		lastCacheWarmed:    now,
		lastInteraction:    now,
		lastReflection:     now,
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

// defaultParentKey returns the default session key, or "" if unavailable.
func (r *Runner) defaultParentKey() string {
	if r.sessionKeyFn == nil {
		return ""
	}
	return r.sessionKeyFn()
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
			// Reflection runs before consolidation so that all the latest
			// memory content is available when consolidation curates MEMORY.md.
			// Consolidation also skips if reflection is still running.
			r.maybeReflection()
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

	parentKey := r.defaultParentKey()
	if parentKey == "" {
		skip = "no default session"
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
		r.branchFn("keepalive", parentKey, promptText, true)
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

	parentKey := r.defaultParentKey()
	if parentKey == "" {
		skip = "no default session"
		return
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
		r.branchFn("background", parentKey, promptText, true)
	}()
}

func (r *Runner) maybeReflection() {
	if !r.reflectCfg.IntervalEnabled {
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
	r.mu.Unlock()

	nextFire := lastReflection.Add(interval)
	if running {
		skip = "already running"
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

	// In delegated mode, reflection runs IN the live CC session (not as
	// a branch), so wait for user to be quiet before interrupting.
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

	// Check availability (rate limit + mana)
	if r.sessionKeyFn != nil && r.canFireFn != nil {
		sessionKey := r.sessionKeyFn()
		canFire, reason := r.canFireFn(context.Background(), sessionKey)
		if !canFire {
			skip = reason
			return
		}
	}

	promptText := prompts.ResolvePrompt(r.reflectCfg.IntervalPrompt, "reflection.md", prompts.Reflection(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.reflectionRunning = true
	r.mu.Unlock()

	r.log.Infof("firing reflection pass for agent %s (%d sessions)", r.agentID, len(keys))

	go func() {
		defer func() {
			r.mu.Lock()
			r.reflectionRunning = false
			r.lastReflection = time.Now()
			r.mu.Unlock()
		}()
		for _, key := range keys {
			t := time.Now()
			if r.branchFn("reflection", key, promptText, true) {
				r.sessionIndex.StampReflection(key, t)
			}
		}
	}()
}

func (r *Runner) maybeConsolidation() {
	if !r.reflectCfg.ConsolidationEnabled {
		return
	}

	skip := ""
	defer func() {
		if skip != "" {
			r.log.Debugf("skip consolidation: %s", skip)
		}
	}()

	interval, ok := r.parseDuration("consolidation interval", r.reflectCfg.ConsolidationInterval)
	if !ok {
		return
	}

	now := time.Now()

	r.mu.Lock()
	lastConsolidation := r.lastConsolidation
	sinceLastInteraction := time.Since(r.lastInteraction)
	running := r.consolidationRunning
	reflectionRunning := r.reflectionRunning
	r.mu.Unlock()

	nextFire := lastConsolidation.Truncate(interval).Add(interval)
	if running {
		skip = "already running"
		return
	}
	if reflectionRunning {
		skip = "reflection running"
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

	parentKey := r.defaultParentKey()
	if parentKey == "" {
		skip = "no default session"
		return
	}

	promptText := prompts.ResolvePrompt(r.reflectCfg.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation(), r.promptSearchDirs...)
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
		if r.runOnceFn != nil {
			// Backend: one-shot via claude --print with full character as system prompt.
			resp, err := r.runOnceFn(context.Background(), promptText, "")
			if err != nil {
				r.log.Warnf("consolidation RunOnce failed: %v", err)
				return
			}
			_ = resp // consolidation writes to files directly via tools
			r.log.Infof("consolidation RunOnce complete")
		} else {
			r.branchFn("consolidation", parentKey, promptText, true)
		}
	}()
}

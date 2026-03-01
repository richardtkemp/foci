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
	"fmt"
	"sync"
	"time"

	"foci/agent"
	"foci/anthropic"
	"foci/config"
	"foci/log"
	"foci/memory"
	"foci/prompts"
	"foci/session"
	"foci/state"
)

const (
	tickInterval = 30 * time.Second
	manaWindow   = 5 * time.Hour

	// Minimum interval between usage API calls.
	usagePollInterval = 60 * time.Second
)

// BranchFunc is called to dispatch a branch session. It receives the branch type
// ("keepalive" or "background"), the prompt text, and whether to skip compaction.
// It must handle branch creation and agent execution internally.
type BranchFunc func(branchType, promptText string, noCompact bool)

// WarningDispatchFunc is called to dispatch a proactive warning turn.
// It receives the formatted warning text and should inject it as a user message
// into the agent's default session, triggering a full agent turn.
type WarningDispatchFunc func(warningText string)

// Runner manages keepalive, background work, and memory formation timers for an agent.
type Runner struct {
	agentID     string
	kaCfg       config.KeepaliveConfig
	bgCfg       config.BackgroundConfig
	mfCfg       config.MemoryFormationConfig
	todoStore   *memory.TodoStore
	usageClient *anthropic.UsageClient
	stateStore  *state.Store
	branchFn    BranchFunc

	mu                    sync.Mutex
	lastCacheWarmed       time.Time
	lastInteraction       time.Time
	keepaliveRunning      bool
	backgroundRunning     bool
	lastBackgroundEnded time.Time // when the last background session finished

	// Memory formation state
	lastMemoryFormation    time.Time
	lastConsolidation      time.Time
	memoryFormationRunning bool
	consolidationRunning   bool

	// Proactive warning dispatch state
	warningQueue              *agent.WarningQueue
	warningDispatchFn         WarningDispatchFunc
	warningActiveInterval     time.Duration
	warningInactiveInterval   time.Duration
	warningActivityThreshold  time.Duration
	lastUserMessageTimeFn     func() time.Time
	lastWarningDispatch       time.Time
	warningDispatching        bool

	// Cached mana state
	lastUsagePoll time.Time
	cachedMana    float64 // 0-100 (100 = fully available)
	cachedReset   time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// RunnerConfig holds all the dependencies for creating a Runner.
type RunnerConfig struct {
	AgentID         string
	Keepalive       config.KeepaliveConfig
	Background      config.BackgroundConfig
	MemoryFormation config.MemoryFormationConfig
	TodoStore       *memory.TodoStore
	UsageClient     *anthropic.UsageClient
	StateStore      *state.Store
	BranchFunc      BranchFunc

	// Proactive warning dispatch
	WarningQueue              *agent.WarningQueue
	WarningDispatchFn         WarningDispatchFunc
	WarningActiveInterval     time.Duration
	WarningInactiveInterval   time.Duration
	WarningActivityThreshold  time.Duration
	LastUserMessageTimeFn     func() time.Time
}

// New creates a runner. Call Start() to begin the timer loop.
func New(cfg RunnerConfig) *Runner {
	now := time.Now()
	r := &Runner{
		agentID:                  cfg.AgentID,
		kaCfg:                    cfg.Keepalive,
		bgCfg:                    cfg.Background,
		mfCfg:                    cfg.MemoryFormation,
		todoStore:                cfg.TodoStore,
		usageClient:              cfg.UsageClient,
		stateStore:               cfg.StateStore,
		branchFn:                 cfg.BranchFunc,
		warningQueue:             cfg.WarningQueue,
		warningDispatchFn:        cfg.WarningDispatchFn,
		warningActiveInterval:    cfg.WarningActiveInterval,
		warningInactiveInterval:  cfg.WarningInactiveInterval,
		warningActivityThreshold: cfg.WarningActivityThreshold,
		lastUserMessageTimeFn:    cfg.LastUserMessageTimeFn,
		lastCacheWarmed:          now,
		lastInteraction:          now,
		lastMemoryFormation:      now,
		done:                     make(chan struct{}),
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
			r.maybeKeepalive(ctx)
			r.maybeBackgroundWork(ctx)
			r.maybeMemoryFormation()
			r.maybeConsolidation()
			r.maybeWarningDispatch()
		}
	}
}

func (r *Runner) maybeKeepalive(ctx context.Context) {
	if !r.kaCfg.Enabled {
		return
	}

	interval, err := time.ParseDuration(r.kaCfg.Interval)
	if err != nil {
		log.Warnf("keepalive", "bad interval %q: %v", r.kaCfg.Interval, err)
		return
	}

	r.mu.Lock()
	elapsed := time.Since(r.lastCacheWarmed)
	running := r.keepaliveRunning
	r.mu.Unlock()

	if elapsed < interval || running {
		return
	}

	promptText := prompts.ResolvePrompt(r.kaCfg.Prompt, "keepalive", prompts.Keepalive())

	r.mu.Lock()
	r.keepaliveRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	log.Infof("keepalive", "firing keepalive for agent %s (cache age %s)", r.agentID, elapsed.Round(time.Second))

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

	interval, err := time.ParseDuration(r.bgCfg.Interval)
	if err != nil {
		log.Warnf("keepalive", "bad background interval %q: %v", r.bgCfg.Interval, err)
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
			log.Warnf("keepalive", "count background todos: %v", err)
			return
		}
		if count == 0 {
			return
		}
	}

	// Check mana
	if !r.manaIsGood(ctx) {
		return
	}

	promptText := prompts.ResolvePrompt(r.bgCfg.Prompt, "background", prompts.Background())

	r.mu.Lock()
	r.backgroundRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	log.Infof("keepalive", "firing background work for agent %s (idle %s)", r.agentID, elapsed.Round(time.Second))

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

	interval, err := time.ParseDuration(r.mfCfg.Interval)
	if err != nil {
		log.Warnf("keepalive", "bad memory formation interval %q: %v", r.mfCfg.Interval, err)
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

	promptText := prompts.ResolvePrompt(r.mfCfg.IntervalPrompt, "memory-formation", prompts.MemoryFormation())
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.memoryFormationRunning = true
	r.lastMemoryFormation = now
	r.mu.Unlock()

	log.Infof("keepalive", "firing memory formation for agent %s", r.agentID)

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

	interval, err := time.ParseDuration(r.mfCfg.ConsolidationInterval)
	if err != nil {
		log.Warnf("keepalive", "bad consolidation interval %q: %v", r.mfCfg.ConsolidationInterval, err)
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

	promptText := prompts.ResolvePrompt(r.mfCfg.ConsolidationPrompt, "consolidation", prompts.MemoryConsolidation())
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.consolidationRunning = true
	r.lastConsolidation = now
	r.mu.Unlock()

	log.Infof("keepalive", "firing memory consolidation for agent %s", r.agentID)

	go func() {
		defer func() {
			r.mu.Lock()
			r.consolidationRunning = false
			r.mu.Unlock()
			if r.stateStore != nil {
				if err := r.stateStore.Set("consolidation_last:"+r.agentID, time.Now()); err != nil {
					log.Warnf("keepalive", "persist consolidation timestamp: %v", err)
				}
			}
		}()
		r.branchFn("consolidation", promptText, true)
	}()
}

// maybeWarningDispatch checks for pending warnings and dispatches them proactively
// as a user-role message. Rate limited by user activity: active interval if the user
// has been recently active, inactive interval otherwise.
func (r *Runner) maybeWarningDispatch() {
	if r.warningQueue == nil || r.warningDispatchFn == nil {
		return
	}

	if !r.warningQueue.Pending() {
		return
	}

	r.mu.Lock()
	dispatching := r.warningDispatching
	sinceLastDispatch := time.Since(r.lastWarningDispatch)
	r.mu.Unlock()

	if dispatching {
		return
	}

	// Determine rate limit interval based on user activity
	interval := r.warningInactiveInterval
	if r.lastUserMessageTimeFn != nil {
		lastMsg := r.lastUserMessageTimeFn()
		if !lastMsg.IsZero() && time.Since(lastMsg) < r.warningActivityThreshold {
			interval = r.warningActiveInterval
		}
	}

	if sinceLastDispatch < interval {
		return
	}

	// Drain and format
	warnings := r.warningQueue.Drain()
	if len(warnings) == 0 {
		return
	}

	text := "[proactive system warnings]"
	for _, w := range warnings {
		text += "\n- " + w
	}

	r.mu.Lock()
	r.warningDispatching = true
	r.lastWarningDispatch = time.Now()
	r.mu.Unlock()

	log.Infof("keepalive", "dispatching %d proactive warnings for agent %s", len(warnings), r.agentID)

	go func() {
		defer func() {
			r.mu.Lock()
			r.warningDispatching = false
			r.mu.Unlock()
		}()
		r.warningDispatchFn(text)
	}()
}

// manaIsGood checks whether we can afford to run background work.
// Uses the manamometer: linear interpolation of expected mana over the 5-hour window.
func (r *Runner) manaIsGood(ctx context.Context) bool {
	if r.usageClient == nil {
		return true // no usage client = no rate limiting
	}

	r.mu.Lock()
	needPoll := time.Since(r.lastUsagePoll) >= usagePollInterval
	r.mu.Unlock()

	if needPoll {
		usage, err := r.usageClient.GetUsage(ctx)
		if err != nil {
			log.Warnf("keepalive", "usage API: %v", err)
			return false // err on the side of caution
		}

		r.mu.Lock()
		r.lastUsagePoll = time.Now()
		if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
			r.cachedMana = 100 - *usage.FiveHour.Utilization
			if r.cachedMana < 0 {
				r.cachedMana = 0
			}
		}
		if usage.FiveHour != nil && usage.FiveHour.ResetsAt != nil {
			r.cachedReset, _ = time.Parse(time.RFC3339Nano, *usage.FiveHour.ResetsAt)
		}
		r.mu.Unlock()
	}

	investInterval, err := time.ParseDuration(r.bgCfg.InvestInterval)
	if err != nil {
		investInterval = 30 * time.Minute
	}

	r.mu.Lock()
	mana := r.cachedMana
	resetsAt := r.cachedReset
	r.mu.Unlock()

	return ManaIsGood(mana, resetsAt, investInterval, time.Now())
}

// ManaIsGood implements the manamometer check. Exported for testing.
//
// Logic:
//  1. Calculate time_since_reset = window - (resetsAt - now)
//  2. If time_since_reset < investInterval: return false (investing period)
//  3. expected_mana = 100 * (window - time_since_reset) / (window - investInterval)
//  4. Return actualMana > expectedMana
func ManaIsGood(actualMana float64, resetsAt time.Time, investInterval time.Duration, now time.Time) bool {
	if resetsAt.IsZero() {
		return true // no data = allow
	}

	timeSinceReset := manaWindow - resetsAt.Sub(now)
	if timeSinceReset < 0 {
		timeSinceReset = 0
	}

	// Investing period — don't spend
	if timeSinceReset < investInterval {
		return false
	}

	// Linear interpolation: expected mana line from 100% at investInterval to 0% at window end
	denominator := manaWindow - investInterval
	if denominator <= 0 {
		return actualMana > 0
	}

	expectedMana := 100 * float64(manaWindow-timeSinceReset) / float64(denominator)

	return actualMana > expectedMana
}

// OrientationBuilder constructs orientation text for a branch session given the
// branch key, parent key, and branch type. Injected from main to avoid duplicating
// prompt defaults.
type OrientationBuilder func(branchKey, parentKey, branchType string) string

// BuildBranchFunc creates a BranchFunc that dispatches branch sessions using the
// provided agent infrastructure. This is the bridge between the keepalive package
// and the main package's agent/session handling.
func BuildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	buildOrientation OrientationBuilder,
	ctx context.Context,
) BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf("keepalive", "no default session for agent %q, skipping %s", agentID, branchType)
			return
		}

		branchID := fmt.Sprintf("%s-%d", branchType, time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", agentID, branchID)

		orientText := buildOrientation(branchKey, parentKey, branchType)
		err := sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        true,
			OrientationMessage: orientText,
		})
		if err != nil {
			log.Errorf("keepalive", "%s branch error: %v", branchType, err)
			return
		}

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			turnCtx = agent.WithNoCompact(turnCtx)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Errorf("keepalive", "%s turn error: %v", branchType, err)
			return
		}
		_ = resp // keepalive/background responses are not delivered to user
	}
}

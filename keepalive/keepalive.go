// Package keepalive provides cache keepalive and mana-gated background work timers.
//
// Two mechanisms run as a single goroutine with ~30s ticks:
//
//   - Keepalive: fires when the cache hasn't been warmed within the configured interval.
//     Creates a lightweight branch session to keep the Anthropic cache prefix alive.
//
//   - Background work: fires when the user has been idle for the configured interval
//     AND there are open background-tagged todos AND the manamometer says we can afford it.
//     Creates a branch session that picks up the next background task.
package keepalive

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"foci/agent"
	"foci/anthropic"
	"foci/config"
	"foci/log"
	"foci/memory"
	"foci/session"
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

// Runner manages keepalive and background work timers for an agent.
type Runner struct {
	agentID     string
	kaCfg       config.KeepaliveConfig
	bgCfg       config.BackgroundConfig
	todoStore   *memory.TodoStore
	usageClient *anthropic.UsageClient
	branchFn    BranchFunc

	mu                sync.Mutex
	lastCacheWarmed   time.Time
	lastInteraction   time.Time
	keepaliveRunning  bool
	backgroundRunning bool

	// Cached mana state
	lastUsagePoll time.Time
	cachedMana    float64 // 0-100 (100 = fully available)
	cachedReset   time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// RunnerConfig holds all the dependencies for creating a Runner.
type RunnerConfig struct {
	AgentID     string
	Keepalive   config.KeepaliveConfig
	Background  config.BackgroundConfig
	TodoStore   *memory.TodoStore
	UsageClient *anthropic.UsageClient
	BranchFunc  BranchFunc
}

// New creates a runner. Call Start() to begin the timer loop.
func New(cfg RunnerConfig) *Runner {
	now := time.Now()
	return &Runner{
		agentID:         cfg.AgentID,
		kaCfg:           cfg.Keepalive,
		bgCfg:           cfg.Background,
		todoStore:       cfg.TodoStore,
		usageClient:     cfg.UsageClient,
		branchFn:        cfg.BranchFunc,
		lastCacheWarmed: now,
		lastInteraction: now,
		done:            make(chan struct{}),
	}
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

	promptText := readPromptFile(r.kaCfg.Prompt)
	if promptText == "" {
		promptText = "[KEEPALIVE] Cache keepalive ping."
	}

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
	r.mu.Unlock()

	if elapsed < interval || running {
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

	promptText := readPromptFile(r.bgCfg.Prompt)
	if promptText == "" {
		promptText = "[BACKGROUND] Check your background todo list and work on the highest priority open item."
	}

	r.mu.Lock()
	r.backgroundRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	log.Infof("keepalive", "firing background work for agent %s (idle %s)", r.agentID, elapsed.Round(time.Second))

	go func() {
		defer func() {
			r.mu.Lock()
			r.backgroundRunning = false
			// Background completion counts as interaction for self-chaining
			r.lastInteraction = time.Now()
			r.mu.Unlock()
		}()
		r.branchFn("background", promptText, true)
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


// readPromptFile reads a prompt from disk, returning empty string on error.
func readPromptFile(path string) string {
	if path == "" {
		return ""
	}
	// Expand ~ to home directory
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		if home != "" {
			path = home + path[1:]
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Debugf("keepalive", "prompt file %s: %v", path, err)
		return ""
	}
	return strings.TrimSpace(string(data))
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

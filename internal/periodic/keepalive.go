// Package periodic provides cache keepalive, background work, and memory formation timers.
//
// Four mechanisms run as a single goroutine on a shared tick (30s by default,
// configurable via [scheduler] tick_interval):
//
//   - Keepalive: fires when the cache hasn't been warmed within the configured interval.
//     Creates a lightweight branch session to keep the Anthropic cache prefix alive.
//
//   - Background work: fires when the user has been idle for the configured interval
//     AND there are open background-tagged todos AND the can_run_background gate allows it.
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
	"foci/internal/skills"

	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/warnings"
	"foci/shared/prompts"
)

const (
	// defaultTickInterval is the production poll cadence for all four periodic
	// timers, used when [scheduler] tick_interval is unset or unparseable. The
	// tick is only a polling cadence — every real threshold is config-driven —
	// so it can be lowered (e.g. by integration tests) without affecting
	// scheduling correctness.
	defaultTickInterval = 30 * time.Second
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

// BackgroundAgent is everything the Runner needs to drive background work on an
// agent. It replaces the former wall of individually-injected closures with one
// honest dependency: "an agent the scheduler can poke between turns". The single
// production implementation is an adapter in cmd/foci-gw; tests use a configurable
// fake. Methods are only invoked once their owning scheduler passes its config and
// gate checks (e.g. RunOnce only when delegated, ResetSession only when a
// reset_time is configured).
type BackgroundAgent interface {
	// Branch dispatches a background prompt as a branch turn (keepalive,
	// background, reflection, consolidation). Returns true if the turn completed.
	Branch(branchType, parentKey, promptText string, noCompact bool) bool
	// HasActiveWork returns the count of in-progress async work (e.g. tmux
	// watches); >0 suppresses background work. 0 = idle.
	HasActiveWork() int
	// DrainRateLimitQueue flushes any queued rate-limited operations; called each tick.
	DrainRateLimitQueue(ctx context.Context)
	// IsTurnInFlight reports whether a turn is executing on the given session base.
	IsTurnInFlight(parentBase string) bool
	// SessionKey returns the session key background operations run against ("" if none).
	SessionKey() string
	// CanFire reports whether a background operation may fire now (rate limit + can_run_background gate).
	CanFire(ctx context.Context, sessionKey string) (allowed bool, reason string)
	// RunOnce executes a one-shot headless prompt (delegated agents only); used by
	// consolidation. Only called when the agent is delegated.
	RunOnce(ctx context.Context, prompt, systemPrompt string) (string, error)
	// ResetSession performs a soft session reset (memory formation + key rotation).
	// Only called when a reset_time is configured.
	ResetSession(ctx context.Context, sessionKey string) error
	// CleanupEphemeralSessions deletes ephemeral (branch/fork) backend transcript
	// files older than retentionDays. Files only. Returns the number deleted.
	// Only called when ephemeral_retention_days > 0.
	CleanupEphemeralSessions(ctx context.Context, retentionDays int) int
}

// Runner manages keepalive, background work, and reflection timers for an agent.
type Runner struct {
	log                *log.ComponentLogger
	agentID            string
	client             provider.Client // for checking caching availability at runtime
	cachingOverride    *bool           // nil=use client.IsCachingAvailable(), non-nil=override
	kaCfg              config.ResolvedKeepalive
	bgCfg              config.ResolvedBackground
	reflectCfg         config.ResolvedReflection
	maintCfg           config.ResolvedMaintenance
	tickInterval       time.Duration // poll cadence for the timer loop (resolved from config)
	promptSearchDirs   []string
	todoStore          *memory.TodoStore
	sessionIndex       *session.SessionIndex

	// openSessionsFn returns the session keys of chats the app currently has open,
	// used by keepalive when warm_open_app_chats is set. nil = feature unavailable
	// (non-app agents) → keepalive warms only the default session.
	openSessionsFn func() []string

	// agent is the single dependency the schedulers drive — Branch, CanFire,
	// IsTurnInFlight, etc. (replaces the former eight injected closures). The
	// in-flight check mirrors the gateway send/branch gate (TODO #753): periodic
	// schedulers dispatch via Branch directly, bypassing the gate, so they need
	// their own in-flight signal to avoid injecting prompts into a busy session
	// as SourceUser follow-ups.
	agent BackgroundAgent

	warningDispatcher     *warnings.Dispatcher
	chatWarningDispatcher *warnings.Dispatcher

	isDelegatedAgent bool // reflection needs quiet period in delegated mode; consolidation uses RunOnce

	// skillDirs are the skill directories to scan for creation/update detection
	// during reflection. nil = feature disabled.
	skillDirs []string
	// notifySkillChange is called with a formatted message when reflection creates
	// or updates a skill. The session key identifies which session's chat to route
	// the notification to. nil = notifications disabled.
	notifySkillChange func(sessionKey, text string)

	mu                  sync.Mutex
	lastCacheWarmed     time.Time
	lastInteraction     time.Time
	keepaliveRunning    bool
	backgroundRunning   bool
	lastBackgroundEnded time.Time // when the last background session finished

	// Reflection state
	lastReflection       time.Time
	lastConsolidation    time.Time
	lastReset            time.Time
	reflectionRunning    bool
	consolidationRunning bool
	resetRunning         bool

	// Ephemeral-session cleanup
	ephemeralRetentionDays int       // 0 = disabled
	lastEphemeralCleanup   time.Time // last daily GC run

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
	Maintenance        config.ResolvedMaintenance
	TickInterval       string   // scheduler poll cadence (Go duration string, default: "30s")
	PromptSearchDirs   []string // directories to search for prompt files (agent workspace, shared)
	TodoStore          *memory.TodoStore
	SessionIndex       *session.SessionIndex

	// EphemeralRetentionDays is the age (days) beyond which ephemeral (branch/
	// fork) backend transcripts are deleted by the daily cleanup. 0 = disabled.
	EphemeralRetentionDays int

	// Agent is the single dependency the schedulers drive. Wired from
	// cmd/foci-gw/periodic_setup.go to an adapter over the agent instance.
	Agent BackgroundAgent

	// OpenSessionsFn returns session keys of the app's currently-open chats.
	// nil for non-app agents (keepalive then warms only the default session).
	OpenSessionsFn func() []string

	WarningDispatcher     *warnings.Dispatcher
	ChatWarningDispatcher *warnings.Dispatcher

	// IsDelegatedAgent indicates this agent uses a delegated transport (CC).
	// When true, reflection requires a quiet period (no recent user activity)
	// because reflection runs IN the live session, and consolidation dispatches
	// via Agent.RunOnce rather than Agent.Branch.
	IsDelegatedAgent bool

	// SkillDirs are the skill directories to scan for creation/update detection
	// during the reflection pass. When non-empty and NotifyOnSkillCreation is
	// true in the reflection config, the runner snapshots skill files before
	// firing reflection, diffs after, and calls NotifySkillChange with the result.
	SkillDirs []string

	// NotifySkillChange is called with a session key and formatted message when
	// reflection creates or updates a skill. The session key routes the
	// notification to the correct chat. nil = no notification.
	NotifySkillChange func(sessionKey, text string)
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
		maintCfg:           cfg.Maintenance,
		promptSearchDirs:   cfg.PromptSearchDirs,
		todoStore:          cfg.TodoStore,
		sessionIndex:       cfg.SessionIndex,
		openSessionsFn:     cfg.OpenSessionsFn,

		ephemeralRetentionDays: cfg.EphemeralRetentionDays,

		agent:                 cfg.Agent,
		warningDispatcher:     cfg.WarningDispatcher,
		chatWarningDispatcher: cfg.ChatWarningDispatcher,
		isDelegatedAgent:      cfg.IsDelegatedAgent,
		skillDirs:             cfg.SkillDirs,
		notifySkillChange:     cfg.NotifySkillChange,
		lastCacheWarmed:       now,
		lastInteraction:       now,
		lastReflection:        now,
		// Like lastReflection, anchor to boot so a FRESH agent waits a full
		// interval before its first consolidation rather than firing one
		// immediately on init (the zero value's Truncate(interval).Add(interval)
		// lands in the distant past). A persisted value below overrides this.
		lastConsolidation: now,
		// Anchor reset to boot so a fresh agent waits for the next scheduled
		// slot rather than resetting immediately on startup (a persisted value
		// below overrides this).
		lastReset: now,
		done:      make(chan struct{}),
	}
	// Resolve the tick cadence: config value if valid, else the 30s default.
	r.tickInterval = defaultTickInterval
	if cfg.TickInterval != "" {
		if d, ok := r.parseDuration("scheduler tick_interval", cfg.TickInterval); ok && d > 0 {
			r.tickInterval = d
		}
	}
	// Restore consolidation timestamp from persistent state
	if cfg.SessionIndex != nil {
		if raw, err := cfg.SessionIndex.GetAgentMetadata(cfg.AgentID, "consolidation_last"); err == nil && raw != "" {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				r.lastConsolidation = ts
			}
		}
		if raw, err := cfg.SessionIndex.GetAgentMetadata(cfg.AgentID, "reset_last"); err == nil && raw != "" {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				r.lastReset = ts
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
	if r.agent == nil {
		return ""
	}
	return r.agent.SessionKey()
}

// parentTurnInFlight reports whether a turn is currently executing on the
// session base derived from parentKey. Returns false if no in-flight callback
// is wired (test runners) or parentKey is empty.
//
// Schedulers consult this before dispatching via branchFn so a periodic prompt
// (reflection, consolidation, internal keepalive, background) doesn't get
// queued behind the active turn as a SourceUser follow-up — see TODO #760
// (companion to TODO #753 which added the same check on the gateway gate path).
func (r *Runner) parentTurnInFlight(parentKey string) bool {
	if r.agent == nil || parentKey == "" {
		return false
	}
	return r.agent.IsTurnInFlight(parentKey)
}

// readyParentKey resolves the default session to dispatch a branch into and
// confirms it isn't mid-turn. It returns the key, or a non-empty skip reason
// (and an empty key) if there is no default session or a turn is in flight. The
// keepalive/background/consolidation/reset schedulers all gate on this.
func (r *Runner) readyParentKey() (parentKey, skip string) {
	parentKey = r.defaultParentKey()
	if parentKey == "" {
		return "", "no default session"
	}
	if r.parentTurnInFlight(parentKey) {
		return "", "turn in flight on parent session"
	}
	return parentKey, ""
}

// checkCanFire applies the agent's rate-limit + can_run_background gate for the
// current default session. Returns "" if the operation may fire, else the skip
// reason. Used by the background/reflection/consolidation schedulers.
func (r *Runner) checkCanFire(ctx context.Context) (skip string) {
	if canFire, reason := r.agent.CanFire(ctx, r.agent.SessionKey()); !canFire {
		return reason
	}
	return ""
}

func (r *Runner) run(ctx context.Context) {
	defer close(r.done)

	// Guard against a zero interval: a Runner built as a struct literal (some
	// tests) bypasses New()'s resolution and leaves tickInterval unset.
	tick := r.tickInterval
	if tick <= 0 {
		tick = defaultTickInterval
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.agent != nil {
				r.agent.DrainRateLimitQueue(ctx)
			}
			r.maybeKeepalive(ctx)
			r.maybeBackgroundWork(ctx)
			// Reflection runs before consolidation so that all the latest
			// memory content is available when consolidation curates MEMORY.md.
			// Consolidation also skips if reflection is still running.
			r.maybeReflection()
			r.maybeConsolidation()
			r.maybeReset(ctx)
			r.maybeEphemeralCleanup(ctx)
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
	if !r.kaCfg.Enabled || r.agent == nil {
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

	targets := r.keepaliveTargets()
	if len(targets) == 0 {
		skip = "no ready session"
		return
	}

	promptText := prompts.ResolvePrompt(r.kaCfg.Prompt, "keepalive.md", prompts.Keepalive(), r.promptSearchDirs...)

	r.mu.Lock()
	r.keepaliveRunning = true
	r.lastCacheWarmed = time.Now()
	r.mu.Unlock()

	r.log.Infof("firing keepalive for agent %s (%d session(s), cache age %s)", r.agentID, len(targets), elapsed.Round(time.Second))

	go func() {
		defer func() {
			r.mu.Lock()
			r.keepaliveRunning = false
			r.mu.Unlock()
		}()
		for _, sk := range targets {
			r.agent.Branch("keepalive", sk, promptText, true)
		}
	}()
}

// keepaliveTargets returns the sessions to warm this cycle, each already filtered
// for an in-flight turn. With warm_open_app_chats and open chats present, it warms
// every open session; otherwise (or when none are open) it warms the default.
func (r *Runner) keepaliveTargets() []string {
	if r.kaCfg.WarmOpenAppChats && r.openSessionsFn != nil {
		var ready []string
		for _, sk := range r.openSessionsFn() {
			if !r.parentTurnInFlight(sk) {
				ready = append(ready, sk)
			}
		}
		if len(ready) > 0 {
			return ready
		}
	}
	parentKey, skip := r.readyParentKey()
	if skip != "" {
		return nil
	}
	return []string{parentKey}
}

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

	// Check availability (rate limit + can_run_background gate)
	if skip = r.checkCanFire(ctx); skip != "" {
		return
	}

	parentKey, skip := r.readyParentKey()
	if skip != "" {
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
		r.agent.Branch("background", parentKey, promptText, true)
	}()
}

func (r *Runner) maybeReflection() {
	if !r.reflectCfg.IntervalEnabled || r.agent == nil {
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

	// Filter out sessions with a turn currently in flight. For delegated
	// agents reflection injects into the main session — firing while the
	// user's turn is mid-flight queues the reflection prompt as a SourceUser
	// follow-up, which is the wrong source attribution and the wrong timing.
	// Defer to the next 30s tick. (TODO #760)
	filtered := keys[:0]
	busy := 0
	for _, k := range keys {
		if r.agent.IsTurnInFlight(k) {
			busy++
			continue
		}
		filtered = append(filtered, k)
	}
	keys = filtered
	if busy > 0 {
		r.log.Debugf("reflection: deferred %d session(s) with in-flight turns", busy)
	}
	if len(keys) == 0 {
		skip = "all candidate sessions have in-flight turns"
		return
	}

	// Check availability (rate limit + can_run_background gate)
	if skip = r.checkCanFire(context.Background()); skip != "" {
		return
	}

	promptText := prompts.ResolvePrompt(r.reflectCfg.IntervalPrompt, "reflection.md", prompts.Reflection(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	r.mu.Lock()
	r.reflectionRunning = true
	r.mu.Unlock()

	r.log.Infof("firing reflection pass for agent %s (%d sessions)", r.agentID, len(keys))

	// Snapshot skill files before reflection so we can detect creation/update
	// after the pass completes.
	var skillBefore skills.SkillSnapshot
	if r.reflectCfg.NotifyOnSkillCreation && len(r.skillDirs) > 0 && r.notifySkillChange != nil {
		skillBefore = skills.Snapshot(r.skillDirs)
	}

	go func() {
		defer func() {
			r.mu.Lock()
			r.reflectionRunning = false
			r.lastReflection = time.Now()
			r.mu.Unlock()
		}()
		for _, key := range keys {
			t := time.Now()
			if r.agent.Branch("reflection", key, promptText, true) {
				r.sessionIndex.StampReflection(key, t)
			}
		}

		// Detect and notify on skill creation/update.
		if skillBefore != nil {
			after := skills.Snapshot(r.skillDirs)
			changes := skills.Diff(skillBefore, after)
			if msg := skills.FormatChanges(changes); msg != "" {
				r.notifySkillChange(keys[0], msg)
			}
		}
	}()
}

// ReflectSessionIfDue fires a single reflection branch for sessionKey iff it is
// due by the same "activity since last reflection" rule the periodic pass uses,
// then stamps it. Used for the final reflection when an app session is archived
// (#app-binding-restore) — wired into the app hub via app.SetReflectOnArchive.
// No-op if the runner has no agent/index, the session isn't due, or no reflection
// prompt resolves.
func (r *Runner) ReflectSessionIfDue(sessionKey string) {
	if r == nil || r.agent == nil || r.sessionIndex == nil {
		return
	}
	if !r.sessionIndex.SessionNeedsReflection(sessionKey) {
		return
	}
	promptText := prompts.ResolvePrompt(r.reflectCfg.IntervalPrompt, "reflection.md", prompts.Reflection(), r.promptSearchDirs...)
	if promptText == "" {
		return
	}

	var skillBefore skills.SkillSnapshot
	if r.reflectCfg.NotifyOnSkillCreation && len(r.skillDirs) > 0 && r.notifySkillChange != nil {
		skillBefore = skills.Snapshot(r.skillDirs)
	}

	t := time.Now()
	if r.agent.Branch("reflection", sessionKey, promptText, true) {
		r.sessionIndex.StampReflection(sessionKey, t)
	}

	if skillBefore != nil {
		after := skills.Snapshot(r.skillDirs)
		changes := skills.Diff(skillBefore, after)
		if msg := skills.FormatChanges(changes); msg != "" {
			r.notifySkillChange(sessionKey, msg)
		}
	}
}

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

	// Check availability (rate limit + can_run_background gate)
	if skip = r.checkCanFire(context.Background()); skip != "" {
		return
	}

	parentKey, skip := r.readyParentKey()
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
func (r *Runner) maybeEphemeralCleanup(ctx context.Context) {
	if r.ephemeralRetentionDays <= 0 || r.agent == nil {
		return
	}
	now := timeutil.Now()
	r.mu.Lock()
	if !r.lastEphemeralCleanup.IsZero() && now.Sub(r.lastEphemeralCleanup) < 24*time.Hour {
		r.mu.Unlock()
		return
	}
	r.lastEphemeralCleanup = now
	r.mu.Unlock()

	if n := r.agent.CleanupEphemeralSessions(ctx, r.ephemeralRetentionDays); n > 0 {
		r.log.Infof("ephemeral cleanup: deleted %d stale transcript(s) older than %dd", n, r.ephemeralRetentionDays)
	}
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

	parentKey, skip := r.readyParentKey()
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

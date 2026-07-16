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
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"

	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/warnings"
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
	// CanFire reports whether a background operation may fire now (rate limit +
	// can_run_background gate). Used ONLY by maybeBackgroundWork.
	CanFire(ctx context.Context, sessionKey string) (allowed bool, reason string)
	// RateLimited reports whether the given session's endpoint is currently
	// rate-limited. The shared gate for every model-calling scheduler
	// (keepalive/reflection/consolidation/reset); does NOT run can_run_background.
	RateLimited(sessionKey string) (limited bool, reason string)
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
	cacheTTL           time.Duration // backend/model prompt-cache lifetime; keepalive upper bound (0 = unknown)
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
	ephemeralRetentionDays  int       // 0 = disabled
	lastEphemeralCleanup    time.Time // last daily GC run
	ephemeralCleanupRunning bool      // guards against overlapping async cleanup runs

	// updateCh delivers live Settings updates into the run loop, so all config
	// mutation happens on the loop goroutine (no locks on the per-tick reads).
	// nil on struct-literal Runners (tests) — a nil channel never selects.
	updateCh chan Settings

	cancel context.CancelFunc
	done   chan struct{}
}

// Settings are the runner knobs that can be updated while it runs (the live
// config-apply path). Everything else on RunnerConfig is fixed at construction.
type Settings struct {
	Keepalive              config.ResolvedKeepalive
	Background             config.ResolvedBackground
	Reflection             config.ResolvedReflection
	Maintenance            config.ResolvedMaintenance
	TickInterval           string // Go duration string; "" = default
	EphemeralRetentionDays int
}

// UpdateSettings hands fresh settings to the run loop. Non-blocking: a pending
// undelivered update is replaced (only the latest state matters).
func (r *Runner) UpdateSettings(s Settings) {
	if r.updateCh == nil {
		return
	}
	for {
		select {
		case r.updateCh <- s:
			return
		default:
			select {
			case <-r.updateCh: // drop the stale pending update
			default:
			}
		}
	}
}

// applySettings installs fresh settings; runs on the loop goroutine only.
func (r *Runner) applySettings(s Settings, ticker *time.Ticker) {
	// warm_open_app_chats is also baked into the OpenSessionsFn closure at
	// construction, so a live update cannot fully take effect — keep the boot
	// value so the two consumers never disagree.
	s.Keepalive.WarmOpenAppChats = r.kaCfg.WarmOpenAppChats
	r.kaCfg = s.Keepalive
	r.bgCfg = s.Background
	r.reflectCfg = s.Reflection
	r.maintCfg = s.Maintenance
	r.ephemeralRetentionDays = s.EphemeralRetentionDays

	newTick := defaultTickInterval
	if s.TickInterval != "" {
		if d, ok := r.parseDuration("scheduler tick_interval", s.TickInterval); ok && d > 0 {
			newTick = d
		}
	}
	if newTick != r.tickInterval {
		r.tickInterval = newTick
		if ticker != nil {
			ticker.Reset(newTick)
		}
	}
	r.log.Infof("live settings applied (ka=%v bg=%v tick=%s)", r.kaCfg.Enabled, r.bgCfg.Enabled, r.tickInterval)
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

	// CacheTTL is the prompt-cache lifetime for this agent's backend/model — a
	// static constant (CC = 1h; API = the model's configured cache_ttl). The
	// keepalive window is [interval, CacheTTL): warm a session that's due but
	// whose cache hasn't yet expired. 0 = unknown → no expiry ceiling (warm on
	// interval alone). Resolved once at setup because it never varies at runtime.
	CacheTTL time.Duration

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
		cacheTTL:           cfg.CacheTTL,
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
		updateCh:  make(chan Settings, 1),
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

// readyGatedParent resolves a ready parent session (readyParentKey) and then
// applies gate to it, in that order — so the gate (which may run an up-to-10s
// can_run_background subprocess) never fires when there's no ready session to
// dispatch on anyway. Returns ("", skip) if there's no ready parent or the gate
// blocks, else (parentKey, ""). The three branch-dispatching schedulers
// (background, consolidation, reset) share this; they differ only in the gate.
func (r *Runner) readyGatedParent(gate func(sessionKey string) string) (parentKey, skip string) {
	parentKey, skip = r.readyParentKey()
	if skip != "" {
		return "", skip
	}
	if skip = gate(parentKey); skip != "" {
		return "", skip
	}
	return parentKey, ""
}

// checkCanFire applies the FULL gate (rate limit + can_run_background) for a
// specific session. Only maybeBackgroundWork uses it — the can_run_background
// script is background-work-only. Returns "" if the operation may fire, else
// the skip reason.
func (r *Runner) checkCanFire(ctx context.Context, sessionKey string) (skip string) {
	if canFire, reason := r.agent.CanFire(ctx, sessionKey); !canFire {
		return reason
	}
	return ""
}

// checkRateLimit is the shared gate for every model-calling scheduler
// (keepalive/reflection/consolidation/reset): it blocks only when the specific
// session's endpoint is rate-limited, and never runs can_run_background.
// Returns "" if clear, else the skip reason.
func (r *Runner) checkRateLimit(sessionKey string) (skip string) {
	if r.agent == nil {
		return ""
	}
	if limited, reason := r.agent.RateLimited(sessionKey); limited {
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
		case s := <-r.updateCh:
			r.applySettings(s, ticker)
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


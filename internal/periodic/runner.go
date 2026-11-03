// Package periodic provides cache keepalive, background work, and memory formation timers.
//
// Five mechanisms run as a single goroutine on a shared tick (30s by default,
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
//
//   - Quiet-hours compaction: during the configured compaction_quiet_hours window,
//     compacts each idle open session whose context usage is at or above
//     compaction_quiet_threshold, once per window occurrence (#2218) — so
//     keepalive stops re-warming a full context nobody is using.
//
//   - Pre-cold compaction (opt-in, #1066): compacts an idle open session just
//     before its prompt cache expires, when keepalive will not warm it again,
//     so the next turn's full cache write is smaller.
package periodic

import (
	"context"
	"sync"
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/skills"

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
//
// ctx bounds the turn: when it ends before the turn completes (a keepalive's
// timeout, #2489), the branch stops the turn and returns.
type BranchFunc func(ctx context.Context, branchType, parentKey, promptText string, noCompact bool) bool

// BackgroundAgent is everything the Runner needs to drive background work on an
// agent. It replaces the former wall of individually-injected closures with one
// honest dependency: "an agent the scheduler can poke between turns". The single
// production implementation is an adapter in cmd/foci-gw; tests use a configurable
// fake. Methods are only invoked once their owning scheduler passes its config and
// gate checks (e.g. RunBatch only when delegated, ResetSession only when a
// reset_time is configured).
type BackgroundAgent interface {
	// Branch dispatches a background prompt as a branch turn (keepalive,
	// background, reflection, consolidation). Returns true if the turn completed.
	// When ctx ends before the turn completes, the turn is stopped.
	Branch(ctx context.Context, branchType, parentKey, promptText string, noCompact bool) bool
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
	// KeepaliveRateLimited is RateLimited for keepalive: a usage hold does
	// not block it, a usage stop does (#2491).
	KeepaliveRateLimited(sessionKey string) (limited bool, reason string)
	// LastTurnEnd returns the wall-clock moment the most recent turn under
	// the session key finished (zero if none is recorded). Read by
	// quiet-hours compaction's idle guard (#2218).
	LastTurnEnd(sessionKey string) time.Time
	// ContextUsage returns the session's context fill and limit the way
	// /status computes them (#2218): fill from the cost ledger (every
	// backend books there; 0 = no turn with a fill recorded), limit from the
	// session's model context window (0 = unknown).
	ContextUsage(sessionKey string) (fill, limit int)
	// QuietCompactBlocked reports why a quiet-hours compaction (#2218) must
	// not run on the session right now, or "" when it may. See
	// Agent.QuietCompactBlocked for the (deliberately strict) rule.
	QuietCompactBlocked(sessionKey string) string
	// QuietCompact runs one scheduled quiet-hours compaction (#2218) through
	// the /compact pipeline. A guards-changed race returns an error wrapping
	// ErrQuietCompactionRefused; a backend decline wraps
	// delegator.ErrCompactionNoBoundary.
	QuietCompact(ctx context.Context, sessionKey string) error
	// RunBatch runs a batch — a headless one-shot turn on an ephemeral child
	// session whose answer comes back here (delegated agents only); used by
	// consolidation. Only called when the agent is delegated.
	RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error)
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
	log              *log.ComponentLogger
	agentID          string
	client           provider.Client // for checking caching availability at runtime
	cachingOverride  *bool           // nil=use client.IsCachingAvailable(), non-nil=override
	kaCfg            config.ResolvedKeepalive
	bgCfg            config.ResolvedBackground
	reflectCfg       config.ResolvedReflection
	maintCfg         config.ResolvedMaintenance
	tickInterval     time.Duration // poll cadence for the timer loop (resolved from config)
	cacheTTL         time.Duration // backend/model prompt-cache lifetime; keepalive upper bound (0 = unknown)
	cacheTTLFor      func(sessionKey string) time.Duration
	promptSearchDirs []string
	// keepaliveScriptTimeout bounds one run of a keepalive prompt script
	// (#1351); 0 = defaultKeepaliveScriptTimeout. Tests shorten it.
	keepaliveScriptTimeout time.Duration
	todoStore              *memory.TodoStore
	sessionIndex           *session.SessionIndex

	// openChatSessionsFn returns the session keys of every app chat the agent
	// has open. Two consumers: quiet-hours compaction takes ALL of them
	// (#2218), and keepalive takes them only while the live
	// kaCfg.WarmOpenAppChats is set (#2641; the flag is read each tick, never
	// baked into this resolver, so it hot-reloads). nil = no app chats
	// (default session only).
	openChatSessionsFn func() []string

	// holdIdleTimeout bounds the wait for a session's interrupted turn to end
	// before the hold's compaction (#2491); 0 = defaultHoldIdleTimeout.
	holdIdleTimeout time.Duration

	// quietCfg is the resolved quiet-hours compaction trigger (#2218).
	// Window "" = off. Installed at construction and by applySettings.
	quietCfg config.ResolvedQuietCompaction

	// precoldCfg is the resolved pre-cold compaction trigger (#1066).
	// Enabled false = off (the default). Installed at construction and by
	// applySettings.
	precoldCfg config.ResolvedPrecoldCompaction

	// clock is the injectable time source for clock-time schedulers
	// (quiet-hours compaction); nil = the real wall clock. The runner
	// converts reads into the process timezone via Runner.now.
	clock clock.Clock

	// notifyQuietCompact posts the one-line quiet-compaction note to the
	// compacted session's chat (#2218) — sent even when compaction_notify is
	// off, because it is the only sign of an unprompted action. nil = no
	// notification.
	notifyQuietCompact func(sessionKey, text string)

	// agent is the single dependency the schedulers drive — Branch, CanFire,
	// IsTurnInFlight, etc. (replaces the former eight injected closures). The
	// in-flight check mirrors the gateway send/branch gate (TODO #753): periodic
	// schedulers dispatch via Branch directly, bypassing the gate, so they need
	// their own in-flight signal to avoid injecting prompts into a busy session
	// as SourceUser follow-ups.
	agent BackgroundAgent

	warningDispatcher     *warnings.Dispatcher
	chatWarningDispatcher *warnings.Dispatcher

	isDelegatedAgent          bool // reflection needs quiet period in delegated mode; consolidation uses RunBatch
	characterSystemPromptFunc func() string

	// skillDirs are the skill directories to scan for creation/update detection
	// during reflection. nil = feature disabled.
	skillDirs *skills.LiveDirs
	// notifySkillChange is called with (sessionKey, skillName, markdown) for a
	// GIT-REPO skill change git-attributed to a commit landing during the
	// reflection window (skills.AttributeToGit) — see #1404. nil = disabled.
	notifySkillChange func(sessionKey, skillName, markdown string)
	// notifySkillChangeText is called with (sessionKey, formatted message) for
	// a NON-git-repo skill change (skills.FormatChanges) — preserves the
	// pre-#1404 plain-text behaviour there exactly (Dick: that case must not
	// change). nil = disabled.
	notifySkillChangeText func(sessionKey, text string)

	mu sync.Mutex
	// lastInteraction is the in-process receipt stamp set by NotifyInteraction
	// (inbound human message or slash command). Zero at boot and never
	// persisted — it only supplements the durable record for interactions that
	// start no turn. Read it via LastUserActivity/sinceUserActivity, never
	// directly (#2023).
	lastInteraction time.Time
	// bootedAt is when New() ran: the last-activity fallback for an agent with
	// no recorded human interaction at all (see sinceUserActivity).
	bootedAt time.Time
	// keepaliveRunning holds the sessions with a keepalive in flight (#2487):
	// each session's keepalive runs on its own, at most one per session.
	keepaliveRunning map[string]bool
	// lastWarmAttempt is, per session, when a scheduler last started work that
	// should warm its cache (a keepalive, or a background branch off it). It is
	// the keepalive debounce (#2509): last_cache_touch moves only on a model
	// response's evidence, so after a failed warm the touch stays old and the
	// session stays in the warm window; this keeps keepalive to one attempt per
	// interval. In memory: a restart allows one early retry. Never read it for
	// warmth: that is lastCacheTouchAge's job. Under mu.
	lastWarmAttempt     map[string]time.Time
	backgroundRunning   bool
	lastBackgroundEnded time.Time // when the last background session finished (persisted, #2026)

	// Reflection state. The last* timers are persisted (persist.go, #2026).
	lastReflection       time.Time
	lastConsolidation    time.Time
	lastReset            time.Time
	reflectionRunning    bool
	consolidationRunning bool
	resetRunning         bool

	// quietCompactionRunning latches a quiet-hours compaction pass in flight
	// (#2218): set synchronously by maybeQuietCompaction, cleared by its
	// dispatch goroutine. maybeKeepalive yields to it — re-warming a full
	// context that is being compacted is the exact waste the trigger exists
	// to remove.
	quietCompactionRunning bool

	// precoldCompactionRunning latches a pre-cold compaction pass in flight
	// (#1066), exactly like quietCompactionRunning; keepalive and quiet
	// compaction yield to it.
	precoldCompactionRunning bool

	// Ephemeral-session cleanup
	ephemeralRetentionDays  int       // 0 = disabled
	lastEphemeralCleanup    time.Time // last daily GC run (persisted, #2026)
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
	QuietCompaction        config.ResolvedQuietCompaction
	PrecoldCompaction      config.ResolvedPrecoldCompaction
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
	r.kaCfg = s.Keepalive
	r.bgCfg = s.Background
	r.reflectCfg = s.Reflection
	r.maintCfg = s.Maintenance
	r.quietCfg = s.QuietCompaction
	r.precoldCfg = s.PrecoldCompaction
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
	AgentID          string
	Client           provider.Client // for checking caching availability at runtime
	CachingOverride  *bool           // nil=use client.IsCachingAvailable(), non-nil=override (for OpenAI/DeepSeek)
	Keepalive        config.ResolvedKeepalive
	Background       config.ResolvedBackground
	Reflection       config.ResolvedReflection
	Maintenance      config.ResolvedMaintenance
	TickInterval     string   // scheduler poll cadence (Go duration string, default: "30s")
	PromptSearchDirs []string // directories to search for prompt files (agent workspace, shared)
	TodoStore        *memory.TodoStore
	SessionIndex     *session.SessionIndex

	// CacheTTL is the prompt-cache lifetime for this agent's backend/model — a
	// static constant (CC = 1h; API = the model's configured cache_ttl). The
	// keepalive window is [interval, CacheTTL): warm a session that's due but
	// whose cache hasn't yet expired. 0 = unknown → no expiry ceiling (warm on
	// interval alone). Resolved once at setup because it never varies at runtime.
	CacheTTL time.Duration

	// CacheTTLFor, when set, gives the lifetime per session and overrides
	// CacheTTL. cmd/foci-gw wires it to agent.SessionCacheTTL — the same
	// resolution the app's warmth indicator uses — returning 0 when unknown.
	CacheTTLFor func(sessionKey string) time.Duration

	// EphemeralRetentionDays is the age (days) beyond which ephemeral (branch/
	// fork) backend transcripts are deleted by the daily cleanup. 0 = disabled.
	EphemeralRetentionDays int

	// Agent is the single dependency the schedulers drive. Wired from
	// cmd/foci-gw/periodic_setup.go to an adapter over the agent instance.
	Agent BackgroundAgent

	// OpenChatSessionsFn returns session keys of the app's currently-open
	// chats: quiet-hours compaction candidates (#2218), and keepalive's
	// targets while warm_open_app_chats is on (#2641). Wired unconditionally;
	// the keepalive gate is the live config, not this func's nil-ness. nil =
	// no app chats (the default session only).
	OpenChatSessionsFn func() []string

	// QuietCompaction is the resolved quiet-hours compaction trigger (#2218).
	// Window "" = off (the default). Hot-reloadable via Settings.
	QuietCompaction config.ResolvedQuietCompaction

	// PrecoldCompaction is the resolved pre-cold compaction trigger (#1066).
	// Enabled false = off (the default). Hot-reloadable via Settings.
	PrecoldCompaction config.ResolvedPrecoldCompaction

	// Clock is the time source for clock-time schedulers (quiet-hours
	// compaction, #2218). nil = the real wall clock; tests inject a
	// *clock.Fake so window membership never depends on the wall clock.
	Clock clock.Clock

	// NotifyQuietCompact is called with (sessionKey, text) after each
	// successful quiet-hours (#2218) or pre-cold (#1066) compaction to post
	// the one-line note to the session's chat. nil = no notification.
	NotifyQuietCompact func(sessionKey, text string)

	WarningDispatcher     *warnings.Dispatcher
	ChatWarningDispatcher *warnings.Dispatcher

	// IsDelegatedAgent indicates this agent uses a delegated transport (CC).
	// When true, reflection requires a quiet period (no recent user activity)
	// because reflection runs IN the live session, and consolidation dispatches
	// via Agent.RunBatch rather than Agent.Branch.
	IsDelegatedAgent bool

	// CharacterSystemPromptFunc composes the agent's character files for the
	// system prompt of delegated batch runs (consolidation via RunBatch) —
	// the same corpus a branch session inherits on the API path. nil = no
	// character context (the pre-#1310 behaviour).
	CharacterSystemPromptFunc func() string

	// SkillDirs are the skill directories to scan for creation/update detection
	// during the reflection pass. When non-empty and NotifyOnSkillCreation is
	// true in the reflection config, the runner snapshots skill files before
	// firing reflection and diffs after. Each changed skill is split on
	// whether its dir is a git repo (skills.SplitByGitRepo — #1404): a
	// non-git-repo change fires NotifySkillChangeText unconditionally (the
	// pre-#1404 behaviour, unchanged); a git-repo change is gated on a commit
	// landing in the reflection window (skills.AttributeToGit) and, if
	// matched, fires NotifySkillChange.
	SkillDirs *skills.LiveDirs

	// NotifySkillChange is called with (sessionKey, skillName, markdown) for
	// each GIT-REPO skill change git-attributed to a commit within the
	// reflection window. markdown is the commit message(s) + diff of the
	// touched files, meant for delivery as a document attachment. nil = no
	// notification.
	NotifySkillChange func(sessionKey, skillName, markdown string)

	// NotifySkillChangeText is called with (sessionKey, formatted message) for
	// each NON-git-repo skill change (skills.FormatChanges) — preserves the
	// pre-#1404 plain-text notification exactly for that case. nil = no
	// notification.
	NotifySkillChangeText func(sessionKey, text string)
}

// New creates a runner. Call Start() to begin the timer loop.
func New(cfg RunnerConfig) *Runner {
	now := time.Now()
	r := &Runner{
		log:              log.NewComponentLogger("keepalive:" + cfg.AgentID),
		agentID:          cfg.AgentID,
		client:           cfg.Client,
		cachingOverride:  cfg.CachingOverride,
		kaCfg:            cfg.Keepalive,
		bgCfg:            cfg.Background,
		reflectCfg:       cfg.Reflection,
		maintCfg:         cfg.Maintenance,
		cacheTTL:         cfg.CacheTTL,
		cacheTTLFor:      cfg.CacheTTLFor,
		promptSearchDirs: cfg.PromptSearchDirs,
		todoStore:        cfg.TodoStore,
		sessionIndex:     cfg.SessionIndex,

		openChatSessionsFn: cfg.OpenChatSessionsFn,
		quietCfg:           cfg.QuietCompaction,
		precoldCfg:         cfg.PrecoldCompaction,
		clock:              cfg.Clock,
		notifyQuietCompact: cfg.NotifyQuietCompact,

		ephemeralRetentionDays: cfg.EphemeralRetentionDays,

		agent:                     cfg.Agent,
		warningDispatcher:         cfg.WarningDispatcher,
		chatWarningDispatcher:     cfg.ChatWarningDispatcher,
		isDelegatedAgent:          cfg.IsDelegatedAgent,
		characterSystemPromptFunc: cfg.CharacterSystemPromptFunc,
		skillDirs:                 cfg.SkillDirs,
		notifySkillChange:         cfg.NotifySkillChange,
		notifySkillChangeText:     cfg.NotifySkillChangeText,
		bootedAt:                  now,
		// Boot anchors below apply only to a FRESH agent: restoreTimers
		// replaces each with its persisted value when one exists, so a restart
		// neither delays (re-anchoring to boot) nor skips (zero) a schedule.
		// A fresh agent waits a full interval before its first reflection.
		lastReflection: now,
		// Like lastReflection, anchor to boot so a FRESH agent waits a full
		// interval before its first consolidation rather than firing one
		// immediately on init (the zero value's Truncate(interval).Add(interval)
		// lands in the distant past).
		lastConsolidation: now,
		// Anchor reset to boot so a fresh agent waits for the next scheduled
		// slot rather than resetting immediately on startup.
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
	// Restore every restart-sensitive timer from agent_metadata (#2026).
	r.restoreTimers()
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

// NotifyInteraction records an inbound human interaction (message or slash
// command received) in the in-process receipt stamp. See LastUserActivity.
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

// checkKeepaliveRateLimit is checkRateLimit for keepalive (#2491): it lets a
// keepalive through a usage hold.
func (r *Runner) checkKeepaliveRateLimit(sessionKey string) (skip string) {
	if r.agent == nil {
		return ""
	}
	if limited, reason := r.agent.KeepaliveRateLimited(sessionKey); limited {
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
			// ORDER IS LOAD-BEARING. Reflection runs before consolidation so
			// that all the latest memory content is available when
			// consolidation curates MEMORY.md (consolidation also skips if
			// reflection is still running), and reset runs after both because
			// it defers to them too. All three run before quiet compaction
			// and keepalive so that both consumers' "another task running"
			// checks see their flags on the SAME tick: each maybeX sets its
			// running flag synchronously and then dispatches in a goroutine,
			// so ordering here decides who wins. Quiet compaction runs after
			// the three memory passes (it yields to them — compacting a
			// session whose memory is being formed would race the pass) and
			// BEFORE keepalive so keepalive's yield check sees
			// quietCompactionRunning on the same tick: without it keepalive
			// would race the compaction and re-warm the full context the
			// compaction is about to shrink — the exact waste #2218 removes.
			// Pre-cold compaction (#1066) sits right after quiet compaction
			// for the same reason, and yields to it (quiet runs first, so
			// its flag is visible on the same tick).
			r.maybeReflection()
			r.maybeConsolidation()
			r.maybeReset(ctx)
			r.maybeQuietCompaction(ctx)
			r.maybePrecoldCompaction(ctx)
			r.maybeKeepalive(ctx)
			r.maybeBackgroundWork(ctx)
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

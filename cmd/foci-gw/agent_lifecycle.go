package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"foci/internal/agent"
	"foci/internal/agentrename"
	"foci/internal/app"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/convo"
	"foci/internal/delegator"
	"foci/internal/execguard"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/session"
	"foci/internal/skills"
	"foci/internal/voice"
)

// agentEnv is everything one agent's build needs from the running gateway.
// Boot builds every configured agent through it (main's per-agent loop), and
// `/agents new` builds the new agent through the same buildAgent (#963), so
// the two cannot drift apart.
type agentEnv struct {
	cfg             *config.Config // the startup config: frozen, read-only (liveapply.go rule)
	configPath      string
	clients         *clientRegistry
	si              sessionInfra
	sec             secretsResult
	mem             *memorySystem
	tts             *ttsRegistry
	sttMap          map[string]voice.STT
	sttStream       *sttStreamProvider
	braveKey        string
	gwSocketPath    string
	skillLoader     *skills.Loader
	globalTurnLimit *agent.TurnLimiter
	liveSessions    *agent.LiveSessionLimiter // gateway-wide live-session count and cap (#2280): one pointer shared by boot and hot-added agents
	wsFileMode      os.FileMode
	startTime       time.Time
	ctx             context.Context
	connMgr         platform.ConnectionManager
	plat            *platform.Messaging
	agentListFn     func() []command.AgentInfo
	agentResolverFn func(agentID string) *agentInstance
	// startAgentFn is the `/agents new` wizard's live start
	// (liveAgentStarter.Start); nil until main has built the starter.
	startAgentFn func(agentID string) ([]string, error)
	// renameAgentFn is /agents rename's live rename (#2787); nil until main
	// has built the starter.
	renameAgentFn command.AgentRenameFn
	// gracefulTimeout bounds how long a live stop waits for the agent's
	// in-flight turns ([http] graceful_shutdown_timeout, as at shutdown).
	gracefulTimeout time.Duration
}

// ensureAgentWorkspace creates the agent's workspace directories that the
// memory stores and logs open files in.
func ensureAgentWorkspace(acfg config.AgentConfig) error {
	for _, sub := range []string{".data", "memory"} {
		dir := filepath.Join(acfg.Workspace, sub)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create workspace dir %s: %w", dir, err)
		}
	}
	return nil
}

// buildAgent builds one agent: character-file seeding, setupAgent (tools,
// commands, platform connections), the restored session overrides, the
// periodic runner and every per-agent callback. Its memory must already be
// in e.mem. Returns nil when setup fails; nothing is started then.
//
// The result is NOT registered and its inbox is NOT started: the caller does
// both (boot after its loop, a hot-add before publishing it).
func (e *agentEnv) buildAgent(acfg config.AgentConfig) *agentInstance {
	// Seed default character files for agents without explicit system_files.
	// This ensures non-provisioned agents (added with just id= in config)
	// get the same default character files as provisioned ones.
	if len(acfg.System.SystemFiles) == 0 {
		sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared")
		if err := provision.SeedCharacterFiles(sharedDir, acfg.Workspace, e.wsFileMode); err != nil {
			mainLog.Warnf("agent %q: seed character files: %v", acfg.ID, err)
		}
	}

	am := e.mem.forAgent(acfg.ID)
	// The agent's own context: everything its build starts runs under it,
	// so a live stop of this agent ends it all (#2787) while the gateway
	// context, and every other agent, runs on.
	actx, cancel := context.WithCancel(e.ctx)
	inst := setupAgent(setupParams{
		acfg:            acfg,
		cfg:             e.cfg,
		configPath:      e.configPath,
		clientProvider:  e.clients,
		sessions:        e.si.sessions,
		store:           e.sec.store,
		bwStore:         e.sec.bwStore,
		memBackends:     am.backends,
		convReader:      e.mem.convReader,
		reminderStore:   am.reminders,
		scratchpadStore: am.scratchpad,
		todoStore:       am.todos,
		taskListStore:   am.taskList,
		sessionIndex:    e.si.sessionIndex,
		tts:             e.tts,
		sttMap:          e.sttMap,
		sttStream:       e.sttStream,
		braveKey:        e.braveKey,
		gwSocketPath:    e.gwSocketPath,
		skillLoader:     e.skillLoader,
		globalTurnLimit: e.globalTurnLimit,
		liveSessions:    e.liveSessions,
		startTime:       e.startTime,
		ctx:             actx,
		agentListFn:     e.agentListFn,
		agentResolverFn: e.agentResolverFn,
		startAgentFn:    e.startAgentFn,
		renameAgentFn:   e.renameAgentFn,
		connMgr:         e.connMgr,
		plat:            e.plat,
	})
	if inst == nil {
		cancel()
		return nil
	}
	inst.cancel = cancel

	// Restore per-session model/effort/thinking/etc. overrides for every
	// session (root AND branch) that has persisted metadata. Querying
	// session_metadata directly — rather than walking connMgr's
	// connections — is transport-agnostic: conn.DefaultSessionKey() only
	// identifies a session for bots with one fixed default chat
	// (Telegram); the app transport has no connection-level default
	// (appConn.bound is only set per-request), so that walk silently
	// skipped restoration for every app-based agent.
	if e.si.sessionIndex != nil {
		keys, err := e.si.sessionIndex.AgentSessionKeysWithMetadata(inst.id)
		if err != nil {
			mainLog.Warnf("agent %q: list session keys with metadata: %v", acfg.ID, err)
		}
		for _, sk := range keys {
			inst.ag.RestoreSessionOverrides(sk)
		}
	}

	setupPeriodic(inst, acfg, periodicParams{
		cfg:                   e.cfg,
		sessions:              e.si.sessions,
		connMgr:               e.connMgr,
		sessionIndex:          e.si.sessionIndex,
		todoStore:             am.todos,
		ctx:                   actx,
		resolveEndpointClient: e.clients.ResolveEndpointClient,
		endpoints:             e.clients.Endpoints,
	})

	// Wire lifecycle callbacks to the periodic runner. OnUserMessage is a
	// message-receipt event and stays on the platform. The turn-end hook lives
	// on the Agent (fired in HandleMessage) so system-injected turns fire it
	// too, not just platform-driven turns. onTurnComplete is nil: cache warmth
	// is tracked per-session via session_index.last_cache_touch (stamped by
	// recordTurnActivity), which keepalive reads directly.
	if inst.kaRunner != nil {
		e.plat.SetLifecycleCallback(acfg.ID, platform.OnUserMessage,
			func() { inst.kaRunner.NotifyInteraction() })
		inst.ag.SetTurnLifecycleHooks(nil, inst.kaRunner.NotifyTurnEnd)
	}

	// Refresh the app client's cache-warmth indicator on ANY last_cache_touch
	// change (turn entry, branch-warms-root, reset) — not only turns that
	// complete through a live app sink (#1217).
	inst.ag.SetOnCacheExpiry(app.SetCacheExpiry)
	// Show the backend's predicted next prompt in the app's composer (#2916).
	inst.ag.SetOnPromptSuggestion(app.DeliverPromptSuggestion)
	inst.ag.SetOnPromptSuggestionStale(app.ClearPromptSuggestion)
	inst.ag.SetOnSubagentActivity(app.NoteSubagentActivity)
	inst.ag.SetPromptSuggestionSurface(app.CanShowPromptSuggestion)
	// Show each chat's pending wake reminders in the app, kept current on
	// every set, fire, cancel and run (#2600).
	app.SetReminderStore(acfg.ID, am.reminders, inst.ag.SystemInputHeld)
	// Push a backend-generated alias (Codex thread name) to the app at once,
	// not on its next reconnect (#2157).
	inst.ag.SetOnAliasChanged(app.NotifyAliasChanged)

	mainLog.Infof("agent %q ready (model=%s, workspace=%s)", acfg.ID, inst.ag.Model, acfg.Workspace)
	return inst
}

// armFirstRun arms first-run onboarding for an agent that has not completed
// it: the prompt is stored so it gets prepended to the first turn that builds
// a message (API or claude-code). first_run_completed is marked only when the
// message is ACTUALLY consumed (OnFirstRunConsumed), not on generic activity:
// a generic OnActivity callback fired on any first turn, including internal
// ones that never delivered the onboarding, marking it done while it was
// still pending — silently losing onboarding on claude-code agents (#853).
// Boot arms every agent (handleRestartAndFirstRun); a hot-add arms the new
// agent before it can take a turn.
func armFirstRun(inst *agentInstance, sessionIndex *session.SessionIndex) {
	msg := checkFirstRun(sessionIndex, inst.agentCfg)
	if msg == "" {
		return
	}
	inst.ag.FirstRunMessage.Store(msg)
	agentID := inst.id
	inst.ag.OnFirstRunConsumed = func() {
		if err := sessionIndex.SetAgentMetadata(agentID, "first_run_completed", "true"); err != nil {
			mainLog.Errorf("set first_run_completed for %s: %v", agentID, err)
		}
		mainLog.Infof("first-run onboarding completed for %s", agentID)
	}
}

// agentRestorePhases are the per-agent restores that need the agent's chats
// up, in the order they must run. Boot runs each phase across every agent
// after StartAll; a hot-add runs them for the new agent after its
// connections start.
var agentRestorePhases = []func(*agentInstance){
	// Pick up the background spawns the last process left running, or whose
	// results it could not deliver (#2589). After StartAll (the resumed runs
	// and deliveries need chats), and before RestoreUndelivered: a resumed
	// spawn takes its branch's unconsumed inputs into its resume turn.
	func(inst *agentInstance) {
		if r := inst.ag.SpawnRestore; r != nil {
			r()
		}
	},
	// Rebuild the depth-mode trees (#2588): leaf views into their root
	// conversations, first turns and fan-ins the last process left undone.
	// After StartAll (the root bindings must exist), before
	// RestoreUndelivered (a leaf's redelivered input needs its leaf view).
	func(inst *agentInstance) {
		if r := inst.ag.DepthRestore; r != nil {
			r()
		}
	},
	// Resume the user-facing sessions whose turn a restart cut, and run the
	// user messages the last process accepted but never started (#2999).
	// After SpawnRestore and DepthRestore (spawns and depth tasks resume by
	// their own records, and write no cut_turn record), and before
	// RestoreUndelivered, so the resume line is the first input the session
	// sees after the restart.
	func(inst *agentInstance) {
		inst.ag.RestoreCutTurns()
	},
	// Re-send inputs a backend never confirmed consuming before the last
	// shutdown or crash (#2050). After StartAll, so the redelivered turns'
	// replies have chats to land in.
	func(inst *agentInstance) {
		if dm := inst.ag.DelegatedManager; dm != nil {
			dm.RestoreUndelivered()
		}
	},
	// Put back the input a usage hold had parked (#2491): it waits for the
	// lift again, or runs now if the hold lifted while foci was down. After
	// RestoreUndelivered, so input the backend already had runs first.
	func(inst *agentInstance) {
		inst.ag.RestoreUsageHeld()
	},
	// Resume the background-work turns a usage hold had paused (#2491): each
	// waits for the lift, then goes on with the resume line on its session.
	func(inst *agentInstance) {
		inst.ag.RestorePausedBackground(func(ctx context.Context, pb agent.PausedBackground) {
			resumePausedBackground(inst, ctx, pb)
		})
	},
	// Tell each agent about asks that expired across the restart (#1894). The
	// ask tool's restore runs inside setupAgent, before the agent is
	// registered (so agentResolverFn can't route to it) and before its inbox
	// or chats are up, so it holds these until now.
	func(inst *agentInstance) {
		if r := inst.ag.AskRouter; r != nil && r.DeliverRestoreNotices != nil {
			r.DeliverRestoreNotices()
		}
	},
}

// hotAddBotConflict refuses a new agent whose Telegram or Discord bot token
// is already used by a running agent — the boot rule
// (config.DetectBotTokenConflicts, buildBotConflictSkipSet), with the running
// agents first so the new agent is the one skipped. The error names the
// platform and the agent it clashes with.
func hotAddBotConflict(running []config.AgentConfig, acfg config.AgentConfig, secrets config.SecretGetter) error {
	all := append(slices.Clone(running), acfg)
	skip := buildBotConflictSkipSet(config.DetectBotTokenConflicts(all, secrets))
	if reason, ok := skip[acfg.ID]; ok {
		return errors.New(reason)
	}
	return nil
}

// missingTelegramTokenHint returns the wizard hint for a new agent that runs
// without its Telegram bot (Dick, 2026-10-11: app/HTTP only, add the secret
// and /restart), or "" when Telegram is not configured for it or its token
// resolves.
func missingTelegramTokenHint(acfg config.AgentConfig, activePlatforms []string, secrets config.SecretGetter) string {
	if !slices.Contains(activePlatforms, "telegram") {
		return ""
	}
	tg := acfg.Platform("telegram")
	if tg == nil || tg.Bot == "" {
		return ""
	}
	if config.ResolveBotToken(tg.Bot, tg.BotSecret, secrets) != "" {
		return ""
	}
	key := tg.BotSecret
	if key == "" {
		key = "telegram." + tg.Bot
	}
	return fmt.Sprintf("Telegram: no bot token, so it runs on the app and HTTP only. Add it with `/secrets set %s <token>`, then /restart to connect Telegram.", key)
}

// liveAgentStarter starts an agent that `/agents new` has just written to
// foci.toml, in the running process, with no restart (#963). Start runs one
// hot-add at a time.
//
// The order is what keeps a failed start from leaving half state: the id is
// reserved (blocking a second start of it) but invisible; the config block
// is read and checked; the agent is built and its inbox started OFF the
// registry; only then is it published (commit) and its bots started. A
// failure before commit releases the reservation, so no reader ever sees the
// agent and none of its bots runs.
type liveAgentStarter struct {
	agents *agentRegistry

	// load reads foci.toml afresh (never the startup config).
	load func() (*config.Config, error)
	// prepare finishes the new block the way boot finishes every block:
	// provider platform defaults and the auto-approve substitutability filter.
	prepare func(acfg *config.AgentConfig)
	// conflict refuses a duplicate bot token (hotAddBotConflict).
	conflict func(acfg config.AgentConfig) error
	// build builds the agent off-registry, inbox started and first run
	// armed. On error it has undone what it opened. notes are hint lines for
	// the wizard reply.
	build func(acfg config.AgentConfig) (inst *agentInstance, notes []string, err error)
	// start runs after the agent is published: its bots, the app roster
	// push, the restore phases.
	start func(inst *agentInstance)

	// The live stop and rename (#2787, agent_rename_live.go). teardown stops
	// one unregistered agent (agentEnv.stopLive); renameDisk is the disk
	// rename (agentrename.Run); renamePlatforms re-points the platforms'
	// per-conversation state (Messaging.RenameAgent); renameNote is a reply
	// hint about the agent to rename ("" for none).
	teardown        func(inst *agentInstance)
	renameDisk      func(ctx context.Context, o agentrename.Options) error
	renamePlatforms func(old, new string)
	renameNote      func(old string) string

	// mu serialises every live start, stop and rename.
	mu     sync.Mutex
	closed bool
}

// errShuttingDown refuses a hot-add once shutdown has begun.
var errShuttingDown = errors.New("foci is shutting down")

// Start starts the agent with this id from foci.toml. It returns hint lines
// for the wizard reply (e.g. a missing Telegram token), or an error naming
// why the agent did not start, in which case nothing of it is registered or
// running.
func (s *liveAgentStarter) Start(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errShuttingDown
	}
	return s.startAt(id, -1)
}

// startAt is Start under s.mu, with the agent placed at slot in the agent
// order (-1: at the end). A live rename starts the new id in the old one's
// slot, so the default (first) agent stays the default.
func (s *liveAgentStarter) startAt(id string, slot int) ([]string, error) {
	if err := s.agents.reserve(id); err != nil {
		return nil, err
	}
	// id runs again under its own name: a rename recorded under it (an
	// earlier agent renamed away from id) no longer applies.
	gwAgentRenames.forget(id)
	published := false
	defer func() {
		if !published {
			s.agents.release(id)
		}
	}()

	fresh, err := s.load()
	if err != nil {
		return nil, fmt.Errorf("read foci.toml: %w", err)
	}
	idx := slices.IndexFunc(fresh.Agents, func(a config.AgentConfig) bool { return a.ID == id })
	if idx < 0 {
		return nil, fmt.Errorf("foci.toml has no [[agents]] block with id %q", id)
	}
	acfg := fresh.Agents[idx]
	s.prepare(&acfg)
	if err := s.conflict(acfg); err != nil {
		return nil, err
	}
	inst, notes, err := s.build(acfg)
	if err != nil {
		return nil, err
	}
	if err := s.agents.commitAt(inst, slot); err != nil {
		return nil, err
	}
	published = true
	s.start(inst)
	mainLog.Infof("agent %q started live (no restart)", id)
	return notes, nil
}

// close refuses every later Start, after any Start in progress has finished,
// so shutdown's snapshot of the registry includes it.
func (s *liveAgentStarter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// newLiveAgentStarter wires the production hot-add onto the running gateway.
func newLiveAgentStarter(e *agentEnv, agents *agentRegistry, load func() (*config.Config, error), warnHook *warningHook) *liveAgentStarter {
	return &liveAgentStarter{
		agents: agents,
		load:   load,
		prepare: func(acfg *config.AgentConfig) {
			config.ApplyAgentProviderDefaults(e.cfg, acfg, providerPlatformDefaults)
			config.DropSubstitutableAgentAutoApproveRules(acfg, execguard.Live())
		},
		conflict: func(acfg config.AgentConfig) error {
			return hotAddBotConflict(agents.Configs(), acfg, e.sec.store)
		},
		build: func(acfg config.AgentConfig) (*agentInstance, []string, error) {
			return e.buildLive(acfg)
		},
		start: func(inst *agentInstance) {
			e.plat.StartAgent(e.ctx, inst.id)
			for _, phase := range agentRestorePhases {
				phase(inst)
			}
			warnHook.install()
		},
		teardown: func(inst *agentInstance) {
			e.stopLive(inst, e.gracefulTimeout)
		},
		renameDisk: renameDiskFn(e.configPath, delegator.RegisteredNames()),
		renamePlatforms: func(old, new string) {
			e.plat.RenameAgent(old, new, renameKeyFn(old, new))
		},
		renameNote: sharedFacetNote(e.cfg, agents),
	}
}

// buildLive is a hot-add's build: the workspace, the conversation log, the
// memory system and buildAgent — the boot steps for one agent — then the
// first-run arming and the inbox start that boot does after its loop.
func (e *agentEnv) buildLive(acfg config.AgentConfig) (*agentInstance, []string, error) {
	if err := ensureAgentWorkspace(acfg); err != nil {
		return nil, nil, err
	}
	closeConvo := func() {}
	if config.DerefBool(e.cfg.Logging.ConversationLog) {
		c, err := convo.OpenAgent(acfg.ID, config.AgentDataPath(acfg.Workspace, "conversation.db"))
		if err != nil {
			return nil, nil, fmt.Errorf("open conversation log: %w", err)
		}
		closeConvo = c
	}
	memNote, err := e.mem.addAgent(e.cfg, acfg)
	if err != nil {
		closeConvo()
		return nil, nil, fmt.Errorf("memory: %w", err)
	}
	inst := e.buildAgent(acfg)
	if inst == nil {
		closeConvo()
		return nil, nil, errors.New("agent setup failed (the foci log says why)")
	}
	armFirstRun(inst, e.si.sessionIndex)
	inst.ag.StartInbox(e.ctx)

	var notes []string
	if h := missingTelegramTokenHint(acfg, e.plat.ActivePlatformNames(), e.sec.store); h != "" {
		notes = append(notes, h)
	}
	if memNote != "" {
		notes = append(notes, memNote)
	}
	return inst, notes, nil
}

// providerPlatformDefaults is the platform defaulter boot passes to
// config.ApplyProviderDefaults: each registered provider's code defaults.
func providerPlatformDefaults(id string) *config.PlatformConfig {
	p := platform.GetProvider(id)
	if p == nil {
		return nil
	}
	defaults := p.DefaultPlatformConfig()
	return &defaults
}

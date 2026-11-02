package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "foci/internal/delegator/all" // register every delegated backend
	_ "foci/internal/discord"       // register discord messaging provider
	_ "foci/internal/slack"         // register slack messaging provider
	_ "foci/internal/telegram"      // register telegram messaging provider

	"foci/internal/agent"
	"foci/internal/app" // registers the app (FAP WebSocket) messaging provider via init; also SetCacheExpiry
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/configcheck"
	"foci/internal/defersend"
	"foci/internal/delegator"
	"foci/internal/execguard"
	"foci/internal/fociroot"
	"foci/internal/log"
	"foci/internal/modelcaps"
	"foci/internal/modelinfo"
	"foci/internal/platform"
	"foci/internal/preload"
	"foci/internal/procx"
	"foci/internal/secrets"
	"foci/internal/shellenv"
	"foci/internal/skills"
	"foci/internal/startup"
	"foci/internal/tempdir"
	"foci/internal/timeutil"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

func printVersion() {
	fmt.Printf("foci-gw %s (commit %s, built %s, %s)\n", version, gitCommit, buildTime, goVersion)
}

func main() {
	// Taken before anything can log: the startup rotation archives only lines
	// stamped before this, so this lifetime's early lines (config warnings,
	// shellenv, preload) stay in the live log (#1869). Truncated to the
	// second because foci.log timestamps are whole seconds — an unrounded
	// cutoff would archive this process's own first-second lines.
	processStart := time.Now().Truncate(time.Second)

	// Handle --version / -v / version before any flag parsing.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			printVersion()
			os.Exit(0)
		case "ledger-migrate":
			os.Exit(runLedgerMigrateDryRun(os.Args[2:], os.Stdout, os.Stderr))
		case "ledger-shadow":
			os.Exit(runLedgerShadow(os.Args[2:], os.Stdout, os.Stderr))
		}
	}

	// Custom usage for -h / --help.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `foci-gw — Foci gateway server

Usage: foci-gw [flags]
       foci-gw version
       foci-gw ledger-migrate -from <api.db> [-out <path>]
       foci-gw ledger-shadow -live <api.db> -shadow <api-shadow.db> [-since <time|duration>]

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Subcommands:
  version          Print version information
  ledger-migrate   Dry-run the cost ledger migration (#2111) on a copy of an api.db
  ledger-shadow    Compare the shadow cost ledger with the live one (#2111)
`)
	}

	// Handle "foci-gw auth" subcommand — use "foci auth" instead.
	if len(os.Args) >= 2 && os.Args[1] == "auth" {
		fmt.Fprintf(os.Stderr, "Use 'foci auth' to manage credentials, or 'foci secrets set <key> <value>' directly.\n")
		os.Exit(1)
	}

	configPath, err := config.ParseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "foci-gw: %v\n", err)
		os.Exit(2)
	}

	// Early log init: open the default event log file so that config parse
	// errors are captured on disk, not just stderr/journal.
	//
	// FOCI_LOG_FILE override exists so the L2 integration harness can pin
	// foci-gw's log to a tempdir BEFORE config load. Without this, every
	// test-spawned foci-gw would open ~/logs/foci.log — which on dev hosts
	// is the same file the production foci is actively writing to —
	// causing rotation interleaving and production-log corruption.
	earlyLogPath := os.Getenv("FOCI_LOG_FILE")
	if earlyLogPath == "" {
		earlyLogPath = config.ResolvePath("logs/foci.log")
	}
	if err := log.Init(log.Config{
		EventFile: earlyLogPath,
	}); err != nil {
		log.Fatalf("main", "early log init: %v", err)
	}

	// Install root separate from HOME (#2580, the macOS LaunchAgent sets
	// FOCI_HOME and leaves HOME as the user's): give the Claude Code foci runs
	// its own config folder under the install root, so it does not share the
	// user's interactive ~/.claude. Set once here, before anything spawns:
	// every child inherits it, and the ccstream transcript readers resolve
	// the same folder through fociroot.CCConfigDir. A no-op on Linux.
	if dir, err := fociroot.EnsureCCConfigEnv(); err != nil {
		mainLog.Warnf("set CLAUDE_CONFIG_DIR for the separate install root: %v", err)
	} else if dir != "" {
		root, _ := fociroot.Dir()
		mainLog.Infof("install root %s is separate from HOME: Claude Code runs with CLAUDE_CONFIG_DIR=%s", root, dir)
	}

	modelinfo.UnpricedModelHook = func(model string) {
		modelinfoLog.Warnf("no pricing for model %q — its calls are unpriced (cost unknown, not zero); add it to the registry or a family match in internal/modelinfo", model)
	}
	modelinfo.AmbiguousModelHook = func(bare string) {
		modelinfoLog.Warnf("ambiguous model %q — multiple registry entries share this leaf id and the lookup couldn't disambiguate by dev/provider; picked deterministically. Qualify the model id with its dev (e.g. openrouter/<dev>/%s)", bare, bare)
	}
	modelinfo.FamilyPricedModelHook = func(model string) {
		modelinfoLog.Warnf("no exact price for %q — inheriting its family's rates; verify against the pricing page and add a models.jsonl row", model)
	}
	// Rows of the embedded models.jsonl that modelinfo skipped at load instead
	// of panicking (#2583). A model whose only row was skipped is then
	// unpriced or family-priced, and the hooks above say so on first use.
	for _, r := range modelinfo.BuiltInRejected() {
		modelinfoLog.Warnf("skipped a models.jsonl row modelinfo rejects — %s", r)
	}

	cfg, err := config.Load(configPath, delegator.RegisteredNames())
	if err != nil {
		log.Fatalf("main", "load config: %v", err)
	}
	// config.Load has no global side effects (#2356): the daemon applies the
	// process-wide parts of the config itself.
	log.DebugLogKeySuffix = config.DerefBool(cfg.Debug.LogAPIKeySuffix)
	config.ApplyModelInfo(cfg.ModelInfo)

	// ========== Timezone ==========
	// Configure timeutil before any logging or agent init uses it.
	if cfg.Timezone != "" {
		tz, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.Fatalf("main", "load timezone %q: %v", cfg.Timezone, err)
		}
		timeutil.SetLocation(tz)
		mainLog.Infof("timezone set to %s", cfg.Timezone)
	}

	// Capture the operator's shell environment ONCE, as a value, before any
	// backend spawns. procx.SetOperatorEnv is what makes it reachable BY NAME
	// from every operator-facing spawn site and from internal/execguard, so
	// the environment agents get and the environment the guard predicts are
	// the same object rather than two lists that can drift (#1914).
	// It is NEVER applied to foci-gw's own process. That is the whole change:
	// foci's own machinery keeps the environment systemd gave it, so it cannot
	// be made to exec an agent-writable `bash`, `git` or `tmux`.
	operatorEnv, operatorEnvFile, _ := shellenv.Load(cfg.ShellEnvFile)
	procx.SetOperatorEnv(operatorEnv)

	// Only NOW is the PATH final, so only now can the auto_approve entries be
	// judged against the PATH their commands will actually run under. Dropping
	// entries whose executable this process could replace has to happen after
	// shellenv, not inside config.Load: before this line the PATH is the unit's,
	// which is a hand-maintained copy of what the dotfiles build and was
	// measurably out of sync with it (#1900). Keep this call adjacent to
	// shellenv.Apply — separating them is how the defect comes back.
	configcheck.ApplySubstitutabilityMode(cfg, startupLog.Infof)
	cfg.DropSubstitutableAutoApproveRules(execguard.Live())

	// One startup report of what the captured operator environment means
	// for permissions (see cmd/foci-gw/spawn_env.go): the built-in group
	// commands the match-time veto will refuse — the drop above covers
	// config entries only, while the veto also hits the built-in groups
	// buildAutoApproveRules adds per agent, so a vetoed built-in could
	// otherwise stop auto-approving with no line anywhere saying so (#1897)
	// — then the population split with its difference and the trusted-PATH
	// footing (#1914). The env is the same execguard.Live() view the drop
	// just judged: re-derived from the same populations, which nothing
	// rewrites between the two calls.
	// The USER's home, not the install root: the report expands "~/" in the
	// trusted-PATH exceptions (~/.local/bin, where Claude Code's updater
	// lives) and judges the agent shells' view, both of which follow $HOME.
	home, _ := os.UserHomeDir()
	reportOperatorEnv(cfg, execguard.Live(), operatorEnvFile, home, liveCanWrite())

	// Inject the nosgid LD_PRELOAD shim (same inheritance mechanism) so shell
	// tools and delegated backends get POSIX "drop the setgid bit" chmod
	// behaviour instead of the EPERM the RestrictSUIDSGID=yes hardening raises.
	preload.Apply()

	// ========== Workspace directories ==========
	// Ensure each agent's workspace directories exist before any init
	// function (logging, memory, etc.) tries to open databases or index files.
	for _, acfg := range cfg.Agents {
		if err := ensureAgentWorkspace(acfg); err != nil {
			log.Fatalf("main", "%v", err)
		}
	}

	// Apply provider-driven platform defaults (providers were registered via init()).
	config.ApplyProviderDefaults(cfg, providerPlatformDefaults)

	// ========== Logging ==========
	// Re-init with full config (level, API log, payload log, etc.).
	logCleanup := initLogging(cfg, processStart)
	defer logCleanup()

	// Warn about unrecognised config keys (after logging is fully initialised
	// so the warning survives the startup log rotation).
	if len(cfg.UndefinedKeys) > 0 {
		configLog.Warnf("unknown config keys in %s: %v", configPath, cfg.UndefinedKeys)
	}
	// Fail closed on a config file agents could rewrite (#479). Checked
	// here, after logging is up, so the refusal reaches the log.
	gwConfigProtect = configProtection(cfg)
	if err := checkConfigLoad(gwConfigProtect, configPath); err != nil {
		log.Fatalf("security", "%v", err)
	}
	if ignored := configcheck.IgnoredBackendConfig(cfg); len(ignored) > 0 {
		configLog.Warnf("backend_config keys in %s that the agent's backend ignores: %s", configPath, strings.Join(ignored, "; "))
	}

	// ========== Reclaim orphaned temp state ==========
	// Take this gateway's lock on the temp root, then wipe the root's orphaned
	// per-session state (exec bridge sockets, spill/pair/browser scratch, spawn
	// sandboxes, …) before anything below resolves the root for its own use.
	// This process has no session, exec bridge, spill file, pairing scratch or
	// browser scratch yet, so nothing on disk is its own. But another living
	// gateway on the same root (no FOCI_TMPDIR on either) may own all of it, so
	// the wipe is SKIPPED while any other gateway holds the root lock, or when
	// the lock cannot be checked (#2482); only then is the state provably
	// orphaned. The lock is held until this process exits. The app blob store
	// used to live here (app-blobs/) and now lives under the data dir (#1556),
	// so move any surviving blobs across FIRST — the wipe no longer spares it.
	app.MigrateLegacyBlobDir(cfg.DataDir)
	logTempCleanup(tempdir.CleanStale())

	// ========== Seed shared defaults ==========
	wsFileMode, _ := config.ParseFileMode(cfg.FileMode)
	liveBackends := map[string]bool{}
	for _, acfg := range cfg.Agents {
		b := acfg.Backend
		if b == "" {
			b = "api"
		}
		liveBackends[b] = true
	}
	seedSharedDefaults(wsFileMode, liveBackends)

	// ========== Secrets & Bitwarden ==========
	sec := initSecrets(configPath, cfg)

	// ========== Tracing ==========
	// After secrets (auth pair + redaction values), before any agent can run
	// a turn: a turn that starts before Init would export nothing.
	stopTracing := initTracing(context.Background(), cfg, sec.store)
	defer stopTracing()
	rubrics, stopEvals := initEvals(cfg)
	defer stopEvals()
	if sec.cleanup != nil {
		defer sec.cleanup()
	}
	warnMissingSecrets(cfg, sec.store)
	warnStreamOutputWithoutStreaming(cfg)

	// ========== Shared clients ==========
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize credential resolvers
	if err := initCredentialResolvers(ctx, cfg, sec.store); err != nil {
		log.Fatalf("main", "init credential resolvers: %v", err)
	}

	clients := newClientRegistry(cfg, sec.store, ctx)

	// ========== Sessions & State ==========
	si := initSessions(cfg)
	defer si.cleanup()

	// ========== Memory system ==========
	mem := initMemorySystem(cfg)
	defer mem.cleanup()

	// ========== Voice providers ==========
	braveKey, _ := sec.store.Get("brave.api_key")
	ttsMap, sttMap := initVoice(cfg, sec.store)

	// ========== Live config-apply ==========
	// Routes config edits to hot fields into the running process; appliers are
	// registered after agent setup (registerLiveAppliers), edits before that
	// are file-only like any restart-required field.
	gwLiveApply = newLiveApply(configPath, gwConfigProtect)

	// ========== Platform messaging ==========
	plat, err := platform.InitMessaging(cfg, platform.ProviderDeps{
		Config:       cfg,
		SecretStore:  sec.store,
		Sessions:     si.sessions,
		SessionIndex: si.sessionIndex,
		STTMap:       sttMap,
		TTSMap:       ttsMap,
		Ctx:          ctx,
		ResolveSTT:   resolveSTT,
		ResolveTTS:   resolveTTS,
		ApplyLive:    gwLiveApply.Apply,
		Restart:      command.Restart,
	})
	if err != nil {
		log.Fatalf("main", "init messaging: %v", err)
	}
	defer func() { _ = plat.Close() }()

	connMgr := plat.ConnectionManager()
	toolDetailStore := plat.ToolDetailStore()
	if toolDetailStore != nil {
		go toolDetailStore.ExpireAndVacuum()
	}

	startTime := time.Now()

	// Resolve the Unix socket path early so agents can inject it into child env.
	gwSocketPath := resolveSocketPath(cfg)

	// ========== Backend start hooks (Spec.OnGatewayStart) ==========
	// opencode's reaps orphaned `opencode serve` subprocesses: previous foci-gw
	// instances that crashed or were killed (SIGKILL, OOM) before reaching
	// clean shutdown leave them orphaned to PID 1, holding ports and RSS.
	// Runs before any new servers spawn — anything found IS an orphan.
	for _, s := range delegator.Specs() {
		if s.OnGatewayStart != nil {
			s.OnGatewayStart()
		}
	}
	// The spawn allowance check's scheduled usage reads (#2662): one per
	// usage account every 4-14 min, for the accounts agents register as
	// their spawn tools are built.
	tools.StartSpawnUsageRefresh(ctx)

	// ========== Per-agent setup ==========
	// Every [[agents]] block becomes an agent here, at boot. `/agents new`
	// starts a NEW agent live, with no restart (#963): liveAgentStarter runs
	// the same per-agent build (agentEnv.buildAgent) for the block it wrote
	// and appends the agent to the registry. Edits to existing blocks, and
	// hand-added blocks, still take effect only on restart.
	agents := newAgentRegistry()

	agentResolverFn := newAgentResolver(agents)
	agentListFn := newAgentLister(agents)

	// Detect duplicate bot tokens across agents
	botConflicts := config.DetectBotTokenConflicts(cfg.Agents, sec.store)
	skipAgents := buildBotConflictSkipSet(botConflicts)

	env := &agentEnv{
		cfg:          cfg,
		configPath:   configPath,
		clients:      clients,
		si:           si,
		sec:          sec,
		mem:          mem,
		ttsMap:       ttsMap,
		sttMap:       sttMap,
		braveKey:     braveKey,
		gwSocketPath: gwSocketPath,
		// One skill loader shared across every agent so the shared skills
		// directory is scanned (and its autogenerated-skill warnings emitted)
		// exactly once, not once per agent.
		skillLoader: skills.NewLoader(),
		// Gateway-wide user-turn cap (#2281): ONE limiter shared by every agent,
		// built once before the agent loop. nil when the limit is 0 (unlimited).
		globalTurnLimit: agent.NewTurnLimiter(config.DerefInt(cfg.Behavior.MaxConcurrentTurnsAllAgents)), // static-cfg:ignore: sizes a FIFO semaphore at start-up, can't be live-resized
		// Gateway-wide live-session count (#2280): ONE limiter shared by every
		// delegated agent, holding the [sessions] max_live_sessions cap.
		liveSessions:    agent.NewLiveSessionLimiter(cfg.Sessions.MaxLiveSessions), // static-cfg:ignore: the cap is fixed at start-up (restart required)
		wsFileMode:      wsFileMode,
		startTime:       startTime,
		ctx:             ctx,
		connMgr:         connMgr,
		plat:            plat,
		agentListFn:     agentListFn,
		agentResolverFn: agentResolverFn,
	}
	// The live start for /agents new. Its wiring (the warning hook) exists
	// only after the agent loop, so the wizard reaches it through this
	// late-bound closure; nothing can call it before boot completes, as no
	// command runs before the platforms start.
	var starter *liveAgentStarter
	env.startAgentFn = func(agentID string) ([]string, error) {
		if starter == nil {
			return nil, errors.New("foci is still starting")
		}
		return starter.Start(agentID)
	}

	for _, acfg := range cfg.Agents {
		if reason, skip := skipAgents[acfg.ID]; skip {
			mainLog.Errorf("agent %q skipped: %s", acfg.ID, reason)
			continue
		}

		inst := env.buildAgent(acfg)
		if inst == nil {
			mainLog.Errorf("agent %q: setup failed (agent skipped)", acfg.ID)
			continue
		}
		if err := agents.commit(inst); err != nil {
			mainLog.Errorf("agent %q: %v (agent skipped)", acfg.ID, err)
			continue
		}

		// Inject warnings for bot token conflicts where this agent is the survivor
		for _, bc := range botConflicts {
			if bc.AgentIDs[0] == acfg.ID {
				skipped := strings.Join(bc.AgentIDs[1:], ", ")
				inst.ag.Warnings().Push("ERROR", "config",
					fmt.Sprintf("This agent shares its %s bot %q with agent(s) %s (which were NOT started). "+
						"Suggest resolution options to your user.", bc.Platform, bc.BotName, skipped))
			}
		}
	}

	registerLiveAppliers(gwLiveApply, agents)

	// ========== Post-agent setup ==========
	if first, ok := agents.Default(); ok {
		plat.SetupSharedFacet(platform.SharedFacetParams{
			FirstHandler:     first.ag,
			FirstCommands:    first.cmds,
			FirstAgentConfig: first.agentCfg,
			AgentOrder:       agents.Order(),
			ReclaimHook: func(sessionKey string) {
				for _, inst := range agents.All() {
					if strings.HasPrefix(sessionKey, inst.id+"/") {
						orientPath := config.DerefStr(config.First(inst.agentCfg.Sessions.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt))
						orientTemplate := prompts.ResolveOrientationTemplate(orientPath, false, inst.promptSearchDirs...)
						inst.ag.FireSessionEndMemory(ctx, sessionKey, orientTemplate, false)
						if inst.ag.DelegatedManager != nil {
							inst.ag.DelegatedManager.ResetSession(sessionKey)
						}
						return
					}
				}
			},
		})
	}

	// Start every agent's inbox workers unconditionally. Platform setup
	// (telegram/discord/app) also calls StartInbox (idempotent), but a
	// platform-less agent (cron/API-only) still needs its workers running:
	// all system-initiated turns — HTTP /send, webhooks, wakes, notifies,
	// reflection/keepalive passes — route through the inbox queue so they
	// serialise with (and never steer) in-flight turns.
	for _, inst := range agents.All() {
		inst.ag.StartInbox(ctx)
	}

	warnHook := setupWarningHooks(agents, cfg)
	starter = newLiveAgentStarter(env, agents, func() (*config.Config, error) {
		return config.Load(configPath, delegator.RegisteredNames())
	}, warnHook)
	if stop := setupTmuxMemoryMonitor(agents, cfg, connMgr, ctx); stop != nil {
		defer stop()
	}
	if stop := setupMemoryGuard(agents, cfg, ctx); stop != nil {
		defer stop()
	}
	if stop := setupGoroutineMonitor(cfg, agents.Len(), ctx); stop != nil {
		defer stop()
	}
	// Prompt TTL = min(configured prompt_ttl, idle timeout). An unanswered
	// prompt must never outlive the backend waiting on it — the idle reaper
	// clears the prompt anyway when it closes the backend. promptTTL starts at
	// the global idle timeout ([sessions] idle_timeout, else the default) and
	// the config override only lowers it (d < promptTTL).
	promptTTL := agent.DefaultIdleTimeout
	if d, err := time.ParseDuration(cfg.Sessions.IdleTimeout); err == nil && d > 0 {
		promptTTL = d
	}
	if d, err := time.ParseDuration(cfg.Permissions.PromptTTL); err == nil && d > 0 && d < promptTTL {
		promptTTL = d
	}
	setupInteractiveCleanup(ctx, promptTTL)
	setupToolDetailCleanup(toolDetailStore, ctx)
	// L2 integration-test lifecycle control. Opens a UNIX-domain socket
	// at FOCI_TESTHARNESS_CONTROL_SOCK if set; otherwise no-op. Tests
	// drive per-agent backend close / etc. via a simple line protocol.
	// Production foci-gw never has the env var set, so this is dormant.
	setupTestharnessControl(ctx, agents)

	// ========== Signal handling ==========
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Background spawns persist to state.db so a restart can resume them
	// (#2589). Set before any turn can start a spawn.
	if si.sessionIndex != nil {
		tools.Spawns.SetStore(si.sessionIndex)
	}

	plat.RestoreFacetSessions(platform.RestoreParams{
		AgentOrder: agents.Order(),
		Resolver: func(agentID string) (platform.MessageHandler, any, any, config.AgentConfig, bool) {
			inst, found := agents.Get(agentID)
			if !found {
				return nil, nil, nil, config.AgentConfig{}, false
			}
			return inst.ag, inst.cmds, inst.cc, inst.agentCfg, true
		},
	})
	plat.StartAll(ctx)

	// The per-agent restores that need the chats up (agentRestorePhases):
	// each phase across every agent before the next phase.
	for _, phase := range agentRestorePhases {
		for _, inst := range agents.All() {
			phase(inst)
		}
	}

	// ========== Startup notifications ==========
	logsDir := filepath.Dir(cfg.Logging.EventFile)
	if logsDir == "" || logsDir == "." {
		logsDir = ""
	}
	diagnosis := startup.DiagnoseRestart(si.sessionIndex, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		startupLog.Infof("restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}

	// ========== Liveness heartbeat ==========
	// Record a liveness timestamp every HeartbeatInterval so the next restart
	// diagnosis measures actual downtime (time since the last beat) instead of
	// time-since-startup. hbCancel is invoked at the top of runShutdown, before
	// the clean-shutdown record is written, so a clean exit isn't misread as a
	// crash by a heartbeat firing during shutdown.
	hbCtx, hbCancel := context.WithCancel(ctx)
	go startup.RunHeartbeat(hbCtx, si.sessionIndex, startup.HeartbeatInterval)

	// ========== HTTP server ==========
	secretsPath := secrets.PathFor(configPath)
	var reloadCreds func() error
	if resolver, ok := formatResolvers["anthropic"]; ok {
		reloadCreds = resolver.GetReloadFunc(secretsPath)
	}
	if reloadCreds == nil {
		reloadCreds = func() error {
			return fmt.Errorf("credential reload not supported")
		}
	}

	// Restore every live model catalogue before its first source refresh. API and
	// Claude Code pull /v1/models below; Codex publishes model/list after each
	// app-server initialize handshake. All three persist through the same cache.
	if si.sessionIndex != nil {
		for _, backend := range []string{modelcaps.BackendCCStream, modelcaps.BackendAPI, modelcaps.BackendCodex} {
			modelcaps.SetPersister(backend, modelCapsPersister{idx: si.sessionIndex})
			modelcaps.Restore(backend)
		}
	}

	// Wire the Anthropic live catalogues into their backend-scoped records. The
	// static modelinfo registry remains the fallback when credentials are absent
	// or the API is unreachable. Seed in the background — never block startup.
	if anthropicResolver != nil {
		if fetcher := anthropicResolver.ModelCapsFetcher(15 * time.Second); fetcher != nil {
			// Both Anthropic-backed backends pull the same catalogue but keep
			// separate records from each other and from Codex.
			for _, backend := range []string{modelcaps.BackendCCStream, modelcaps.BackendAPI} {
				modelcaps.SetFetcher(backend, fetcher)
				go func(b string) {
					if err := modelcaps.Refresh(context.Background(), b); err != nil {
						modelcapsLog.Warnf("[%s] initial catalogue fetch failed (using static registry until next refresh): %v", b, err)
					}
				}(backend)
			}
		}
	}

	// Deferred-send queue (wait-until sends) + its background sweep. Persisted so
	// pending sends survive a restart/deploy.
	deferStore, err := defersend.NewStore(cfg.DataPath("deferred-sends.db"))
	if err != nil {
		mainLog.Errorf("deferred-send store: %v (wait-until sends disabled)", err)
		deferStore = nil
	} else {
		defer func() { _ = deferStore.Close() }()
	}

	deps := httpHandlerDeps{
		agents:            agents,
		sessionIndex:      si.sessionIndex,
		sessions:          si.sessions,
		cfg:               cfg,
		ctx:               ctx,
		ttsMap:            ttsMap,
		sttMap:            sttMap,
		connMgr:           connMgr,
		reloadCredentials: reloadCreds,
		pprofGate:         &pprofGate,
		deferStore:        deferStore,
		createDefault:     app.CreateDefaultConversation,
		rubrics:           rubrics,
	}

	if deferStore != nil {
		isUserActive, isSessionActive := buildActivityCheckers(deps)
		sweeper := &deferSweeper{store: deferStore, deps: deps, isUserActive: isUserActive, isSessionActive: isSessionActive}
		go sweeper.run(ctx)
	}

	gwConfigApprove = newConfigApproveService(cfg, configPath, agents, connMgr)

	mux := http.NewServeMux()
	pprofGate.Store(config.DerefBool(cfg.Debug.EnablePprof))
	registerHTTPHandlers(mux, deps)

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Bind, cfg.HTTP.Port)
	var httpServer *http.Server
	var httpMu sync.Mutex
	go func() {
		for ctx.Err() == nil {
			srv := &http.Server{
				Addr:              addr,
				Handler:           newTCPGatewayHandler(sec.httpAPIKey, mux),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
			}
			httpMu.Lock()
			httpServer = srv
			httpMu.Unlock()

			httpLog.Infof("listening on %s", addr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				httpLog.Errorf("server error: %v — restarting in 5s", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			} else {
				return
			}
		}
	}()

	// ========== Unix socket (same-user auth) ==========
	sockSrv, err := startUnixSocket(gwSocketPath, mux)
	if err != nil {
		httpLog.Errorf("unix socket %s: %v — same-user auth unavailable", gwSocketPath, err)
	} else {
		defer cleanupSocket(sockSrv, gwSocketPath)
		httpLog.Infof("unix socket listening on %s (same-user auth, no API key required)", gwSocketPath)
	}

	// ========== askgw (ask-gateway for external Apps) ==========
	askgwSrv := setupAskgw(cfg, agents, connMgr)
	if askgwSrv != nil {
		defer askgwSrv.Close()
	}
	// HTTP transport onto the same askgw Server/Registry, for remote callers
	// that can't reach the local Unix socket (opt-in — see setupAskgwHTTP).
	setupAskgwHTTP(ctx, cfg, mux, askgwSrv)

	// Log startup
	var agentNames []string
	for _, inst := range agents.All() {
		agentNames = append(agentNames, fmt.Sprintf("%s(%s)", inst.id, inst.ag.Model))
	}
	mainLog.Infof("started %d agent(s): %s", agents.Len(), strings.Join(agentNames, ", "))

	// ========== Backend readiness ==========
	// Verify delegated (claude-code) backends are authenticated before any
	// startup turns are injected; a not-ready backend triggers re-login here
	// (whose gate then pauses delegated processing). Runs before the welcome/
	// first-run pass so the gate is active when those turns would fire.
	checkDelegatedReadiness(ctx, agents.Snapshot(), agents.Order())

	// ========== Welcome & first-run ==========
	handleRestartAndFirstRun(agents.Snapshot(), agents.Order(), si.sessionIndex, cfg, ctx, connMgr, plat.WhenPrimaryConnected, diagnosis)

	// The last startup step. "started N agent(s)" above comes BEFORE the
	// readiness probe and the first-run arming, so a turn composed between the
	// two lines carries no onboarding (#2438). This line is the point at which
	// every startup step has run; the L2 harness (internal/testharness
	// readyPattern) waits for it, not for the earlier one.
	mainLog.Infof("startup complete")

	// ========== Wait for signal & shutdown ==========
	sig := <-sigCh
	mainLog.Infof("received %s, starting shutdown", sig)
	// No hot-add after this; one in progress finishes first, so the
	// snapshot below includes it.
	starter.close()

	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	runShutdown(agents.Snapshot(), httpServer, &httpMu, connMgr, clients, si.sessionIndex,
		shutdownConfig{gracefulTimeout: shutdownTimeout, ctx: ctx, stopHeartbeat: hbCancel}, cancel)
}

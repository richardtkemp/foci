package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"clod/agent"
	"clod/anthropic"
	"clod/command"
	"clod/compaction"
	"clod/config"
	"clod/log"
	"clod/memory"
	"clod/secrets"
	"clod/secrets/bitwarden"
	"clod/session"
	"clod/skills"
	"clod/state"
	"clod/telegram"
	"clod/tools"
	"clod/voice"
	"clod/workspace"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

// agentInstance holds all per-agent state.
type agentInstance struct {
	id                string
	ag                *agent.Agent
	cmds              *command.Registry
	registry          *tools.Registry
	bootstrap         *workspace.Bootstrap
	defaultSessionKey func() string // resolves current default session key
	heartbeat         *agent.Heartbeat
	agentCfg          config.AgentConfig
	tmuxClearAll      func() // clears tmux tool state (watches, owned sessions)
}

func main() {
	configPath := config.ParseFlags()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("main", "load config: %v", err)
	}

	// Initialize logging
	if err := log.Init(log.Config{
		Level:       cfg.Logging.Level,
		EventFile:   cfg.Logging.EventFile,
		APIFile:     cfg.Logging.APIFile,
		PayloadFile: cfg.Logging.PayloadFile,
	}); err != nil {
		log.Fatalf("main", "init logging: %v", err)
	}
	defer log.Close()

	// Conversation log (SQLite)
	if cfg.Logging.ConversationFile != "" {
		if err := log.InitConversation(cfg.Logging.ConversationFile); err != nil {
			log.Fatalf("main", "init conversation log: %v", err)
		}
		defer log.CloseConversation()
	}

	// Load secrets (from secrets.toml alongside config file)
	secretsPath := filepath.Join(filepath.Dir(configPath), "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		log.Fatalf("main", "load secrets: %v", err)
	}
	if names := store.Names(); len(names) > 0 {
		log.Infof("main", "loaded %d secrets: %v", len(names), names)
	}

	// Startup security checks for secrets.toml
	if !cfg.SkipSecurityChecks {
		if warnings := store.CheckSecurity(); len(warnings) > 0 {
			for _, w := range warnings {
				log.Warnf("security", "%s", w)
			}
		}
	}

	// Wire child process group-dropping into the command package
	// (so script commands also drop supplementary groups).
	command.ChildSysProcAttr = tools.ChildSysProcAttr

	// Resolve shared credentials: secrets.toml overrides clod.toml
	anthropicToken := cfg.Anthropic.Token
	if v, ok := store.Get("anthropic.token"); ok {
		anthropicToken = v
	}
	anthropicOAuthToken := cfg.Anthropic.OAuthToken
	if v, ok := store.Get("anthropic.oauth_token"); ok {
		anthropicOAuthToken = v
	}
	braveKey := cfg.Anthropic.BraveAPIKey
	if v, ok := store.Get("brave.api_key"); ok {
		braveKey = v
	}
	groqKey := ""
	if v, ok := store.Get("groq.api_key"); ok {
		groqKey = v
	}
	openrouterKey := ""
	if v, ok := store.Get("openrouter.api_key"); ok {
		openrouterKey = v
	}

	// Shared: Bitwarden store (optional)
	var bwStore *bitwarden.Store
	if cfg.Bitwarden.Enabled {
		secretTTL, _ := time.ParseDuration(cfg.Bitwarden.SecretTTL)
		bwExec := &bitwarden.DefaultExecutor{SessionFile: cfg.Bitwarden.SessionFile}
		bwStore = bitwarden.New(bwExec, secretTTL)

		if err := bwStore.Refresh(); err != nil {
			log.Errorf("main", "bitwarden initial refresh: %v", err)
		} else {
			log.Infof("main", "bitwarden: loaded %d vault items", bwStore.ItemCount())
		}

		// Background refresh ticker
		refreshInterval, _ := time.ParseDuration(cfg.Bitwarden.RefreshInterval)
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := bwStore.Refresh(); err != nil {
					log.Warnf("bitwarden", "background refresh: %v", err)
				}
			}
		}()

		// Background cleanup of expired values
		cleanupInterval, _ := time.ParseDuration(cfg.Bitwarden.CleanupInterval)
		bwStore.StartCleanup(cleanupInterval)
		defer bwStore.Close()
	}

	// Shared: Anthropic client
	httpTimeout, err := time.ParseDuration(cfg.Anthropic.HTTPTimeout)
	if err != nil {
		log.Warnf("main", "invalid anthropic.http_timeout, using default: %v", err)
		httpTimeout = 120 * time.Second
	}
	client := anthropic.NewClientWithTimeout(anthropicToken, httpTimeout)
	log.Debugf("main", "anthropic client timeout=%s", httpTimeout)

	// Shared: Session store
	sessions := session.NewStore(cfg.Sessions.Dir)
	log.Debugf("main", "session store dir=%s", cfg.Sessions.Dir)

	// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts)
	if n, err := sessions.RepairOrphans(); err != nil {
		log.Warnf("main", "session repair: %v", err)
	} else if n > 0 {
		log.Infof("main", "repaired %d orphaned session(s) with interrupted tool calls", n)
	}

	// Inject restart markers into recently active sessions
	if n, err := sessions.InjectRestartMarkers(session.RestartMarkerMaxAge); err != nil {
		log.Warnf("main", "restart markers: %v", err)
	} else if n > 0 {
		log.Infof("main", "injected restart markers into %d active session(s)", n)
	}

	// Shared: State persistence (JSON file in data dir)
	statePath := cfg.DataPath("state.json")
	stateStore := state.New(statePath)
	if err := stateStore.Load(); err != nil {
		log.Errorf("main", "load state: %v", err)
	}

	// ========== Memory system ==========
	// Build global source map from [memory] config
	globalMemSources := make(map[string]memory.SourceConfig)
	if len(cfg.Memory.Sources) > 0 {
		for _, src := range cfg.Memory.Sources {
			globalMemSources[src.Name] = memory.SourceConfig{Dir: src.Dir, Weight: src.Weight}
		}
	} else if cfg.Memory.Dir != "" {
		globalMemSources["memory"] = memory.SourceConfig{Dir: cfg.Memory.Dir, Weight: 1.0}
	}

	// Parse debounce delay
	memDebounce := time.Duration(0)
	if cfg.Memory.ReindexDebounce != "" {
		memDebounce, err = time.ParseDuration(cfg.Memory.ReindexDebounce)
		if err != nil {
			log.Fatalf("main", "invalid reindex_debounce: %v", err)
		}
	}

	// Check if any agent has per-agent memory sources
	hasPerAgentMemory := false
	for _, acfg := range cfg.Agents {
		if len(acfg.Memory.Sources) > 0 {
			hasPerAgentMemory = true
			break
		}
	}

	var sharedMemIdx *memory.Index                    // used when no per-agent memory
	agentMemIndices := make(map[string]*memory.Index) // agentID → per-agent index
	var reminderStore *memory.ReminderStore
	var scratchpadStore *memory.Scratchpad
	var todoStore *memory.TodoStore

	memoryEnabled := len(globalMemSources) > 0 || hasPerAgentMemory
	if memoryEnabled {
		if hasPerAgentMemory {
			// Per-agent indices: each agent gets global + agent-specific sources
			for _, acfg := range cfg.Agents {
				combined := buildAgentMemorySources(globalMemSources, acfg.Memory.Sources)
				if len(combined) == 0 {
					continue
				}
				dbPath := cfg.DataPath(fmt.Sprintf("memory-%s.db", acfg.ID))
				idx, err := memory.NewIndex(dbPath, combined, memDebounce, cfg.Memory.ConversationWeight)
				if err != nil {
					log.Fatalf("main", "create memory index for agent %q: %v", acfg.ID, err)
				}
				defer idx.Close()
				if err := idx.Reindex(); err != nil {
					log.Errorf("main", "reindex memory for agent %q: %v", acfg.ID, err)
				}
				if memDebounce > 0 || len(combined) > 0 {
					if err := idx.Watch(); err != nil {
						log.Errorf("main", "start memory file watching for agent %q: %v", acfg.ID, err)
					}
				}
				agentMemIndices[acfg.ID] = idx
				log.Infof("main", "agent %q: memory index with %d sources", acfg.ID, len(combined))
			}

			// Conversation hook: route to agent's index by session key prefix
			log.ConversationHook = func(text, session string) {
				for agentID, idx := range agentMemIndices {
					if strings.HasPrefix(session, "agent:"+agentID+":") {
						idx.IndexConversation(text, session)
						return
					}
				}
			}
		} else {
			// Shared index (backward compat — no agent has per-agent memory)
			memDbPath := cfg.DataPath("memory.db")
			sharedMemIdx, err = memory.NewIndex(memDbPath, globalMemSources, memDebounce, cfg.Memory.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create memory index: %v", err)
			}
			defer sharedMemIdx.Close()

			if err := sharedMemIdx.Reindex(); err != nil {
				log.Errorf("main", "reindex memory: %v", err)
			}
			if memDebounce > 0 || len(cfg.Memory.Sources) > 0 {
				if err := sharedMemIdx.Watch(); err != nil {
					log.Errorf("main", "start memory file watching: %v", err)
				}
			}
			log.ConversationHook = sharedMemIdx.IndexConversation
		}

		// Reminder store (shared across agents)
		reminderDbPath := cfg.DataPath("reminders.db")
		reminderStore, err = memory.NewReminderStore(reminderDbPath)
		if err != nil {
			log.Fatalf("main", "create reminder store: %v", err)
		}
		defer reminderStore.Close()

		// Scratchpad (shared across agents)
		scratchpadDbPath := cfg.DataPath("scratchpad.db")
		scratchpadStore, err = memory.NewScratchpad(scratchpadDbPath)
		if err != nil {
			log.Fatalf("main", "create scratchpad: %v", err)
		}
		defer scratchpadStore.Close()

		// Todo list (shared across agents, agent_id scoped per-agent)
		todoDbPath := cfg.DataPath("todo.db")
		todoStore, err = memory.NewTodoStore(todoDbPath)
		if err != nil {
			log.Fatalf("main", "create todo store: %v", err)
		}
		defer todoStore.Close()
	}

	// Shared: Voice providers
	var sttProvider voice.STT
	var ttsProvider voice.TTS

	// STT: Whisper API (Groq by default, any OpenAI-compatible endpoint)
	sttEndpoint := cfg.Voice.STTEndpoint
	if sttEndpoint == "" {
		sttEndpoint = "https://api.groq.com/openai/v1/audio/transcriptions"
	}
	sttModel := cfg.Voice.STTModel
	if sttModel == "" {
		sttModel = "whisper-large-v3"
	}
	if groqKey != "" {
		sttProvider = &voice.WhisperSTT{
			Endpoint: sttEndpoint,
			APIKey:   groqKey,
			Model:    sttModel,
		}
		log.Infof("main", "voice STT enabled (whisper, %s)", sttModel)
	}

	// TTS: edge-tts (default, free) or openai-compatible API
	ttsProviderName := cfg.Voice.TTSProvider
	if ttsProviderName == "" {
		ttsProviderName = "edge-tts"
	}
	switch ttsProviderName {
	case "edge-tts":
		ttsProvider = &voice.EdgeTTS{
			Voice: cfg.Voice.TTSVoice,
			Rate:  cfg.Voice.TTSRate,
		}
		log.Infof("main", "voice TTS enabled (edge-tts, voice=%s rate=%.2f)", cfg.Voice.TTSVoice, cfg.Voice.TTSRate)
	case "openai":
		ttsEndpoint := cfg.Voice.TTSEndpoint
		if ttsEndpoint == "" {
			ttsEndpoint = "https://openrouter.ai/api/v1/audio/speech"
		}
		ttsModel := cfg.Voice.TTSModel
		if ttsModel == "" {
			ttsModel = "openai/tts-1-mini"
		}
		ttsVoice := cfg.Voice.TTSVoice
		if ttsVoice == "" {
			ttsVoice = "alloy"
		}
		ttsProvider = &voice.OpenAITTS{
			Endpoint: ttsEndpoint,
			APIKey:   openrouterKey,
			Model:    ttsModel,
			Voice:    ttsVoice,
			Speed:    cfg.Voice.TTSRate,
		}
		log.Infof("main", "voice TTS enabled (openai, %s, voice=%s rate=%.2f)", ttsModel, ttsVoice, cfg.Voice.TTSRate)
	default:
		log.Warnf("main", "unknown tts_provider %q, TTS disabled", ttsProviderName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTime := time.Now()

	// Bot manager — owns all Telegram bots
	botMgr := telegram.NewBotManager()

	// Shared: usage client — prefer credentials file (auto-refreshing), fall back to static token
	credFile := cfg.Anthropic.CredentialsFile
	if strings.HasPrefix(credFile, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			credFile = filepath.Join(home, credFile[2:])
		}
	}
	var usageClient *anthropic.UsageClient
	if credFile != "" {
		usageClient = anthropic.NewUsageClientWithFunc(func() string {
			token, err := anthropic.ReadCredentialsToken(credFile)
			if err != nil {
				log.Debugf("main", "read credentials file: %v", err)
				return anthropicOAuthToken // fallback to static token
			}
			return token
		})
		log.Infof("main", "usage: reading OAuth token from %s", credFile)
	} else {
		usageClient = anthropic.NewUsageClient(anthropicOAuthToken)
	}

	// Mana detection startup checks
	for _, w := range checkManaPrereqs(credFile) {
		log.Warnf("main", "%s", w)
	}

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string // preserve config order

	// Closure for /agents command — captures agents/agentOrder, resolved at call time.
	agentListFn := func() []command.AgentInfo {
		var infos []command.AgentInfo
		for _, id := range agentOrder {
			inst := agents[id]
			sk := inst.defaultSessionKey()
			mc, _ := inst.ag.Sessions.MessageCount(sk)
			infos = append(infos, command.AgentInfo{
				ID:           id,
				SessionKey:   sk,
				Model:        inst.ag.Model,
				Busy:         inst.ag.IsProcessing(),
				MessageCount: mc,
				LastActivity: inst.ag.Sessions.LastActivity(sk),
			})
		}
		return infos
	}

	for _, acfg := range cfg.Agents {
		// Resolve memory index: per-agent (if configured) or shared
		agentMemIdx := sharedMemIdx
		if idx, ok := agentMemIndices[acfg.ID]; ok {
			agentMemIdx = idx
		}

		inst := setupAgent(setupParams{
			acfg:                acfg,
			cfg:                 cfg,
			configPath:          configPath,
			client:              client,
			sessions:            sessions,
			store:               store,
			bwStore:             bwStore,
			stateStore:          stateStore,
			memIdx:              agentMemIdx,
			reminderStore:       reminderStore,
			scratchpadStore:     scratchpadStore,
			todoStore:           todoStore,
			sttProvider:         sttProvider,
			ttsProvider:         ttsProvider,
			braveKey:            braveKey,
			anthropicOAuthToken: anthropicOAuthToken,
			usageClient:         usageClient,
			botMgr:              botMgr,
			startTime:           startTime,
			ctx:                 ctx,
			agentListFn:         agentListFn,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)
		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// Shared multiball pool — fallback bots available to any agent.
	// Created after all agents so we can use the first agent's instance for initial binding.
	// Bots are re-wired to the acquiring agent at fork time via SetAgentAndCommands.
	if len(cfg.Telegram.MultiballBots) > 0 && len(agentOrder) > 0 {
		firstInst := agents[agentOrder[0]]
		for _, botName := range cfg.Telegram.MultiballBots {
			mbToken := cfg.ResolveBotToken(botName, store)
			if mbToken == "" {
				log.Errorf("main", "shared multiball bot %q: token not found", botName)
				continue
			}
			mbBot, err := telegram.NewBot(mbToken, cfg.Telegram.AllowedUsers,
				firstInst.ag, firstInst.cmds, command.NewLastMessageStore(), "")
			if err != nil {
				log.Errorf("main", "shared multiball bot %q: create: %v", botName, err)
				continue
			}
			if sttProvider != nil {
				mbBot.SetTranscriber(sttProvider)
			}
			if ttsProvider != nil {
				mbBot.SetTTS(ttsProvider)
			}
			mbBot.SetStopAliases(cfg.Telegram.StopAliases, cfg.Telegram.EnableStopAliases)
			if stateStore != nil {
				ss := stateStore
				mbBot.OnSessionKeyChange = func(username, sessionKey string) {
					key := "multiball:" + username
					if sessionKey == "" {
						ss.Delete(key)
					} else {
						ss.Set(key, sessionKey)
					}
				}
			}
			botMgr.AddSharedMultiball(mbBot)
		}
		if pool := botMgr.SharedPool(); pool != nil && pool.Size() > 0 {
			ttl, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
			if ttl > 0 {
				pool.SetSessionTTL(ttl, sessions)
			}
			pool.ReclaimHook = func(sessionKey string) {
				// Determine agent from session key for the reset hook
				for _, id := range agentOrder {
					inst := agents[id]
					prefix := "agent:" + id + ":"
					if strings.HasPrefix(sessionKey, prefix) {
						fireResetHook(inst.ag, sessions, sessionKey, cfg, ctx)
						return
					}
				}
			}
			log.Infof("main", "%d shared multiball bots ready", pool.Size())
		}
	}

	// Wire log warnings into agent sessions (any agent with inject_agent_warnings)
	{
		anyInjection := false
		for _, acfg := range cfg.Agents {
			if acfg.InjectAgentWarnings {
				anyInjection = true
				break
			}
		}
		if anyInjection {
			log.WarnHook = func(level log.Level, component string, msg string) {
				for _, inst := range agents {
					if inst.ag.Warnings != nil {
						inst.ag.Warnings.Push(level.String(), component, msg)
					}
				}
			}
			log.Infof("main", "warning injection into agent sessions enabled")
		}
	}

	// Tmux memory monitor — checks RSS of tmux server, notifies/kills at thresholds
	if cfg.Tools.TmuxMemoryCheckInterval != "0" {
		checkInterval, _ := time.ParseDuration(cfg.Tools.TmuxMemoryCheckInterval)
		if checkInterval > 0 {
			tmuxMemMon := tools.NewTmuxMemoryMonitor(
				tools.TmuxMemoryConfig{
					CheckInterval: checkInterval,
					WarnStr:       cfg.Tools.TmuxMemoryWarn,
					CriticalStr:   cfg.Tools.TmuxMemoryCritical,
					KillStr:       cfg.Tools.TmuxMemoryKill,
				},
				// Notification callback: send to agents without inject_agent_warnings
				func(msg string) {
					for _, id := range agentOrder {
						inst := agents[id]
						if inst.agentCfg.InjectAgentWarnings {
							continue // agent sees warnings via injection
						}
						if bot := botMgr.PrimaryBot(id); bot != nil {
							bot.SendNotification(msg)
						}
					}
				},
				// Cleanup callback: clear all tmux tool instances
				func() {
					for _, id := range agentOrder {
						if fn := agents[id].tmuxClearAll; fn != nil {
							fn()
						}
					}
				},
			)
			tmuxMemMon.Start(ctx)
			defer tmuxMemMon.Stop()
		}
	}

	// Intercept SIGINT/SIGTERM before starting bots or heartbeats.
	// Must be registered before any goroutine that could trigger a signal
	// (e.g. /restart via Telegram), otherwise Go's default handler
	// terminates the process with no graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Restore multiball sessions from persisted state.
	// For each secondary bot, check if a saved session key exists and the session
	// file is still active. If so, restore the session key and re-wire the agent.
	if stateStore != nil {
		restoreMultiballSessions(botMgr, stateStore, sessions, agents, agentOrder)
	}

	// Start all bots
	botMgr.StartAll(ctx)

	// Send startup notifications to users via Telegram
	// Per-agent startup_notification overrides global enable_startup_notify
	for _, id := range agentOrder {
		inst := agents[id]
		enabled := cfg.Telegram.EnableStartupNotify // global default
		if inst.agentCfg.StartupNotification != nil {
			enabled = *inst.agentCfg.StartupNotification // per-agent override
		}
		if enabled {
			if bot := botMgr.PrimaryBot(id); bot != nil {
				bot.SendStartupNotification(id)
			}
		}
	}

	// Start all heartbeats
	for _, id := range agentOrder {
		agents[id].heartbeat.Start(ctx)
	}

	// ========== HTTP server ==========
	mux := http.NewServeMux()

	// resolveAgent returns the agent instance for the given ID, or the first agent if empty.
	resolveAgent := func(agentID string) (*agentInstance, bool) {
		if agentID == "" && len(agentOrder) > 0 {
			return agents[agentOrder[0]], true
		}
		inst, ok := agents[agentID]
		return inst, ok
	}

	// POST /send — send message to agent session, return response
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent   string `json:"agent"`
			Session string `json:"session"`
			Text    string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, "bad request: need {\"text\": \"...\"}", http.StatusBadRequest)
			return
		}

		inst, ok := resolveAgent(req.Agent)
		if !ok {
			log.Warnf("http", "POST /send: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}

		sessionKey := inst.defaultSessionKey()
		if req.Session != "" {
			sessionKey = fmt.Sprintf("agent:%s:%s", inst.id, req.Session)
		}
		if sessionKey == "" {
			log.Warnf("http", "POST /send: no default session for agent %q", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		log.Infof("http", "send (agent=%s, session=%s): %s", inst.id, sessionKey, req.Text)

		// Route slash commands through the command dispatcher
		if strings.HasPrefix(req.Text, "/") {
			if result, ok := inst.cmds.Dispatch(ctx, req.Text); ok {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{"response": result}); err != nil {
					log.Errorf("http", "encode response: %v", err)
				}
				return
			}
		}

		resp, err := inst.ag.HandleMessage(agent.WithTrigger(ctx, "user"), sessionKey, req.Text)
		if err != nil {
			log.Errorf("http", "send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		inst.heartbeat.Reset()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": resp}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	})

	// GET /status — return agent status
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agentID := r.URL.Query().Get("agent")
		inst, ok := resolveAgent(agentID)
		if !ok {
			log.Warnf("http", "GET /status: unknown agent %q", agentID)
			http.Error(w, fmt.Sprintf("unknown agent: %q", agentID), http.StatusBadRequest)
			return
		}
		result, ok := inst.cmds.Dispatch(context.Background(), "/status")
		if !ok {
			http.Error(w, "status command not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": result}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	})

	// POST /command — dispatch any slash command
	mux.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent   string `json:"agent"`
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
			http.Error(w, "bad request: need {\"command\": \"/ping\"}", http.StatusBadRequest)
			return
		}
		inst, ok := resolveAgent(req.Agent)
		if !ok {
			log.Warnf("http", "POST /command: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		result, ok := inst.cmds.Dispatch(context.Background(), req.Command)
		if !ok {
			http.Error(w, "unknown command", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": result}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	})

	// POST /wake — branch session for cron/external triggers
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Agent       string `json:"agent"`
			Text        string `json:"text"`
			NoCompact   bool   `json:"no_compact"`
			NoResetHook bool   `json:"no_reset_hook"`
		}
		// Allow empty body — treat as wake with default text
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request: need {\"text\": \"...\"}", http.StatusBadRequest)
				return
			}
		}

		inst, ok := resolveAgent(req.Agent)
		if !ok {
			log.Warnf("http", "POST /wake: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			req.Text = "[WAKE]"
		}

		// Create a branch session for this wake call
		parentKey := inst.defaultSessionKey()
		if parentKey == "" {
			log.Warnf("wake", "no default session for agent %q, skipping", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}
		branchID := fmt.Sprintf("wake-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", inst.id, branchID)

		var branchErr error
		if req.NoResetHook {
			branchErr = sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
				NoResetHook: true,
			})
		} else {
			branchErr = sessions.CreateBranch(parentKey, branchKey)
		}
		if branchErr != nil {
			log.Errorf("wake", "branch error: %v", branchErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Infof("wake", "branch %s from %s, text=%q no_compact=%v no_reset_hook=%v", branchKey, parentKey, req.Text, req.NoCompact, req.NoResetHook)

		wakeCtx := agent.WithTrigger(ctx, "wake")
		if req.NoCompact {
			wakeCtx = agent.WithNoCompact(wakeCtx)
		}
		resp, err := inst.ag.HandleMessage(wakeCtx, branchKey, req.Text)
		if err != nil {
			log.Errorf("wake", "error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		inst.heartbeat.Reset()

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": resp}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	})

	log.Infof("http", "registered endpoints: /send, /status, /command, /wake")

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Bind, cfg.HTTP.Port)
	var httpServer *http.Server
	var httpMu sync.Mutex
	go func() {
		for ctx.Err() == nil {
			srv := &http.Server{Addr: addr, Handler: mux}
			httpMu.Lock()
			httpServer = srv
			httpMu.Unlock()

			log.Infof("http", "listening on %s", addr)
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				log.Errorf("http", "server error: %v — restarting in 5s", err)
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

	// Log startup
	var agentNames []string
	for _, id := range agentOrder {
		agentNames = append(agentNames, fmt.Sprintf("%s(%s)", id, agents[id].agentCfg.Model))
	}
	log.Infof("main", "started %d agent(s): %s", len(agents), strings.Join(agentNames, ", "))

	// Check for welcome file (written by setup.sh on update)
	// Returns the changelog content if injected, empty string otherwise.
	if content := injectWelcomeFile(cfg.WelcomeFile, agents, agentOrder, sessions); content != "" {
		// Fire an agent turn so the changelog is processed immediately,
		// rather than waiting for the next user message.
		inst := agents[agentOrder[0]]
		go func() {
			sk := inst.defaultSessionKey()
			if sk == "" {
				log.Warnf("main", "no default session for welcome file injection, skipping")
				return
			}
			restartCtx := agent.WithTrigger(ctx, "restart")
			restartCtx = agent.WithNoCompact(restartCtx)
			msg := fmt.Sprintf("[SYSTEM UPDATE]\n%s", content)
			if _, err := inst.ag.HandleMessage(restartCtx, sk, msg); err != nil {
				log.Errorf("main", "restart turn failed: %v", err)
			}
		}()
	}

	// Wait for signal
	<-sigCh

	log.Infof("main", "shutting down...")

	// Stop all heartbeats first — prevents new heartbeat-triggered turns
	for _, id := range agentOrder {
		agents[id].heartbeat.Stop()
	}

	// Close HTTP server — prevents new HTTP-triggered turns
	httpMu.Lock()
	if httpServer != nil {
		httpServer.Close()
	}
	httpMu.Unlock()

	// Wait for in-flight agent turns to complete naturally.
	// The turn lock prevents new turns from starting on sessions that are
	// already processing. With heartbeats stopped and HTTP closed, no new
	// turns will be initiated. We defer cancel() until AFTER this loop so
	// that in-flight turns (including exec subprocesses) aren't killed.
	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	gracefulShutdown(agents, shutdownTimeout)

	// Now cancel the context — stops Telegram bots and cleans up goroutines
	cancel()

	// Wait for Telegram bots to finish cleanup (ack processed updates)
	botMgr.Wait()
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg                config.AgentConfig
	cfg                 *config.Config
	configPath          string
	client              *anthropic.Client
	sessions            *session.Store
	store               *secrets.Store
	bwStore             *bitwarden.Store
	stateStore          *state.Store
	memIdx              *memory.Index
	reminderStore       *memory.ReminderStore
	scratchpadStore     *memory.Scratchpad
	todoStore           *memory.TodoStore
	sttProvider         voice.STT
	ttsProvider         voice.TTS
	braveKey            string
	anthropicOAuthToken string
	usageClient         *anthropic.UsageClient
	botMgr              *telegram.BotManager
	startTime           time.Time
	ctx                 context.Context
	agentListFn         func() []command.AgentInfo
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg

	// Default session key resolver — returns the session key for the agent's default chat.
	// Before any Telegram message arrives, this returns "" (no default set).
	// After the first message, it returns agent:<id>:chat:<chatID>.
	// The resolver is set to use the primary bot's DefaultSessionKey once wired.
	var defaultSessionKeyFn func() string

	defaultSessionKey := func() string {
		if defaultSessionKeyFn != nil {
			return defaultSessionKeyFn()
		}
		return ""
	}

	// Declare ag early so closures (tmux wake, etc.) can capture it.
	// Assigned later in this function.
	var ag *agent.Agent

	// Per-agent tool registry
	registry := tools.NewRegistry()

	// Async notifier: delivers results from auto-backgrounded exec commands
	// and tmux watch inactivity alerts to the agent session.
	// The response is delivered to Telegram via the primary bot's SendText.
	notifier := tools.NewAsyncNotifier(func(originSession, message string) {
		go func() {
			// Route to the originating session; fall back to default if unknown
			target := originSession
			if target == "" {
				target = defaultSessionKey()
			}
			resp, err := ag.HandleMessage(agent.WithTrigger(p.ctx, "async_notify"), target, message)
			if err != nil {
				log.Errorf("async_notify", "error: %v", err)
				return
			}
			log.Debugf("async_notify", "response length: %d", len(resp))
			if resp == "" {
				return
			}
			bot := p.botMgr.PrimaryBot(acfg.ID)
			if bot == nil {
				log.Warnf("async_notify", "no primary bot for agent %s, response not delivered", acfg.ID)
				return
			}
			if err := bot.SendText(resp); err != nil {
				log.Errorf("async_notify", "telegram delivery: %v", err)
			}
		}()
	})
	registry.Register(tools.NewExecTool(p.store, p.bwStore, p.cfg.Tools.ExecAutoBackground, notifier, acfg.Workspace))
	tmuxTool, tmuxClearAll := tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier, p.stateStore, "tmux:"+acfg.ID)
	registry.Register(tmuxTool)
	registry.Register(tools.NewReadTool())
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewEditTool())
	registry.Register(tools.NewWebFetchTool())
	registry.Register(tools.NewHTTPRequestTool(p.store, p.bwStore, p.cfg.Tools.TempDir, p.cfg.Tools.ExecAutoBackground, notifier))
	if p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	// Memory tools (shared stores, registered per-agent)
	if p.memIdx != nil {
		registry.Register(tools.NewMemorySearchTool(p.memIdx))
	}
	if p.reminderStore != nil {
		registry.Register(tools.NewMemoryRemindTool(p.reminderStore, acfg.ID))
	}
	if p.scratchpadStore != nil {
		registry.Register(tools.NewScratchpadWriteTool(p.scratchpadStore, acfg.ID))
		registry.Register(tools.NewScratchpadReadTool(p.scratchpadStore, acfg.ID))
		registry.Register(tools.NewScratchpadClearTool(p.scratchpadStore, acfg.ID))
		registry.Register(tools.NewScratchpadListTool(p.scratchpadStore, acfg.ID))
	}
	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}

	// Bitwarden tools (if enabled)
	if p.bwStore != nil {
		registry.Register(tools.NewBitwardenSearchTool(p.bwStore))
		registry.Register(tools.NewBitwardenUnlockTool(p.bwStore))
	}

	// Per-agent workspace bootstrap
	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.SystemFiles)
	bootstrap.SetSecretNames(p.store.Names(), p.bwStore != nil)
	checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)

	// Per-agent skills
	skillRegistry := skills.Load(p.cfg.Skills.Dirs)
	var extraSystemBlocks []anthropic.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}

	// Per-agent compactor
	compactor := compaction.NewCompactor(p.client, p.sessions, acfg.Model, p.cfg.Sessions.CompactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionModel,
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
	)
	compactor.Scratchpad = p.scratchpadStore
	compactor.AgentID = acfg.ID

	// Per-agent send_telegram tool (closure captures this agent's bot)
	registry.Register(tools.NewSendTelegramTool(func() tools.TelegramSender {
		bot := p.botMgr.PrimaryBot(acfg.ID)
		if bot == nil {
			return nil
		}
		return bot
	}))

	// send_to_session tool — inject messages into other sessions
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier))

	// Per-agent environment block
	var envBlock string
	if p.cfg.Environment.Enabled {
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg)
	}

	// Per-agent agent struct
	ag = &agent.Agent{
		Client:                      p.client,
		Sessions:                    p.sessions,
		Tools:                       registry,
		EnvironmentBlock:            envBlock,
		Bootstrap:                   bootstrap,
		Compactor:                   compactor,
		Reminders:                   p.reminderStore,
		AgentID:                     acfg.ID,
		Model:                       acfg.Model,
		ExtraSystemBlocks:           extraSystemBlocks,
		CacheStrategy:               p.cfg.Cache.Strategy,
		CacheBustDetect:             p.cfg.Logging.CacheBustDetect,
		DuplicateMessages:           acfg.DuplicateMessages,
		MaxResultChars:              p.cfg.Tools.MaxResultChars,
		ToolResultTempDir:           p.cfg.Tools.TempDir,
		StateStore:                  p.stateStore,
		UsageClient:                 p.usageClient,
		PromptRules:                 agent.CompilePromptRules(p.cfg.PromptRules),
		CompactionSummaryPromptPath: p.cfg.Sessions.CompactionSummaryPrompt,
		ReadPromptFile:              readPromptFile,
		CompactionHandoffMsg:        p.cfg.Sessions.CompactionHandoffMsg,
		MaxToolLoops:                acfg.MaxToolLoops,
		MaxOutputTokens:             acfg.MaxOutputTokens,
	}
	if p.store != nil && p.bwStore != nil {
		ag.Redact = func(text string) string {
			text = p.store.Redact(text)
			return p.bwStore.Redact(text)
		}
	} else if p.store != nil {
		ag.Redact = p.store.Redact
	} else if p.bwStore != nil {
		ag.Redact = p.bwStore.Redact
	}
	// Restore voice mode and seed session meta for default session (if any).
	// These are no-ops if no default session exists yet (first startup).
	if sk := defaultSessionKey(); sk != "" {
		ag.RestoreVoiceMode(sk)
		ag.SeedSessionMeta(sk)
	}

	// Warning injection queue (if enabled per-agent)
	if acfg.InjectAgentWarnings {
		warningWindow, err := time.ParseDuration(p.cfg.Logging.WarningWindowDuration)
		if err != nil {
			warningWindow = 5 * time.Minute
		}
		ag.Warnings = agent.NewWarningQueue(p.cfg.Logging.WarningMaxPerWindow, warningWindow)
	}

	// Mana threshold warnings (if thresholds configured)
	if len(p.cfg.ManaWarnings.Thresholds) > 0 {
		ag.ManaWatcher = agent.NewManaWatcher(p.cfg.ManaWarnings.Name, p.cfg.ManaWarnings.Thresholds)
		ag.ManaWatcher.SetStore(p.stateStore)
		ag.ManaWatcher.Restore()
	}

	// Spawn tool — replaces request_model, adds inherit (self-fork) mode.
	// Uses lazy getter for agent since ag is assigned later in this function.
	spawnDeps := tools.SpawnDeps{
		Client:     p.client,
		Bootstrap:  bootstrap,
		Sessions:   &sessionBranchAdapter{store: p.sessions},
		AgentID:    acfg.ID,
		Model:      acfg.Model,
		MaxInherit: p.cfg.Tools.MaxConcurrentSpawns,
		Notifier:   notifier,
	}
	registry.Register(tools.NewSpawnTool(spawnDeps, func() tools.SpawnAgent { return ag }))

	// TTS tool -- voice reply func is injected into tool context by the agent loop
	if p.ttsProvider != nil {
		registry.Register(tools.NewTTSTool(p.ttsProvider))
	}

	// Per-agent scheduled wakes
	var wakesMu sync.Mutex
	wakes := make(map[string]context.CancelFunc)
	wakeScheduleFn := func(delay time.Duration, message string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-time.After(delay):
				log.Infof("schedule_wake", "firing wake after %v for agent %s: %q", delay, acfg.ID, message)
				sk := defaultSessionKey()
				if sk == "" {
					log.Warnf("schedule_wake", "no default session for agent %s, skipping", acfg.ID)
					return
				}
				resp, err := ag.HandleMessage(agent.WithTrigger(p.ctx, "scheduled_wake"), sk, "[SCHEDULED WAKE]\n"+message)
				if err != nil {
					log.Errorf("schedule_wake", "error: %v", err)
				} else {
					log.Debugf("schedule_wake", "response: %s", resp)
				}
				wakesMu.Lock()
				delete(wakes, message)
				wakesMu.Unlock()
			case <-wakeCtx.Done():
				wakesMu.Lock()
				delete(wakes, message)
				wakesMu.Unlock()
			}
		}()
		wakesMu.Lock()
		wakes[message] = wakeCancel
		wakesMu.Unlock()
		return nil
	}
	registry.Register(tools.NewScheduleWakeTool(wakeScheduleFn))

	// Per-agent slash commands
	lastMsgStore := command.NewLastMessageStore()
	cmds := command.NewRegistry()
	cmds.Register(command.NewPingCommand())
	cmds.Register(command.NewStatusCommand(func() command.StatusInfo {
		sk := defaultSessionKey()
		return command.StatusInfo{
			AgentID:          acfg.ID,
			SessionKey:       sk,
			MessageCount:     sessionMessageCount(p.sessions, sk),
			Model:            ag.Model,
			Uptime:           time.Since(p.startTime),
			StartTime:        p.startTime,
			AgentBusy:        ag.IsProcessing(),
			CreatedAt:        p.sessions.CreatedAt(sk),
			LastActivity:     p.sessions.LastActivity(sk),
			ContextLimit:     compaction.ContextLimit(ag.Model),
			CompactThreshold: p.cfg.Sessions.CompactionThreshold,
		}
	}, p.cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewContextCommand(p.cfg.Logging.APIFile, func() command.ContextInfo {
		return command.ContextInfo{
			SessionKey:       defaultSessionKey(),
			Model:            ag.Model,
			CompactionThresh: p.cfg.Sessions.CompactionThreshold,
			ContextLimit:     compaction.ContextLimit(ag.Model),
		}
	}))
	cmds.Register(command.NewResetCommand(func() error {
		if ag.IsProcessing() {
			return fmt.Errorf("agent is processing — send /stop first, then /reset")
		}
		sk := defaultSessionKey()
		if sk == "" {
			return fmt.Errorf("no active session to reset")
		}
		fireResetHook(ag, p.sessions, sk, p.cfg, p.ctx)
		if err := p.sessions.Clear(sk); err != nil {
			return err
		}
		bootstrap.Reload()
		return nil
	}))
	cmds.Register(command.NewModelCommand(
		func() string { return ag.Model },
		func(m string) { ag.Model = m },
	))
	cmds.Register(command.NewToolsCommand(func() []command.ToolInfo {
		var infos []command.ToolInfo
		for _, t := range registry.All() {
			infos = append(infos, command.ToolInfo{Name: t.Name, Description: t.Description})
		}
		return infos
	}))
	cmds.Register(command.NewConfigCommand(func() string {
		return fmt.Sprintf("[agent]\nid = %q\nmodel = %q\nworkspace = %q\n\n[sessions]\ndir = %q\n\n[memory]\ndir = %q\n\n[http]\nbind = %q\nport = %d\n\n[logging]\nlevel = %q",
			acfg.ID, ag.Model, acfg.Workspace,
			p.cfg.Sessions.Dir, p.cfg.Memory.Dir,
			p.cfg.HTTP.Bind, p.cfg.HTTP.Port,
			p.cfg.Logging.Level)
	}))
	cmds.Register(command.NewLogCommand(p.cfg.Logging.EventFile))
	cmds.Register(command.NewErrorsCommand(p.cfg.Logging.EventFile))
	cmds.Register(command.NewVersionCommand(command.BuildInfo{
		Version:   version,
		GoVersion: goVersion,
		GitCommit: gitCommit,
		BuildTime: buildTime,
	}))
	cmds.Register(command.NewHelpCommand(cmds))

	// /usage command (shared usage client)
	cmds.Register(command.NewUsageCommand(func(ctx context.Context) (string, error) {
		usage, err := p.usageClient.GetUsage(ctx)
		if err != nil {
			return fmt.Sprintf("Error fetching usage: %v", err), nil
		}
		return anthropic.FormatUsage(usage), nil
	}))

	// Dynamic mana command (configurable name: /mana, /juice, /credits, etc.)
	manaName := p.cfg.ManaWarnings.Name
	if manaName == "" {
		manaName = "mana"
	}
	cmds.Register(command.NewManaCommand(manaName, func(ctx context.Context) (string, error) {
		usage, err := p.usageClient.GetUsage(ctx)
		if err != nil {
			return fmt.Sprintf("Error fetching %s: %v", manaName, err), nil
		}
		percent := anthropic.FormatMana(usage)
		if percent == "" {
			return fmt.Sprintf("%s: unknown", manaName), nil
		}
		result := fmt.Sprintf("%s: %s remaining", manaName, percent)
		if reset := anthropic.FormatManaReset(usage); reset != "" {
			result += fmt.Sprintf(" (resets %s)", reset)
		}
		return result, nil
	}))

	// /reload command
	cmds.Register(command.NewReloadCommand(func() (string, error) {
		bootstrap.Reload()
		checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)
		newSkillRegistry := skills.Load(p.cfg.Skills.Dirs)
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock(acfg.Workspace)},
			}
		}
		ag.ExtraSystemBlocks = newExtraSystemBlocks
		msg := fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: clod.toml config changes require a service restart to take effect. Prompt file changes take effect immediately.", newSkillRegistry.Len())
		return msg, nil
	}))

	// Custom script commands from config
	for _, cc := range p.cfg.Commands {
		cmds.Register(command.NewScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Skill slash commands (command + script in frontmatter)
	for _, s := range skillRegistry.All() {
		if s.Command != "" && s.Script != "" {
			name := strings.TrimPrefix(s.Command, "/")
			cmds.Register(command.NewScriptCommand(name, s.Description, s.Script, 30))
		}
	}

	// /voice command
	cmds.Register(command.NewVoiceCommand(
		func() bool { return ag.VoiceMode(defaultSessionKey()) },
		func(on bool) { ag.SetVoiceMode(defaultSessionKey(), on) },
	))

	// /multiball and /mb — per-agent pool first, shared pool fallback.
	// Forks from the requesting chat's session (per-chat routing).
	forkFn := func(ctx context.Context) (string, error) {
		if !p.botMgr.HasMultiball(acfg.ID) {
			return "", fmt.Errorf("no multiball bots configured")
		}
		secBot, ok := p.botMgr.AcquireMultiball(acfg.ID)
		if !ok {
			return "", fmt.Errorf("all multiball bots are busy")
		}

		// Re-wire the bot to this agent (needed when acquired from shared pool)
		secBot.SetAgentAndCommands(ag, cmds)

		// Determine parent session: use the requesting chat's session
		parentKey := defaultSessionKey()
		if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
			parentKey = telegram.SessionKeyForChat(acfg.ID, chatID)
		}
		if parentKey == "" {
			secBot.SetSessionKey("") // release back to pool
			return "", fmt.Errorf("no active session to fork from")
		}

		branchID := fmt.Sprintf("mb-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:multiball:%s", acfg.ID, branchID)

		if err := p.sessions.CreateBranch(parentKey, branchKey); err != nil {
			secBot.SetSessionKey("") // release back to pool
			return "", fmt.Errorf("create branch: %w", err)
		}

		// Inject fork prompt so the agent knows it's on a branch.
		// Uses configured file if set, otherwise a sensible default.
		var forkText string
		if acfg.ForkPrompt != "" {
			forkText = readPromptFile(acfg.ForkPrompt, "fork")
		}
		if forkText == "" {
			forkText = "You are a branch session forked from the main session. You can communicate with other sessions using the send_to_session tool — provide the session key and your message."
		}
		p.sessions.Append(branchKey, anthropic.Message{
			Role:    "user",
			Content: anthropic.TextContent(forkText),
		})

		secBot.SetSessionKey(branchKey)
		if primaryBot := p.botMgr.PrimaryBot(acfg.ID); primaryBot != nil {
			secBot.SetChatID(primaryBot.ChatID())
		}
		secBot.SendNotification("🎱 Forked from main. What do you need?")

		return fmt.Sprintf("Forked to @%s (session: %s)", secBot.Username(), branchKey), nil
	}
	cmds.Register(command.NewMultiballCommand(forkFn))
	cmds.Register(&command.Command{
		Name:        "mb",
		Description: "Fork session to a secondary bot (alias for /multiball)",
		Category:    "session",
		Hidden:      true,
		Execute: func(ctx context.Context, args string) (string, error) {
			return forkFn(ctx)
		},
	})
	agentNewDeps := &command.AgentNewDeps{
		ConfigPath:  p.configPath,
		DefaultsDir: filepath.Join(filepath.Dir(acfg.Workspace), "shared", "defaults"),
		HomeDir:     filepath.Dir(acfg.Workspace),
		ListFn:      p.agentListFn,
		SecretNames: func() []string { return p.store.Names() },
		BotNames: func() []string {
			names := make([]string, 0, len(p.cfg.Telegram.Bots))
			for name := range p.cfg.Telegram.Bots {
				names = append(names, name)
			}
			return names
		},
	}
	cmds.Register(command.NewAgentsCommand(p.agentListFn, cmds, agentNewDeps))
	cmds.Register(command.NewCompactCommand(func(ctx context.Context) (int, error) {
		if ag.Compactor == nil {
			return 0, fmt.Errorf("compaction is not configured")
		}
		sk := defaultSessionKey()
		if sk == "" {
			return 0, fmt.Errorf("no active session to compact")
		}
		mc, _ := p.sessions.MessageCount(sk)
		if mc < 5 {
			return 0, fmt.Errorf("too few messages to compact (%d)", mc)
		}
		if ag.CompactionNotifyFunc != nil {
			ag.CompactionNotifyFunc(sk, "⏳ Compacting context...")
		}
		system := bootstrap.SystemBlocks()
		summaryPrompt := readPromptFile(ag.CompactionSummaryPromptPath, "compaction")
		if err := ag.Compactor.Compact(ctx, sk, system, summaryPrompt, ag.CompactionHandoffMsg); err != nil {
			return 0, fmt.Errorf("compaction failed: %w", err)
		}
		if ag.CompactionNotifyFunc != nil {
			ag.CompactionNotifyFunc(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
		}
		bootstrap.Reload()
		return mc, nil
	}))
	cmds.Register(command.NewRepeatCommand(lastMsgStore))
	cmds.Register(command.NewSessionsCommand(command.SessionsDeps{
		AgentID: acfg.ID,
		ListFn: func() ([]command.SessionChatInfo, error) {
			chatSessions, err := p.sessions.ListChatSessions(acfg.ID)
			if err != nil {
				return nil, err
			}
			var result []command.SessionChatInfo
			for _, cs := range chatSessions {
				info := command.SessionChatInfo{
					ChatID:       cs.ChatID,
					MessageCount: cs.MessageCount,
					LastActivity: cs.LastActivity,
				}
				// Look up username from state store
				if p.stateStore != nil {
					var username string
					key := fmt.Sprintf("agent:%s:chat:%d:username", acfg.ID, cs.ChatID)
					if p.stateStore.Get(key, &username) {
						info.Username = username
					}
				}
				result = append(result, info)
			}
			return result, nil
		},
		SetDefaultFn: func(chatID int64) error {
			if p.stateStore == nil {
				return fmt.Errorf("no state store configured")
			}
			return p.stateStore.Set("agent:"+acfg.ID+":default_chat", chatID)
		},
		DefaultChatFn: func() int64 {
			if p.stateStore == nil {
				return 0
			}
			var chatID int64
			p.stateStore.Get("agent:"+acfg.ID+":default_chat", &chatID)
			return chatID
		},
	}))
	cmds.Register(command.NewSecretsCommand(p.store))
	cmds.Register(command.NewBitwardenCommand(p.bwStore, p.cfg.Bitwarden.Enabled))
	cmds.Register(command.NewRestartCommand(nil))

	// Auto-expose all slash commands as tools
	for _, cmd := range cmds.All() {
		if cmd.SkipToolExport {
			continue
		}
		if existingTool := registry.Get(cmd.Name); existingTool != nil {
			log.Fatalf("main", "agent %q: naming collision: command '%s' conflicts with existing tool", acfg.ID, cmd.Name)
		}
		registry.Register(tools.CreateCommandWrapperTool(cmd))
	}

	// Log registered tools
	allTools := registry.All()
	toolNames := make([]string, len(allTools))
	for i, t := range allTools {
		toolNames[i] = t.Name
	}
	log.Infof("main", "agent %q: registered %d tools: [%s]", acfg.ID, len(toolNames), strings.Join(toolNames, ", "))

	// Create and register Telegram bots via BotManager
	telegramToken := p.cfg.ResolveBotToken(acfg.TelegramBot, p.store)
	if telegramToken != "" {
		primaryBot, err := telegram.NewBot(telegramToken, p.cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, acfg.ID)
		if err != nil {
			log.Fatalf("main", "agent %q: create telegram bot: %v", acfg.ID, err)
		}

		if p.stateStore != nil {
			botKey := "bot:" + acfg.TelegramBot
			if botKey == "bot:" {
				botKey = "bot:" + acfg.ID
			}
			primaryBot.SetStateStore(p.stateStore, botKey)
		}

		if p.sttProvider != nil {
			primaryBot.SetTranscriber(p.sttProvider)
		}
		if p.ttsProvider != nil {
			primaryBot.SetTTS(voice.WithRate(p.ttsProvider, acfg.TTSRate))
		}
		primaryBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
		primaryBot.SetToolCallPreviewChars(p.cfg.Tools.ToolCallPreviewChars)

		// Wire cache bust alerts to this agent's bot
		if ag.CacheBustDetect {
			ag.CacheBustAlert = func(session string, prevRead, curRead int) {
				msg := fmt.Sprintf("⚠️ Cache bust: read dropped %d → %d on %s", prevRead, curRead, session)
				log.Warnf("agent", "%s", msg)
				primaryBot.SendNotification(msg)
			}
		}

		// Wire mana threshold warnings to Telegram
		if ag.ManaWatcher != nil {
			ag.ManaWarnFunc = func(warn string) {
				log.Warnf("mana", "%s", warn)
				primaryBot.SendNotification("⚠️ " + warn)
			}
		}

		// Wire rate limit notifications to Telegram
		ag.RateLimitFunc = func(retryAfter int) {
			msg := "I've hit my rate limit (mana exhausted). Mana refills on a rolling window — should have capacity again soon."
			if retryAfter > 0 {
				mins := (retryAfter + 59) / 60
				msg = fmt.Sprintf("I've hit my rate limit (mana exhausted). Should have capacity again in roughly %d minutes.", mins)
			}
			primaryBot.SendNotification("⚡ " + msg)
		}

		// Wire max_tokens warnings to Telegram
		ag.MaxTokensWarnFunc = func(warn string) {
			primaryBot.SendNotification("⚠️ " + warn)
		}

		// Wire compaction notifications to Telegram (default on)
		if p.cfg.Sessions.CompactionNotify == nil || *p.cfg.Sessions.CompactionNotify {
			ag.CompactionNotifyFunc = func(session string, msg string) {
				primaryBot.SendNotification(msg)
			}
		}

		p.botMgr.AddPrimary(acfg.ID, primaryBot)

		// Per-agent multiball bots (if configured)
		for _, botName := range acfg.MultiballBots {
			mbToken := p.cfg.ResolveBotToken(botName, p.store)
			if mbToken == "" {
				log.Errorf("main", "agent %q: multiball bot %q: token not found", acfg.ID, botName)
				continue
			}
			mbBot, err := telegram.NewBot(mbToken, p.cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, "") // secondary: no agentID
			if err != nil {
				log.Errorf("main", "agent %q: create multiball bot %q: %v", acfg.ID, botName, err)
				continue
			}
			if p.sttProvider != nil {
				mbBot.SetTranscriber(p.sttProvider)
			}
			if p.ttsProvider != nil {
				mbBot.SetTTS(p.ttsProvider)
			}
			mbBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
			if p.stateStore != nil {
				ss := p.stateStore
				mbBot.OnSessionKeyChange = func(username, sessionKey string) {
					key := "multiball:" + username
					if sessionKey == "" {
						ss.Delete(key)
					} else {
						ss.Set(key, sessionKey)
					}
				}
			}
			p.botMgr.AddMultiball(acfg.ID, mbBot)
		}
		if pool := p.botMgr.Pool(acfg.ID); pool != nil && pool.Size() > 0 {
			log.Infof("main", "agent %q: %d per-agent multiball bots ready", acfg.ID, pool.Size())
		}

		// Configure session TTL for per-agent multiball pool
		if pool := p.botMgr.Pool(acfg.ID); pool != nil {
			ttl, _ := time.ParseDuration(p.cfg.Telegram.MultiballSessionTTL) // validated earlier
			if ttl > 0 {
				pool.SetSessionTTL(ttl, p.sessions)
				log.Infof("main", "agent %q: multiball session TTL = %v", acfg.ID, ttl)
			}
			pool.ReclaimHook = func(sessionKey string) {
				fireResetHook(ag, p.sessions, sessionKey, p.cfg, p.ctx)
			}
		}
	}

	// Per-agent heartbeat — uses a lazy session key resolver
	interval, err := time.ParseDuration(acfg.HeartbeatInterval)
	if err != nil {
		log.Warnf("main", "agent %q: invalid heartbeat_interval %q, using default 45m", acfg.ID, acfg.HeartbeatInterval)
		interval = 45 * time.Minute
	}
	hb := agent.NewHeartbeat(ag, "", interval)
	hb.SessionKeyFn = defaultSessionKey

	// Wire the default session key function after bot creation.
	// Must be deferred because primaryBot may not exist yet.
	defer func() {
		bot := p.botMgr.PrimaryBot(acfg.ID)
		if bot != nil {
			defaultSessionKeyFn = bot.DefaultSessionKey
		}
	}()

	return &agentInstance{
		id:                acfg.ID,
		ag:                ag,
		cmds:              cmds,
		registry:          registry,
		bootstrap:         bootstrap,
		defaultSessionKey: defaultSessionKey,
		heartbeat:         hb,
		agentCfg:          acfg,
		tmuxClearAll:      tmuxClearAll,
	}
}

// checkSystemPromptSizes logs warnings if system prompt files exceed thresholds.
func checkSystemPromptSizes(bootstrap *workspace.Bootstrap, sess config.SessionsConfig, agentID string) {
	maxFile := sess.MaxSystemPromptFile
	if maxFile == 0 {
		maxFile = 20000
	}
	maxTotal := sess.MaxSystemPromptTotal
	if maxTotal == 0 {
		maxTotal = 80000
	}
	for _, w := range bootstrap.CheckSizes(maxFile, maxTotal) {
		log.Warnf(agentID, "%s", w)
	}
}

// buildEnvironmentBlock generates the environment system block content
// from config values known at startup.
func buildEnvironmentBlock(acfg config.AgentConfig, configPath string, cfg *config.Config) string {
	logDir := filepath.Dir(cfg.Logging.EventFile)

	var b strings.Builder
	b.WriteString("# Environment\n\n")
	b.WriteString("You are running on **clod**, an AI agent platform.\n\n")

	// Workspace
	b.WriteString("## Workspace\n")
	fmt.Fprintf(&b, "- Workspace: %s\n", acfg.Workspace)
	fmt.Fprintf(&b, "- Agent ID: %s\n", acfg.ID)
	b.WriteString("- Platform: clod (https://github.com/richardtkemp/clod)\n")
	if cfg.Environment.DocsPath != "" {
		fmt.Fprintf(&b, "- Platform docs: %s\n", cfg.Environment.DocsPath)
	}
	if acfg.TelegramBot != "" {
		b.WriteString("- Messaging: Telegram\n")
	}

	// Paths
	b.WriteString("\n## Paths\n")
	fmt.Fprintf(&b, "- Config: %s\n", configPath)
	fmt.Fprintf(&b, "- Logs: %s\n", logDir)

	// Message Metadata
	b.WriteString("\n## Message Metadata\n")
	b.WriteString("Every inbound message includes a `[meta]` header with:\n")
	b.WriteString("- **time** — UTC timestamp\n")
	b.WriteString("- **gap** — time since last message\n")
	b.WriteString("- **model** — current model\n")
	b.WriteString("- **prev_cost** — USD equivalent cost of previous turn\n")
	b.WriteString("- **prev_tokens** — token breakdown: in (new input), out (output), cR (cache read), cW (cache write)\n")
	b.WriteString("- **mana** — remaining API quota percentage\n")

	// Session Structure
	b.WriteString("\n## Session Structure\n")
	b.WriteString("Your context is assembled from: this environment block, character files, a secrets list, and the conversation history.\n")
	sysFiles := acfg.SystemFiles
	if len(sysFiles) == 0 {
		sysFiles = workspace.DefaultFileOrder
	}
	b.WriteString("Character files (in order): ")
	b.WriteString(strings.Join(sysFiles, ", "))
	b.WriteString("\n")
	b.WriteString("The human only sees the conversation — they cannot see your system prompt, character files, or this environment block. ")
	b.WriteString("Do not assume shared context when referencing system prompt content. If you need the human to understand something from your instructions, explain it in your own words.\n")

	return b.String()
}

func sessionMessageCount(sessions *session.Store, key string) int {
	n, err := sessions.MessageCount(key)
	if err != nil {
		log.Warnf("main", "message count for %s: %v", key, err)
		return 0
	}
	return n
}

// gracefulShutdown waits for all in-flight agent turns to complete, up to the
// configured timeout. This allows exec subprocesses and API calls to finish
// naturally before the context is cancelled.
func gracefulShutdown(agents map[string]*agentInstance, timeout time.Duration) {
	const tickInterval = 100 * time.Millisecond
	deadline := time.After(timeout)

	for {
		var anyBusy bool
		for _, inst := range agents {
			if inst.ag.IsProcessing() {
				anyBusy = true
				break
			}
		}
		if !anyBusy {
			return
		}
		select {
		case <-deadline:
			var parts []string
			now := time.Now()
			for id, inst := range agents {
				for _, d := range inst.ag.ProcessingDetails() {
					s := fmt.Sprintf("%s(session=%s", id, d.SessionKey)
					if d.ToolName != "" {
						s += fmt.Sprintf(", tool=%s", d.ToolName)
					}
					if d.Trigger != "" {
						s += fmt.Sprintf(", trigger=%s", d.Trigger)
					}
					s += fmt.Sprintf(", elapsed=%s)", now.Sub(d.StartTime).Truncate(time.Second))
					parts = append(parts, s)
				}
			}
			if len(parts) == 0 {
				// Shouldn't happen, but be safe
				log.Warnf("main", "graceful shutdown timed out after %s — agents still processing (no detail available)", timeout)
			} else {
				log.Warnf("main", "graceful shutdown timed out after %s — blocking: %s", timeout, strings.Join(parts, ", "))
			}
			return
		default:
			time.Sleep(tickInterval)
		}
	}
}

// injectWelcomeFile checks for a welcome/changelog file written by setup.sh
// on update. If found, returns the file contents and deletes the file.
// Returns empty string if no file exists or file is empty.
func injectWelcomeFile(path string, agents map[string]*agentInstance, agentOrder []string, sessions *session.Store) string {
	if path == "" || len(agentOrder) == 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // file doesn't exist — normal for non-update starts
	}
	content := strings.TrimSpace(string(data))
	if err := os.Remove(path); err != nil {
		log.Warnf("main", "remove welcome file: %v", err)
	}
	if content == "" {
		return ""
	}

	log.Infof("main", "found welcome file for agent %s (%d bytes)", agentOrder[0], len(content))
	return content
}

// AgentMemoryBoost is the weight added to agent-specific memory sources.
// With a boost of 1.0, an agent-specific source with weight 0.5 gets an
// effective weight of 1.5 (multiplier = 1.0 + 1.5 = 2.5), making it rank
// higher than global sources with the same base weight.
const AgentMemoryBoost = 1.0

// buildAgentMemorySources combines global memory sources with agent-specific
// sources. Agent-specific sources get a weight boost to rank higher.
func buildAgentMemorySources(globalSources map[string]memory.SourceConfig, agentSources []config.MemorySource) map[string]memory.SourceConfig {
	combined := make(map[string]memory.SourceConfig, len(globalSources)+len(agentSources))

	// Add global sources as-is
	for name, src := range globalSources {
		combined[name] = src
	}

	// Add agent-specific sources with weight boost
	for _, src := range agentSources {
		combined["agent:"+src.Name] = memory.SourceConfig{
			Dir:    src.Dir,
			Weight: src.Weight + AgentMemoryBoost,
		}
	}

	return combined
}

// readPromptFile reads a prompt from a file path. Returns the trimmed contents,
// or empty string (with error logged) if the file can't be read.
func readPromptFile(path, label string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Errorf(label, "read prompt file %s: %v", path, err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

// fireResetHook sends the reset prompt to the agent before a session is cleared.
// Checks BranchMeta.NoResetHook for branch sessions. Non-fatal: logs and returns
// on error so the caller can proceed with the reset.
func fireResetHook(ag *agent.Agent, sessions *session.Store, sessionKey string, cfg *config.Config, parentCtx context.Context) {
	prompt := readPromptFile(cfg.Sessions.SessionResetPrompt, "reset-hook")
	if prompt == "" {
		return
	}

	// Check branch metadata for NoResetHook
	meta, err := sessions.GetBranchMeta(sessionKey)
	if err != nil {
		log.Warnf("reset-hook", "check branch meta for %s: %v", sessionKey, err)
	}
	if meta != nil && meta.NoResetHook {
		log.Debugf("reset-hook", "skipping for %s (no_reset_hook set)", sessionKey)
		return
	}

	hookCtx, cancel := context.WithTimeout(parentCtx, 60*time.Second)
	defer cancel()
	hookCtx = agent.WithTrigger(hookCtx, "reset_hook")
	hookCtx = agent.WithNoCompact(hookCtx)

	log.Infof("reset-hook", "firing reset hook for %s", sessionKey)
	if _, err := ag.HandleMessage(hookCtx, sessionKey, prompt); err != nil {
		log.Warnf("reset-hook", "hook failed for %s: %v", sessionKey, err)
	}
}

// sessionBranchAdapter wraps session.Store to implement tools.SessionBrancher.
type sessionBranchAdapter struct {
	store *session.Store
}

func (a *sessionBranchAdapter) CreateBranch(parentKey, branchKey string, noResetHook bool) error {
	return a.store.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		NoResetHook: noResetHook,
	})
}

// extractAgentID extracts the agent ID from a session key.
// Session keys have the format "agent:<id>:..." — returns the second segment.
func extractAgentID(sessionKey string) string {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// restoreMultiballSessions restores persisted multiball session mappings after restart.
// For each secondary bot in all pools, it looks up "multiball:<username>" in stateStore.
// If a saved session key exists and the session file is still active, the bot is restored.
func restoreMultiballSessions(
	botMgr *telegram.BotManager,
	stateStore *state.Store,
	sessions *session.Store,
	agents map[string]*agentInstance,
	agentOrder []string,
) {
	// Collect all pools to iterate
	type poolInfo struct {
		pool *telegram.Pool
		name string
	}
	var pools []poolInfo
	for _, id := range agentOrder {
		if pool := botMgr.Pool(id); pool != nil {
			pools = append(pools, poolInfo{pool: pool, name: "agent:" + id})
		}
	}
	if sp := botMgr.SharedPool(); sp != nil {
		pools = append(pools, poolInfo{pool: sp, name: "shared"})
	}

	restored := 0
	for _, pi := range pools {
		pi.pool.ForEach(func(bot *telegram.Bot) {
			username := bot.Username()
			if username == "" {
				return
			}
			var savedKey string
			if !stateStore.Get("multiball:"+username, &savedKey) || savedKey == "" {
				return
			}

			// Validate session still exists on disk
			if sessions.LastActivity(savedKey) == "n/a" {
				log.Infof("main", "multiball restore: @%s session %s no longer exists, cleaning up", username, savedKey)
				stateStore.Delete("multiball:" + username)
				return
			}

			// Restore session key (bypass callback — already persisted)
			bot.SetSessionKeyDirect(savedKey)

			// Re-wire agent if we can identify it from the session key
			agentID := extractAgentID(savedKey)
			if inst, ok := agents[agentID]; ok {
				bot.SetAgentAndCommands(inst.ag, inst.cmds)
			}

			// Copy chatID from primary bot so notifications work
			if agentID != "" {
				if primary := botMgr.PrimaryBot(agentID); primary != nil {
					if chatID := primary.ChatID(); chatID != 0 {
						bot.SetChatID(chatID)
					}
				}
			}

			restored++
			log.Infof("main", "multiball restore: @%s → %s", username, savedKey)
		})
	}
	if restored > 0 {
		log.Infof("main", "restored %d multiball session(s) from state", restored)
	}
}

// checkManaPrereqs returns warnings if mana detection prerequisites are missing.
func checkManaPrereqs(credFile string) []string {
	var warnings []string
	if _, err := exec.LookPath("claude"); err != nil {
		warnings = append(warnings, "mana: 'claude' not found in PATH — mana detection requires Claude Code to be installed")
	}
	if credFile != "" {
		if _, err := os.Stat(credFile); os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("mana: credentials file not found at %s — mana detection requires Claude Code to be running periodically to refresh the OAuth token", credFile))
		}
	}
	return warnings
}

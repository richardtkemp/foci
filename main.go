package main

import (
	"context"
	"encoding/json"
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

	"clod/agent"
	"clod/anthropic"
	"clod/command"
	"clod/compaction"
	"clod/config"
	"clod/log"
	"clod/memory"
	"clod/secrets"
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
	id         string
	ag         *agent.Agent
	cmds       *command.Registry
	registry   *tools.Registry
	bootstrap  *workspace.Bootstrap
	sessionKey string
	heartbeat  *agent.Heartbeat
	agentCfg   config.AgentConfig
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

	// Shared: Anthropic client
	httpTimeout, err := time.ParseDuration(cfg.Anthropic.HTTPTimeout)
	if err != nil {
		log.Warnf("main", "invalid anthropic.http_timeout, using default: %v", err)
		httpTimeout = 120 * time.Second
	}
	client := anthropic.NewClientWithTimeout(anthropicToken, httpTimeout)

	// Shared: Session store
	sessions := session.NewStore(cfg.Sessions.Dir)

	// Repair sessions with orphaned tool_use blocks (from mid-tool-call restarts)
	if n, err := sessions.RepairOrphans(); err != nil {
		log.Warnf("main", "session repair: %v", err)
	} else if n > 0 {
		log.Infof("main", "repaired %d orphaned session(s) with interrupted tool calls", n)
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
		}
		log.Infof("main", "voice TTS enabled (edge-tts, voice=%s)", cfg.Voice.TTSVoice)
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
		}
		log.Infof("main", "voice TTS enabled (openai, %s, voice=%s)", ttsModel, ttsVoice)
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

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string // preserve config order

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
			stateStore:          stateStore,
			memIdx:              agentMemIdx,
			reminderStore:       reminderStore,
			scratchpadStore:     scratchpadStore,
			sttProvider:         sttProvider,
			ttsProvider:         ttsProvider,
			braveKey:            braveKey,
			anthropicOAuthToken: anthropicOAuthToken,
			usageClient:         usageClient,
			botMgr:              botMgr,
			startTime:           startTime,
			ctx:                 ctx,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)
		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// Wire log warnings into agent sessions (if enabled)
	if cfg.Logging.InjectAgentWarnings {
		log.WarnHook = func(level log.Level, component string, msg string) {
			for _, inst := range agents {
				if inst.ag.Warnings != nil {
					inst.ag.Warnings.Push(level.String(), component, msg)
				}
			}
		}
		log.Infof("main", "warning injection into agent sessions enabled")
	}

	// Start all bots
	botMgr.StartAll(ctx)

	// Send startup notifications to users via Telegram (if enabled)
	if cfg.Telegram.EnableStartupNotify {
		botMgr.SendStartupNotifications()
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
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}

		sessionKey := inst.sessionKey
		if req.Session != "" {
			sessionKey = fmt.Sprintf("agent:%s:%s", inst.id, req.Session)
		}

		log.Infof("http", "send (agent=%s, session=%s): %s", inst.id, sessionKey, req.Text)

		// Route slash commands through the command dispatcher
		if strings.HasPrefix(req.Text, "/") {
			if result, ok := inst.cmds.Dispatch(ctx, req.Text); ok {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"response": result})
				return
			}
		}

		resp, err := inst.ag.HandleMessage(ctx, sessionKey, req.Text)
		if err != nil {
			log.Errorf("http", "send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		inst.heartbeat.Reset()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
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
			http.Error(w, fmt.Sprintf("unknown agent: %q", agentID), http.StatusBadRequest)
			return
		}
		result, ok := inst.cmds.Dispatch(context.Background(), "/status")
		if !ok {
			http.Error(w, "status command not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": result})
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
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		result, ok := inst.cmds.Dispatch(context.Background(), req.Command)
		if !ok {
			http.Error(w, "unknown command", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": result})
	})

	// POST /wake — branch session for cron/external triggers
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Agent string `json:"agent"`
			Text  string `json:"text"`
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
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			req.Text = "[WAKE]"
		}

		// Create a branch session for this wake call
		parentKey := inst.sessionKey
		branchID := fmt.Sprintf("wake-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", inst.id, branchID)

		if err := sessions.CreateBranch(parentKey, branchKey); err != nil {
			log.Errorf("wake", "branch error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Infof("wake", "branch %s from %s, text=%q", branchKey, parentKey, req.Text)

		resp, err := inst.ag.HandleMessage(ctx, branchKey, req.Text)
		if err != nil {
			log.Errorf("wake", "error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		inst.heartbeat.Reset()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

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
	injectWelcomeFile(cfg.WelcomeFile, agents, agentOrder, sessions)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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
	gracefulShutdown(agents)

	// Now cancel the context — stops Telegram bots and cleans up goroutines
	cancel()
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg                config.AgentConfig
	cfg                 *config.Config
	configPath          string
	client              *anthropic.Client
	sessions            *session.Store
	store               *secrets.Store
	stateStore          *state.Store
	memIdx              *memory.Index
	reminderStore       *memory.ReminderStore
	scratchpadStore     *memory.Scratchpad
	sttProvider         voice.STT
	ttsProvider         voice.TTS
	braveKey            string
	anthropicOAuthToken string
	usageClient         *anthropic.UsageClient
	botMgr              *telegram.BotManager
	startTime           time.Time
	ctx                 context.Context
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg
	sessionKey := fmt.Sprintf("agent:%s:main", acfg.ID)

	// Declare ag early so closures (tmux wake, etc.) can capture it.
	// Assigned later in this function.
	var ag *agent.Agent

	// Per-agent tool registry
	registry := tools.NewRegistry()

	// Async notifier: delivers results from auto-backgrounded exec commands
	// and tmux watch inactivity alerts to the agent session.
	// The response is delivered to Telegram via the primary bot's SendText.
	notifier := tools.NewAsyncNotifier(func(message string) {
		go func() {
			resp, err := ag.HandleMessage(p.ctx, sessionKey, message)
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
	registry.Register(tools.NewExecTool(p.store, p.cfg.Tools.ExecAutoBackground, notifier, acfg.Workspace))
	registry.Register(tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier))
	registry.Register(tools.NewReadTool())
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewEditTool())
	registry.Register(tools.NewWebFetchTool())
	if p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	// Memory tools (shared stores, registered per-agent)
	if p.memIdx != nil {
		registry.Register(tools.NewMemorySearchTool(p.memIdx))
	}
	if p.reminderStore != nil {
		registry.Register(tools.NewMemoryRemindTool(p.reminderStore))
	}
	if p.scratchpadStore != nil {
		registry.Register(tools.NewScratchpadWriteTool(p.scratchpadStore))
		registry.Register(tools.NewScratchpadReadTool(p.scratchpadStore))
		registry.Register(tools.NewScratchpadClearTool(p.scratchpadStore))
		registry.Register(tools.NewScratchpadListTool(p.scratchpadStore))
	}

	// Per-agent workspace bootstrap
	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.SystemFiles)
	bootstrap.SetSecretNames(p.store.Names())

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

	// Per-agent send_telegram tool (closure captures this agent's bot)
	registry.Register(tools.NewSendTelegramTool(func() tools.TelegramSender {
		bot := p.botMgr.PrimaryBot(acfg.ID)
		if bot == nil {
			return nil
		}
		return bot
	}))

	// Per-agent agent struct
	ag = &agent.Agent{
		Client:                  p.client,
		Sessions:                p.sessions,
		Tools:                   registry,
		Bootstrap:               bootstrap,
		Compactor:               compactor,
		Reminders:               p.reminderStore,
		Model:                   acfg.Model,
		ExtraSystemBlocks:       extraSystemBlocks,
		CacheStrategy:           p.cfg.Cache.Strategy,
		CacheBustDetect:         p.cfg.Logging.CacheBustDetect,
		DuplicateMessages:       acfg.DuplicateMessages,
		MaxResultChars:          p.cfg.Tools.MaxResultChars,
		ToolResultTempDir:       p.cfg.Tools.TempDir,
		StateStore:              p.stateStore,
		UsageClient:             p.usageClient,
		PromptRules:             agent.CompilePromptRules(p.cfg.PromptRules),
		CompactionSummaryPrompt: p.cfg.Sessions.CompactionSummaryPrompt,
		CompactionHandoffMsg:    p.cfg.Sessions.CompactionHandoffMsg,
		MaxToolLoops:            acfg.MaxToolLoops,
		MaxOutputTokens:         acfg.MaxOutputTokens,
	}
	ag.RestoreVoiceMode(sessionKey)

	// Warning injection queue (if enabled)
	if p.cfg.Logging.InjectAgentWarnings {
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

	// Model escalation tool (needs this agent's bootstrap)
	registry.Register(tools.NewRequestModelTool(p.client, bootstrap))

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
				resp, err := ag.HandleMessage(p.ctx, sessionKey, "[SCHEDULED WAKE]\n"+message)
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
		return command.StatusInfo{
			SessionKey:   sessionKey,
			MessageCount: sessionMessageCount(p.sessions, sessionKey),
			Model:        ag.Model,
			Uptime:       time.Since(p.startTime),
			AgentBusy:    ag.IsProcessing(),
		}
	}, p.cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewContextCommand(p.cfg.Logging.APIFile, func() command.ContextInfo {
		return command.ContextInfo{
			SessionKey:       sessionKey,
			Model:            ag.Model,
			CompactionThresh: p.cfg.Sessions.CompactionThreshold,
			ContextLimit:     compaction.ContextLimit(ag.Model),
		}
	}))
	cmds.Register(command.NewResetCommand(func() error {
		if ag.IsProcessing() {
			return fmt.Errorf("agent is processing — send /stop first, then /reset")
		}
		if err := p.sessions.Clear(sessionKey); err != nil {
			return err
		}
		bootstrap.Reload()
		return nil
	}))
	cmds.Register(command.NewModelCommand(
		func() string { return ag.Model },
		func(m string) { ag.Model = m },
	))
	cmds.Register(command.NewSessionCommand(func() command.SessionInfo {
		return command.SessionInfo{
			SessionKey:   sessionKey,
			MessageCount: sessionMessageCount(p.sessions, sessionKey),
			CreatedAt:    p.sessions.CreatedAt(sessionKey),
			LastActivity: p.sessions.LastActivity(sessionKey),
		}
	}))
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
	cmds.Register(command.NewUptimeCommand(p.startTime))
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
		newSkillRegistry := skills.Load(p.cfg.Skills.Dirs)
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock(acfg.Workspace)},
			}
		}
		ag.ExtraSystemBlocks = newExtraSystemBlocks
		msg := fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: clod.toml config changes require a service restart to take effect.", newSkillRegistry.Len())
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
		func() bool { return ag.VoiceMode(sessionKey) },
		func(on bool) { ag.SetVoiceMode(sessionKey, on) },
	))

	// /multiball and /mb — per-agent, uses this agent's pool
	forkFn := func() (string, error) {
		pool := p.botMgr.Pool(acfg.ID)
		if pool == nil || pool.Size() == 0 {
			return "", fmt.Errorf("no secondary bots configured")
		}
		secBot, ok := pool.Acquire()
		if !ok {
			return "", fmt.Errorf("all secondary bots are busy")
		}

		branchID := fmt.Sprintf("mb-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:multiball:%s", acfg.ID, branchID)

		if err := p.sessions.CreateBranch(sessionKey, branchKey); err != nil {
			pool.Release(secBot)
			return "", fmt.Errorf("create branch: %w", err)
		}

		// Inject fork prompt so the agent knows it's on a branch
		if fp := acfg.ForkPrompt; fp != "" {
			p.sessions.AppendAll(branchKey, []anthropic.Message{
				{Role: "user", Content: anthropic.TextContent(fp)},
				{Role: "assistant", Content: anthropic.TextContent("Understood.")},
			})
		}

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
		Execute: func(ctx context.Context, args string) (string, error) {
			return forkFn()
		},
	})
	cmds.Register(command.NewRepeatCommand(lastMsgStore))

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

	// Create and register Telegram bots via BotManager
	telegramToken := p.cfg.ResolveBotToken(acfg.TelegramBot, p.store)
	if telegramToken != "" {
		primaryBot, err := telegram.NewBot(telegramToken, p.cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, sessionKey)
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
			primaryBot.SetTTS(p.ttsProvider)
		}
		primaryBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)

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

		p.botMgr.AddPrimary(acfg.ID, primaryBot)

		// Multiball bot (if configured)
		if acfg.MultiballBot != "" {
			mbToken := p.cfg.ResolveBotToken(acfg.MultiballBot, p.store)
			if mbToken != "" {
				mbBot, err := telegram.NewBot(mbToken, p.cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, "")
				if err != nil {
					log.Errorf("main", "agent %q: create multiball bot: %v", acfg.ID, err)
				} else {
					if p.sttProvider != nil {
						mbBot.SetTranscriber(p.sttProvider)
					}
					if p.ttsProvider != nil {
						mbBot.SetTTS(p.ttsProvider)
					}
					mbBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
					p.botMgr.AddMultiball(acfg.ID, mbBot)
					log.Infof("main", "agent %q: multiball bot ready", acfg.ID)
				}
			}
		} else {
			// Legacy: secondary bots from [telegram] config
			secondaryTokens := p.cfg.Telegram.SecondaryBots
			if v, ok := p.store.Get("telegram.secondary_bots"); ok && v != "" {
				for _, t := range strings.Split(v, ",") {
					if t = strings.TrimSpace(t); t != "" {
						secondaryTokens = append(secondaryTokens, t)
					}
				}
			}
			for _, token := range secondaryTokens {
				secBot, err := telegram.NewBot(token, p.cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, "")
				if err != nil {
					log.Errorf("main", "agent %q: create secondary bot: %v", acfg.ID, err)
					continue
				}
				if p.sttProvider != nil {
					secBot.SetTranscriber(p.sttProvider)
				}
				if p.ttsProvider != nil {
					secBot.SetTTS(p.ttsProvider)
				}
				secBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
				p.botMgr.AddMultiball(acfg.ID, secBot)
			}
			if pool := p.botMgr.Pool(acfg.ID); pool != nil && pool.Size() > 0 {
				log.Infof("main", "agent %q: %d multiball bots ready", acfg.ID, pool.Size())
			}
		}

		// Configure session TTL for multiball pool (auto-reclaim stale sessions)
		if pool := p.botMgr.Pool(acfg.ID); pool != nil {
			ttl, _ := time.ParseDuration(p.cfg.Telegram.MultiballSessionTTL) // validated earlier
			if ttl > 0 {
				pool.SetSessionTTL(ttl, p.sessions)
				log.Infof("main", "agent %q: multiball session TTL = %v", acfg.ID, ttl)
			}
		}
	}

	// Per-agent heartbeat
	interval, err := time.ParseDuration(acfg.HeartbeatInterval)
	if err != nil {
		interval = 45 * time.Minute
	}
	hb := agent.NewHeartbeat(ag, sessionKey, interval)

	return &agentInstance{
		id:         acfg.ID,
		ag:         ag,
		cmds:       cmds,
		registry:   registry,
		bootstrap:  bootstrap,
		sessionKey: sessionKey,
		heartbeat:  hb,
		agentCfg:   acfg,
	}
}

func sessionMessageCount(sessions *session.Store, key string) int {
	n, err := sessions.MessageCount(key)
	if err != nil {
		log.Warnf("main", "message count for %s: %v", key, err)
		return 0
	}
	return n
}

// gracefulShutdown waits up to 5 seconds for all in-flight agent turns to complete.
// This allows exec subprocesses and API calls to finish naturally before the
// context is cancelled.
func gracefulShutdown(agents map[string]*agentInstance) {
	const maxWaitTicks = 50
	const tickInterval = 100 * time.Millisecond

	for i := 0; i < maxWaitTicks; i++ {
		anyBusy := false
		for _, inst := range agents {
			if inst.ag.IsProcessing() {
				anyBusy = true
				break
			}
		}
		if !anyBusy {
			return
		}
		time.Sleep(tickInterval)
	}
	log.Warnf("main", "graceful shutdown timed out — some agents still processing")
}

// injectWelcomeFile checks for a welcome/changelog file written by setup.sh
// on update. If found, appends its contents to the most recent active session
// for the first agent, then deletes the file.
func injectWelcomeFile(path string, agents map[string]*agentInstance, agentOrder []string, sessions *session.Store) {
	if path == "" || len(agentOrder) == 0 {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // file doesn't exist — normal for non-update starts
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		os.Remove(path)
		return
	}

	// Inject into the first agent's main session
	inst := agents[agentOrder[0]]
	msg := fmt.Sprintf("[SYSTEM UPDATE]\n%s", content)
	sessions.AppendAll(inst.sessionKey, []anthropic.Message{
		{Role: "user", Content: anthropic.TextContent(msg)},
		{Role: "assistant", Content: anthropic.TextContent("Update acknowledged. I'll review the changes.")},
	})
	log.Infof("main", "injected welcome file into session %s", inst.sessionKey)

	if err := os.Remove(path); err != nil {
		log.Warnf("main", "remove welcome file: %v", err)
	}
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

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
	client := anthropic.NewClient(anthropicToken)

	// Shared: Session store
	sessions := session.NewStore(cfg.Sessions.Dir)

	// Shared: Memory FTS5 index
	var memIdx *memory.Index
	var reminderStore *memory.ReminderStore
	var scratchpadStore *memory.Scratchpad
	if cfg.Memory.Dir != "" || len(cfg.Memory.Sources) > 0 {
		memDbPath := filepath.Join(filepath.Dir(configPath), "memory.db")

		// Build source map from config
		sources := make(map[string]memory.SourceConfig)

		if len(cfg.Memory.Sources) > 0 {
			// Use new multi-source config
			for _, src := range cfg.Memory.Sources {
				sources[src.Name] = memory.SourceConfig{
					Dir:    src.Dir,
					Weight: src.Weight,
				}
			}
		} else if cfg.Memory.Dir != "" {
			// Backward compat: single dir with default weight
			sources["memory"] = memory.SourceConfig{
				Dir:    cfg.Memory.Dir,
				Weight: 1.0,
			}
		}

		// Parse debounce delay
		debounce := time.Duration(0)
		if cfg.Memory.ReindexDebounce != "" {
			debounce, err = time.ParseDuration(cfg.Memory.ReindexDebounce)
			if err != nil {
				log.Fatalf("main", "invalid reindex_debounce: %v", err)
			}
		}

		memIdx, err = memory.NewIndex(memDbPath, sources, debounce)
		if err != nil {
			log.Fatalf("main", "create memory index: %v", err)
		}
		defer memIdx.Close()

		if err := memIdx.Reindex(); err != nil {
			log.Errorf("main", "reindex memory: %v", err)
		}

		// Start file watching if debounce is configured
		if debounce > 0 || len(cfg.Memory.Sources) > 0 {
			if err := memIdx.Watch(); err != nil {
				log.Errorf("main", "start memory file watching: %v", err)
			}
		}

		// Reminder store (same DB directory)
		reminderDbPath := filepath.Join(filepath.Dir(configPath), "reminders.db")
		reminderStore, err = memory.NewReminderStore(reminderDbPath)
		if err != nil {
			log.Fatalf("main", "create reminder store: %v", err)
		}
		defer reminderStore.Close()

		// Scratchpad (working state that survives compaction)
		scratchpadDbPath := filepath.Join(filepath.Dir(configPath), "scratchpad.db")
		scratchpadStore, err = memory.NewScratchpad(scratchpadDbPath)
		if err != nil {
			log.Fatalf("main", "create scratchpad: %v", err)
		}
		defer scratchpadStore.Close()

		// Index conversation messages into FTS5 as they're logged
		log.ConversationHook = memIdx.IndexConversation
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
		inst := setupAgent(setupParams{
			acfg:                acfg,
			cfg:                 cfg,
			configPath:          configPath,
			client:              client,
			sessions:            sessions,
			store:               store,
			memIdx:              memIdx,
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
			Agent string `json:"agent"`
			Text  string `json:"text"`
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

		log.Infof("http", "send (agent=%s): %s", inst.id, req.Text)

		// Route slash commands through the command dispatcher
		if strings.HasPrefix(req.Text, "/") {
			if result, ok := inst.cmds.Dispatch(ctx, req.Text); ok {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"response": result})
				return
			}
		}

		resp, err := inst.ag.HandleMessage(ctx, inst.sessionKey, req.Text)
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
		result, _ := inst.cmds.Dispatch(context.Background(), "/status")
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

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Infof("main", "shutting down...")

	// Stop all heartbeats
	for _, id := range agentOrder {
		agents[id].heartbeat.Stop()
	}
	cancel()

	// Wait for in-flight agent turns to flush session state
	for i := 0; i < 50; i++ {
		anyBusy := false
		for _, inst := range agents {
			if inst.ag.IsProcessing() {
				anyBusy = true
				break
			}
		}
		if !anyBusy {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	httpMu.Lock()
	if httpServer != nil {
		httpServer.Close()
	}
	httpMu.Unlock()
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg                config.AgentConfig
	cfg                 *config.Config
	configPath          string
	client              *anthropic.Client
	sessions            *session.Store
	store               *secrets.Store
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

	// Per-agent tool registry
	registry := tools.NewRegistry()
	registry.Register(tools.NewExecTool(p.store))
	registry.Register(tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows))
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
	}

	// Per-agent workspace bootstrap
	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.SystemFiles)
	bootstrap.SetSecretNames(p.store.Names())

	// Per-agent skills
	skillRegistry := skills.Load(p.cfg.Skills.Dirs)
	var extraSystemBlocks []anthropic.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock()},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}

	// Per-agent compactor
	compactor := compaction.NewCompactor(p.client, p.sessions, acfg.Model, p.cfg.Sessions.CompactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionModel,
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		p.cfg.Sessions.CompactionSummaryPrompt,
		p.cfg.Sessions.CompactionHandoffMsg,
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
	ag := &agent.Agent{
		Client:            p.client,
		Sessions:          p.sessions,
		Tools:             registry,
		Bootstrap:         bootstrap,
		Compactor:         compactor,
		Reminders:         p.reminderStore,
		Model:             acfg.Model,
		ExtraSystemBlocks: extraSystemBlocks,
		CacheStrategy:     p.cfg.Cache.Strategy,
		CacheBustDetect:   p.cfg.Logging.CacheBustDetect,
		DuplicateMessages: acfg.DuplicateMessages,
		MaxResultChars:    p.cfg.Tools.MaxResultChars,
		ToolResultTempDir: p.cfg.Tools.TempDir,
	}

	// Warning injection queue (if enabled)
	if p.cfg.Logging.InjectAgentWarnings {
		ag.Warnings = agent.NewWarningQueue()
	}

	// Model escalation tool (needs this agent's bootstrap)
	registry.Register(tools.NewRequestModelTool(p.client, bootstrap))

	// TTS tool (needs this agent's voice reply func)
	if p.ttsProvider != nil {
		registry.Register(tools.NewTTSTool(p.ttsProvider, func() tools.VoiceReplyFunc {
			fn := ag.GetVoiceReplyFunc()
			if fn == nil {
				return nil
			}
			return tools.VoiceReplyFunc(fn)
		}))
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

	// /reload command
	cmds.Register(command.NewReloadCommand(func() (string, error) {
		bootstrap.Reload()
		newSkillRegistry := skills.Load(p.cfg.Skills.Dirs)
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock()},
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
	n, _ := sessions.MessageCount(key)
	return n
}

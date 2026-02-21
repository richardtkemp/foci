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
	"clod/telegram"
	"clod/skills"
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

	// Resolve credentials: secrets.toml overrides clod.toml
	anthropicToken := cfg.Anthropic.Token
	if v, ok := store.Get("anthropic.token"); ok {
		anthropicToken = v
	}
	anthropicOAuthToken := cfg.Anthropic.OAuthToken
	if v, ok := store.Get("anthropic.oauth_token"); ok {
		anthropicOAuthToken = v
	}
	telegramToken := cfg.Telegram.BotToken
	if v, ok := store.Get("telegram.bot_token"); ok {
		telegramToken = v
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

	// Anthropic client
	client := anthropic.NewClient(anthropicToken)

	// Session store
	sessions := session.NewStore(cfg.Sessions.Dir)

	// Forward-declare bot pointer for tools that need telegram access.
	// Populated later when the bot is created.
	var primaryBot *telegram.Bot

	// Tool registry
	registry := tools.NewRegistry()
	registry.Register(tools.NewExecTool(store))
	registry.Register(tools.NewTmuxTool())
	registry.Register(tools.NewReadTool())
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewEditTool())
	registry.Register(tools.NewWebFetchTool())
	if braveKey != "" {
		registry.Register(tools.NewWebSearchTool(braveKey))
	}
	registry.Register(tools.NewSendTelegramTool(func() tools.TelegramSender {
		if primaryBot == nil {
			return nil
		}
		return primaryBot
	}))
	// Memory FTS5 index
	var memIdx *memory.Index
	var reminderStore *memory.ReminderStore
	var scratchpadStore *memory.Scratchpad
	if cfg.Memory.Dir != "" {
		memDbPath := filepath.Join(filepath.Dir(configPath), "memory.db")
		memIdx, err = memory.NewIndex(memDbPath, cfg.Memory.Dir)
		if err != nil {
			log.Fatalf("main", "create memory index: %v", err)
		}
		defer memIdx.Close()

		if err := memIdx.Reindex(); err != nil {
			log.Errorf("main", "reindex memory: %v", err)
		}
		registry.Register(tools.NewMemorySearchTool(memIdx))

		// Reminder store (same DB directory)
		reminderDbPath := filepath.Join(filepath.Dir(configPath), "reminders.db")
		reminderStore, err = memory.NewReminderStore(reminderDbPath)
		if err != nil {
			log.Fatalf("main", "create reminder store: %v", err)
		}
		defer reminderStore.Close()
		registry.Register(tools.NewMemoryRemindTool(reminderStore))

		// Scratchpad (working state that survives compaction)
		scratchpadDbPath := filepath.Join(filepath.Dir(configPath), "scratchpad.db")
		scratchpadStore, err = memory.NewScratchpad(scratchpadDbPath)
		if err != nil {
			log.Fatalf("main", "create scratchpad: %v", err)
		}
		defer scratchpadStore.Close()
		registry.Register(tools.NewScratchpadWriteTool(scratchpadStore))
		registry.Register(tools.NewScratchpadReadTool(scratchpadStore))
		registry.Register(tools.NewScratchpadClearTool(scratchpadStore))

		// Index conversation messages into FTS5 as they're logged
		log.ConversationHook = memIdx.IndexConversation
	}

	// Workspace bootstrap
	bootstrap := workspace.NewBootstrap(cfg.Agent.Workspace, cfg.Agent.SystemFiles)

	// Inject available secret names so agent knows what {{secret:NAME}} references are available
	bootstrap.SetSecretNames(store.Names())

	// Skills
	skillRegistry := skills.Load(cfg.Skills.Dirs)
	var extraSystemBlocks []anthropic.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock()},
		}
		log.Infof("main", "loaded %d skills", skillRegistry.Len())
	}

	// Compactor
	compactor := compaction.NewCompactor(client, sessions, cfg.Agent.Model, cfg.Sessions.CompactionThreshold)
	compactor.WithConfig(
		cfg.Sessions.CompactionModel,
		cfg.Sessions.CompactionMaxTokens,
		cfg.Sessions.CompactionMinMessages,
		cfg.Sessions.CompactionSummaryPrompt,
		cfg.Sessions.CompactionHandoffMsg,
	)
	compactor.Scratchpad = scratchpadStore

	// Agent
	ag := &agent.Agent{
		Client:             client,
		Sessions:           sessions,
		Tools:              registry,
		Bootstrap:          bootstrap,
		Compactor:          compactor,
		Reminders:          reminderStore,
		Model:              cfg.Agent.Model,
		ExtraSystemBlocks:  extraSystemBlocks,
		CacheStrategy:      cfg.Cache.Strategy,
		CacheBustDetect:    cfg.Logging.CacheBustDetect,
		DuplicateMessages:  cfg.Agent.DuplicateMessages,
		MaxResultChars:     cfg.Tools.MaxResultChars,
		ToolResultTempDir:  cfg.Tools.TempDir,
	}

	// Model escalation tool (sync one-shot call to a different model)
	registry.Register(tools.NewRequestModelTool(client, bootstrap))

	// Voice: STT (speech-to-text) and TTS (text-to-speech)
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

	if ttsProvider != nil {
		// Register TTS tool — lets the agent send voice notes explicitly
		registry.Register(tools.NewTTSTool(ttsProvider, func() tools.VoiceReplyFunc {
			fn := ag.GetVoiceReplyFunc()
			if fn == nil {
				return nil
			}
			return tools.VoiceReplyFunc(fn)
		}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionKey := fmt.Sprintf("agent:%s:main", cfg.Agent.ID)
	startTime := time.Now()

	// Scheduled wakes (timers that inject messages into the session)
	var wakesMu sync.Mutex
	wakes := make(map[string]context.CancelFunc)

	wakeScheduleFn := func(delay time.Duration, message string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())

		go func() {
			select {
			case <-time.After(delay):
				log.Infof("schedule_wake", "firing wake after %v: %q", delay, message)
				resp, err := ag.HandleMessage(ctx, sessionKey, "[SCHEDULED WAKE]\n"+message)
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
	tools.SetScheduleWakeFn(wakeScheduleFn)
	registry.Register(tools.NewScheduleWakeTool())

	// Slash commands — bypass agent pipeline entirely
	// Create store for tracking last messages (used by // repeat command)
	lastMsgStore := command.NewLastMessageStore()

	cmds := command.NewRegistry()
	cmds.Register(command.NewPingCommand())
	cmds.Register(command.NewStatusCommand(func() command.StatusInfo {
		return command.StatusInfo{
			SessionKey:   sessionKey,
			MessageCount: sessionMessageCount(sessions, sessionKey),
			Model:        ag.Model,
			Uptime:       time.Since(startTime),
			AgentBusy:    ag.IsProcessing(),
		}
	}, cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(cfg.Logging.APIFile))
	cmds.Register(command.NewResetCommand(func() error {
		if ag.IsProcessing() {
			return fmt.Errorf("agent is processing — send /stop first, then /reset")
		}
		if err := sessions.Clear(sessionKey); err != nil {
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
			MessageCount: sessionMessageCount(sessions, sessionKey),
			CreatedAt:    sessions.CreatedAt(sessionKey),
			LastActivity: sessions.LastActivity(sessionKey),
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
			cfg.Agent.ID, ag.Model, cfg.Agent.Workspace,
			cfg.Sessions.Dir, cfg.Memory.Dir,
			cfg.HTTP.Bind, cfg.HTTP.Port,
			cfg.Logging.Level)
	}))
	cmds.Register(command.NewLogCommand(cfg.Logging.EventFile))
	cmds.Register(command.NewErrorsCommand(cfg.Logging.EventFile))
	cmds.Register(command.NewVersionCommand(command.BuildInfo{
		Version:   version,
		GoVersion: goVersion,
		GitCommit: gitCommit,
		BuildTime: buildTime,
	}))
	cmds.Register(command.NewUptimeCommand(startTime))
	cmds.Register(command.NewHelpCommand(cmds))

	// Register /usage command (check Claude subscription usage)
	usageClient := anthropic.NewUsageClient(anthropicOAuthToken)
	cmds.Register(command.NewUsageCommand(func(ctx context.Context) (string, error) {
		if anthropicOAuthToken == "" {
			return "OAuth token not configured (add anthropic.oauth_token to config or secrets.toml)", nil
		}
		usage, err := usageClient.GetUsage(ctx)
		if err != nil {
			return fmt.Sprintf("Error fetching usage: %v", err), nil
		}
		return anthropic.FormatUsage(usage), nil
	}))

	// Register /reload command (reload config, skills, system files)
	cmds.Register(command.NewReloadCommand(func() (string, error) {
		// Reload workspace (system files)
		bootstrap.Reload()

		// Reload skills
		newSkillRegistry := skills.Load(cfg.Skills.Dirs)

		// Update system blocks with new skills
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock()},
			}
		}
		ag.ExtraSystemBlocks = newExtraSystemBlocks

		msg := fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills", newSkillRegistry.Len())
		return msg, nil
	}))

	// Custom script commands from config
	for _, cc := range cfg.Commands {
		cmds.Register(command.NewScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Skill slash commands (command + script in frontmatter)
	for _, s := range skillRegistry.All() {
		if s.Command != "" && s.Script != "" {
			name := strings.TrimPrefix(s.Command, "/")
			cmds.Register(command.NewScriptCommand(name, s.Description, s.Script, 30))
		}
	}

	// Register /voice command (before bot start, needs sessionKey)
	cmds.Register(command.NewVoiceCommand(
		func() bool { return ag.VoiceMode(sessionKey) },
		func(on bool) { ag.SetVoiceMode(sessionKey, on) },
	))

	// Multiball: secondary bot pool (populated below if configured)
	var pool *telegram.Pool

	// Register /multiball (and /mb alias) — closures capture pool by reference
	forkFn := func() (string, error) {
		if pool == nil || pool.Size() == 0 {
			return "", fmt.Errorf("no secondary bots configured")
		}
		secBot, ok := pool.Acquire()
		if !ok {
			return "", fmt.Errorf("all secondary bots are busy")
		}

		branchID := fmt.Sprintf("mb-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:multiball:%s", cfg.Agent.ID, branchID)

		if err := sessions.CreateBranch(sessionKey, branchKey); err != nil {
			pool.Release(secBot)
			return "", fmt.Errorf("create branch: %w", err)
		}

		// Inject fork prompt so the agent knows it's on a branch
		if fp := cfg.Agent.ForkPrompt; fp != "" {
			sessions.AppendAll(branchKey, []anthropic.Message{
				{Role: "user", Content: anthropic.TextContent(fp)},
				{Role: "assistant", Content: anthropic.TextContent("Understood.")},
			})
		}

		secBot.SetSessionKey(branchKey)
		if primaryBot != nil {
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

	// Auto-expose all slash commands as tools (except those marked SkipToolExport)
	// This allows the agent to invoke commands programmatically
	// Detect and prevent naming collisions between commands and tools
	for _, cmd := range cmds.All() {
		if cmd.SkipToolExport {
			continue // skip commands that should not be exposed as tools
		}
		if existingTool := registry.Get(cmd.Name); existingTool != nil {
			log.Fatalf("main", "naming collision: command '%s' conflicts with existing tool '%s'", cmd.Name, cmd.Name)
		}
		registry.Register(tools.CreateCommandWrapperTool(cmd))
	}

	// Start Telegram bot
	if telegramToken != "" {
		var err error
		primaryBot, err = telegram.NewBot(telegramToken, cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, sessionKey)
		if err != nil {
			log.Fatalf("main", "create telegram bot: %v", err)
		}

		// Wire voice support
		if sttProvider != nil {
			primaryBot.SetTranscriber(sttProvider)
		}
		if ttsProvider != nil {
			primaryBot.SetTTS(ttsProvider)
		}

		// Wire cache bust alerts to Telegram notification
		if ag.CacheBustDetect {
			ag.CacheBustAlert = func(session string, prevRead, curRead int) {
				msg := fmt.Sprintf("⚠️ Cache bust: read dropped %d → %d on %s", prevRead, curRead, session)
				log.Warnf("agent", "%s", msg)
				primaryBot.SendNotification(msg)
			}
		}

		// Secondary bots for multiball
		secondaryTokens := cfg.Telegram.SecondaryBots
		if v, ok := store.Get("telegram.secondary_bots"); ok && v != "" {
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					secondaryTokens = append(secondaryTokens, t)
				}
			}
		}
		if len(secondaryTokens) > 0 {
			pool = telegram.NewPool()
			for _, token := range secondaryTokens {
				secBot, err := telegram.NewBot(token, cfg.Telegram.AllowedUsers, ag, cmds, lastMsgStore, "")
				if err != nil {
					log.Errorf("main", "create secondary bot: %v", err)
					continue
				}
				secBot.SetSecondary(pool)
				if sttProvider != nil {
					secBot.SetTranscriber(sttProvider)
				}
				if ttsProvider != nil {
					secBot.SetTTS(ttsProvider)
				}
				pool.Add(secBot)
				go secBot.Run(ctx)
			}
			log.Infof("main", "multiball: %d secondary bots ready", pool.Size())
		}

		go primaryBot.Run(ctx)
	}

	// Start heartbeat
	interval, err := time.ParseDuration(cfg.Agent.HeartbeatInterval)
	if err != nil {
		interval = 45 * time.Minute
	}
	hb := agent.NewHeartbeat(ag, sessionKey, interval)
	hb.Start(ctx)

	// HTTP server
	mux := http.NewServeMux()

	// POST /send — send message to main session, return response
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, "bad request: need {\"text\": \"...\"}", http.StatusBadRequest)
			return
		}

		log.Infof("http", "send: %s", req.Text)

		// Route slash commands through the command dispatcher
		if strings.HasPrefix(req.Text, "/") {
			if result, ok := cmds.Dispatch(ctx, req.Text); ok {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"response": result})
				return
			}
		}

		resp, err := ag.HandleMessage(ctx, sessionKey, req.Text)
		if err != nil {
			log.Errorf("http", "send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hb.Reset()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": resp})
	})

	// GET /status — return agent status
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, _ := cmds.Dispatch(context.Background(), "/status")
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
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
			http.Error(w, "bad request: need {\"command\": \"/ping\"}", http.StatusBadRequest)
			return
		}
		result, ok := cmds.Dispatch(context.Background(), req.Command)
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

		if req.Agent == "" {
			req.Agent = cfg.Agent.ID
		}
		if req.Text == "" {
			req.Text = "[WAKE]"
		}

		// Create a branch session for this wake call
		parentKey := fmt.Sprintf("agent:%s:main", req.Agent)
		branchID := fmt.Sprintf("wake-%d", time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", req.Agent, branchID)

		if err := sessions.CreateBranch(parentKey, branchKey); err != nil {
			log.Errorf("wake", "branch error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Infof("wake", "branch %s from %s, text=%q", branchKey, parentKey, req.Text)

		resp, err := ag.HandleMessage(ctx, branchKey, req.Text)
		if err != nil {
			log.Errorf("wake", "error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		hb.Reset()

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

	log.Infof("main", "started (agent=%s, model=%s)", cfg.Agent.ID, cfg.Agent.Model)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Infof("main", "shutting down...")
	hb.Stop()
	cancel()

	// Wait for in-flight agent turns to flush session state
	for i := 0; i < 50 && ag.IsProcessing(); i++ {
		time.Sleep(100 * time.Millisecond)
	}

	httpMu.Lock()
	if httpServer != nil {
		httpServer.Close()
	}
	httpMu.Unlock()
}

func sessionMessageCount(sessions *session.Store, key string) int {
	n, _ := sessions.MessageCount(key)
	return n
}

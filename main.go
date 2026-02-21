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

	// Tool registry
	registry := tools.NewRegistry()
	registry.Register(tools.NewExecTool(store))
	registry.Register(tools.NewReadTool())
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewEditTool())
	registry.Register(tools.NewWebFetchTool())
	if braveKey != "" {
		registry.Register(tools.NewWebSearchTool(braveKey))
	}
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
	bootstrap := workspace.NewBootstrap(cfg.Agent.Workspace, nil)

	// Compactor
	compactor := compaction.NewCompactor(client, sessions, cfg.Agent.Model, cfg.Sessions.CompactionThreshold)
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
		CacheBustThreshold: cfg.Logging.CacheBustThreshold,
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

	// Slash commands — bypass agent pipeline entirely
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
		return sessions.Clear(sessionKey)
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

	// Custom script commands from config
	for _, cc := range cfg.Commands {
		cmds.Register(command.NewScriptCommand(cc.Name, cc.Description, cc.Script, cc.Timeout))
	}

	// Register /voice command (before bot start, needs sessionKey)
	cmds.Register(command.NewVoiceCommand(
		func() bool { return ag.VoiceMode(sessionKey) },
		func(on bool) { ag.SetVoiceMode(sessionKey, on) },
	))

	// Start Telegram bot
	if telegramToken != "" {
		bot, err := telegram.NewBot(telegramToken, cfg.Telegram.AllowedUsers, ag, cmds, sessionKey)
		if err != nil {
			log.Fatalf("main", "create telegram bot: %v", err)
		}

		// Wire voice support
		if sttProvider != nil {
			bot.SetTranscriber(sttProvider)
		}
		if ttsProvider != nil {
			bot.SetTTS(ttsProvider)
		}

		// Wire cache bust alerts to Telegram notification
		if ag.CacheBustThreshold > 0 {
			ag.CacheBustAlert = func(session string, tokens int, cost float64) {
				msg := fmt.Sprintf("⚠️ Cache write: %d tokens ($%.2f) on %s", tokens, cost, session)
				log.Warnf("agent", "%s", msg)
				bot.SendNotification(msg)
			}
		}

		go bot.Run(ctx)
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

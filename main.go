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
		Level:     cfg.Logging.Level,
		EventFile: cfg.Logging.EventFile,
		APIFile:   cfg.Logging.APIFile,
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
		Client:    client,
		Sessions:  sessions,
		Tools:     registry,
		Bootstrap: bootstrap,
		Compactor: compactor,
		Reminders: reminderStore,
		Model:     cfg.Agent.Model,
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

	// Start Telegram bot
	if telegramToken != "" {
		bot, err := telegram.NewBot(telegramToken, cfg.Telegram.AllowedUsers, ag, cmds, sessionKey)
		if err != nil {
			log.Fatalf("main", "create telegram bot: %v", err)
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
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Infof("http", "listening on %s", addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Errorf("http", "server error: %v", err)
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
	server.Close()
}

func sessionMessageCount(sessions *session.Store, key string) int {
	n, _ := sessions.MessageCount(key)
	return n
}

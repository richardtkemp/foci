package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"clod/agent"
	"clod/anthropic"
	"clod/config"
	"clod/log"
	"clod/secrets"
	"clod/session"
	"clod/telegram"
	"clod/tools"
	"clod/workspace"
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
	if cfg.Memory.Dir != "" {
		registry.Register(tools.NewMemorySearchTool(cfg.Memory.Dir))
	}

	// Workspace bootstrap
	bootstrap := workspace.NewBootstrap(cfg.Agent.Workspace, nil)

	// Agent
	ag := &agent.Agent{
		Client:    client,
		Sessions:  sessions,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     cfg.Agent.Model,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionKey := fmt.Sprintf("agent:%s:main", cfg.Agent.ID)

	// Start Telegram bot
	if telegramToken != "" {
		bot, err := telegram.NewBot(telegramToken, cfg.Telegram.AllowedUsers, ag, sessionKey)
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

	// HTTP server for wake endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Agent string `json:"agent"`
			Text  string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if req.Agent == "" {
			req.Agent = cfg.Agent.ID
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

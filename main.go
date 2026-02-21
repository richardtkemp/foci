package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clod/agent"
	"clod/anthropic"
	"clod/config"
	"clod/session"
	"clod/telegram"
	"clod/tools"
	"clod/workspace"
)

func main() {
	configPath := config.ParseFlags()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Anthropic client
	client := anthropic.NewClient(cfg.Anthropic.Token)

	// Session store
	sessions := session.NewStore(cfg.Sessions.Dir)

	// Tool registry
	registry := tools.NewRegistry()
	registry.Register(tools.NewExecTool())
	registry.Register(tools.NewReadTool())
	registry.Register(tools.NewWriteTool())
	registry.Register(tools.NewEditTool())
	registry.Register(tools.NewWebFetchTool())
	if cfg.Anthropic.BraveAPIKey != "" {
		registry.Register(tools.NewWebSearchTool(cfg.Anthropic.BraveAPIKey))
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
	if cfg.Telegram.BotToken != "" {
		bot, err := telegram.NewBot(cfg.Telegram.BotToken, cfg.Telegram.AllowedUsers, ag, sessionKey)
		if err != nil {
			log.Fatalf("create telegram bot: %v", err)
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
			log.Printf("[wake] branch error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		resp, err := ag.HandleMessage(ctx, branchKey, req.Text)
		if err != nil {
			log.Printf("[wake] error: %v", err)
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
		log.Printf("[http] listening on %s", addr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[http] server error: %v", err)
		}
	}()

	log.Printf("[clod] started (agent=%s, model=%s)", cfg.Agent.ID, cfg.Agent.Model)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("[clod] shutting down...")
	hb.Stop()
	cancel()
	server.Close()
}

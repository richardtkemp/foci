package main

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/agent"
	"foci/anthropic"
	"foci/command"
	"foci/compaction"
	"foci/config"
	"foci/gemini"
	"foci/keepalive"
	"foci/log"
	"foci/mana"
	mcpkg "foci/mcp"
	"foci/memory"
	"foci/prompts"
	"foci/provider"
	"foci/resources"
	"foci/secrets"
	"foci/secrets/bitwarden"
	"foci/session"
	"foci/skills"
	"foci/startup"
	"foci/state"
	"foci/telegram"
	"foci/tools"
	"foci/voice"
	"foci/warnings"
	"foci/workspace"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

// tokenHolder is a thread-safe, swappable credential string.
// Used with NewClientWithTokenFunc so that credentials can be hot-reloaded
// (e.g. after `foci auth` saves a new setup-token) without restarting.
type tokenHolder struct {
	mu    sync.RWMutex
	token string
}

func (h *tokenHolder) Get() (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.token == "" {
		return "", fmt.Errorf("no credential configured")
	}
	return h.token, nil
}

func (h *tokenHolder) Set(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
}

// agentInstance holds all per-agent state.
type agentInstance struct {
	id                string
	ag                *agent.Agent
	cmds              *command.Registry
	registry          *tools.Registry
	bootstrap         *workspace.Bootstrap
	defaultSessionKey func() string // resolves current default session key
	agentCfg          config.AgentConfig
	promptSearchDirs  []string           // directories to search for prompt files
	tmuxClearAll      func()             // clears tmux tool state (watches, owned sessions)
	kaRunner          *keepalive.Runner  // keepalive & background work timer (nil if disabled)
	mcpManager        *mcpkg.Manager     // nil if no MCP servers configured
}

// applyAgentDisplaySettings sets per-agent display settings on a bot,
// falling back to global config when the agent field is nil/empty.
// Used for primary bots, per-agent multiball bots, and shared pool bots
// acquired or restored for a specific agent.
func applyAgentDisplaySettings(bot *telegram.Bot, acfg config.AgentConfig, cfg *config.Config) {
	switch {
	case acfg.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*acfg.ShowToolCalls))
	case cfg.Defaults.ShowToolCalls != nil:
		bot.SetShowToolCalls(string(*cfg.Defaults.ShowToolCalls))
	}
	switch {
	case acfg.ShowThinking != nil:
		bot.SetShowThinking(string(*acfg.ShowThinking))
	case cfg.Defaults.ShowThinking != nil:
		bot.SetShowThinking(string(*cfg.Defaults.ShowThinking))
	}
	switch {
	case acfg.DisplayWidth != nil:
		bot.SetDisplayWidth(*acfg.DisplayWidth)
	case cfg.Defaults.DisplayWidth != nil:
		bot.SetDisplayWidth(*cfg.Defaults.DisplayWidth)
	}
	if acfg.MessagesInLog != nil {
		bot.SetMessagesInLog(*acfg.MessagesInLog)
	} else {
		bot.SetMessagesInLog(cfg.Logging.MessagesInLog)
	}
	if acfg.ReceivedFilesDir != "" {
		bot.SetReceivedFilesDir(acfg.ReceivedFilesDir)
	} else if cfg.Telegram.ReceivedFilesDir != "" {
		bot.SetReceivedFilesDir(cfg.Telegram.ReceivedFilesDir)
	}
}

// checkActivityGate parses if_active/if_inactive durations, checks them against
// isActive, and writes a skip JSON response if the gate blocks the request.
// Returns true if the request should continue, false if it was skipped or errored.
func checkActivityGate(w http.ResponseWriter, agentID, ifActive, ifInactive string,
	isActive func(string, time.Duration) bool, logTag, endpoint string) bool {
	if ifActive != "" {
		dur, err := time.ParseDuration(ifActive)
		if err != nil {
			http.Error(w, fmt.Sprintf("bad if_active duration: %v", err), http.StatusBadRequest)
			return false
		}
		if !isActive(agentID, dur) {
			log.Debugf(logTag, "POST %s: skipping (no user activity within %s for agent %s)", endpoint, ifActive, agentID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"response": "skipped: no recent user activity"})
			return false
		}
	}
	if ifInactive != "" {
		dur, err := time.ParseDuration(ifInactive)
		if err != nil {
			http.Error(w, fmt.Sprintf("bad if_inactive duration: %v", err), http.StatusBadRequest)
			return false
		}
		if isActive(agentID, dur) {
			log.Debugf(logTag, "POST %s: skipping (user active within %s for agent %s)", endpoint, ifInactive, agentID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"response": "skipped: session recently active"})
			return false
		}
	}
	return true
}

// multiballBotConfig holds common settings applied to every multiball bot.
type multiballBotConfig struct {
	sttProvider     voice.STT
	ttsProvider     voice.TTS
	stopAliases     []string
	enableStopAlias bool
	acfg            config.AgentConfig
	cfg             *config.Config
	toolDetailStore *telegram.ToolDetailStore
	stateStore      *state.Store
}

// configureMultiballBot applies the standard multiball bot settings shared by
// both per-agent and shared-pool multiball bots.
func configureMultiballBot(bot *telegram.Bot, mc multiballBotConfig) {
	if mc.sttProvider != nil {
		bot.SetTranscriber(mc.sttProvider)
	}
	if mc.ttsProvider != nil {
		bot.SetTTS(mc.ttsProvider)
	}
	bot.SetStopAliases(mc.stopAliases, mc.enableStopAlias)
	applyAgentDisplaySettings(bot, mc.acfg, mc.cfg)
	if mc.toolDetailStore != nil {
		bot.SetToolDetailStore(mc.toolDetailStore)
	}
	if mc.stateStore != nil {
		ss := mc.stateStore
		bot.OnSessionKeyChange = func(username, sessionKey string) {
			key := "multiball:" + username
			if sessionKey == "" {
				_ = ss.Delete(key)
			} else {
				_ = ss.Set(key, sessionKey)
			}
		}
	}
}

// authMiddleware returns an HTTP middleware that requires a valid API key on
// all endpoints except /voice (which has its own auth via voice.api_key).
// Checks Authorization: Bearer header first, then falls back to api_key query
// param (for WebSocket compat). Uses constant-time comparison.
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /voice has its own auth via voice.api_key
		if r.URL.Path == "/voice" {
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization: Bearer header
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = auth[len("Bearer "):]
		}

		// Fallback: api_key query param (WebSocket compat)
		if token == "" {
			token = r.URL.Query().Get("api_key")
		}

		if token == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			http.Error(w, "invalid credentials", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// httpHandlerDeps holds shared state needed by HTTP endpoint handlers.
type httpHandlerDeps struct {
	agents             map[string]*agentInstance
	agentOrder         []string
	stateStore         *state.Store
	sessions           *session.Store
	botMgr             *telegram.BotManager
	cfg                *config.Config
	ctx                context.Context
	voiceAPIKey        string
	sttProvider        voice.STT
	ttsProvider        voice.TTS
	reloadCredentials  func() error // hot-reload credentials from secrets.toml (nil if not supported)
}

// registerHTTPHandlers registers all HTTP endpoints (/send, /status, /command, /wake, /voice).
func registerHTTPHandlers(mux *http.ServeMux, d httpHandlerDeps) {
	// resolveAgent returns the agent instance for the given ID, or the first agent if empty.
	resolveAgent := func(agentID string) (*agentInstance, bool) {
		if agentID == "" && len(d.agentOrder) > 0 {
			return d.agents[d.agentOrder[0]], true
		}
		inst, ok := d.agents[agentID]
		return inst, ok
	}

	// isAgentActive checks whether a real user has interacted with the agent
	// within the given duration. Used by --if-active gating on CLI commands.
	isAgentActive := func(agentID string, within time.Duration) bool {
		if d.stateStore == nil {
			return true // no state store = assume active
		}
		var ts int64
		if !d.stateStore.Get("agent:"+agentID+":last_user_activity", &ts) {
			return false // no activity recorded = not active
		}
		return time.Since(time.Unix(ts, 0)) <= within
	}

	// POST /send — send message to agent session, return response
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent      string `json:"agent"`
			Session    string `json:"session"`
			Text       string `json:"text"`
			IfActive   string `json:"if_active"`   // Go duration — skip if no user activity within this window
			IfInactive string `json:"if_inactive"` // Go duration — skip if user was active within this window
			Async      bool   `json:"async"`       // fire-and-forget: return 202 immediately, deliver response via Telegram
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

		if !checkActivityGate(w, inst.id, req.IfActive, req.IfInactive, isAgentActive, "http", "/send") {
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
			if result, ok := inst.cmds.Dispatch(d.ctx, req.Text); ok {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{"response": result}); err != nil {
					log.Errorf("http", "encode response: %v", err)
				}
				return
			}
		}

		sendCtx := agent.WithTrigger(d.ctx, "user")
		if req.Async {
			asyncDispatch(w, inst, sendCtx, sessionKey, req.Text, "http", d.botMgr, false)
			return
		}

		resp, err := inst.ag.HandleMessage(sendCtx, sessionKey, req.Text)
		if err != nil {
			log.Errorf("http", "send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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
			IfActive    string `json:"if_active"`   // Go duration — skip if no user activity within this window
			IfInactive  string `json:"if_inactive"` // Go duration — skip if user was active within this window
			Async       bool   `json:"async"`       // fire-and-forget: return 202 immediately, deliver response via Telegram
			Silent      bool   `json:"silent"`      // suppress Telegram delivery of branch response (oneshot cron branches)
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

		if !checkActivityGate(w, inst.id, req.IfActive, req.IfInactive, isAgentActive, "wake", "/wake") {
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

		orientPath := resolveOrientPath(inst.agentCfg.BranchOrientationHeadlessPrompt, d.cfg.Sessions.BranchOrientationHeadlessPrompt, inst.agentCfg.BranchOrientationPrompt, d.cfg.Sessions.BranchOrientationPrompt)
		orientText := buildBranchOrientation(orientPath, branchKey, parentKey, "cron", false, inst.promptSearchDirs)
		branchErr := d.sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        req.NoResetHook,
			OrientationMessage: orientText,
		})
		if branchErr != nil {
			log.Errorf("wake", "branch error: %v", branchErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Infof("wake", "branch %s from %s, text=%q no_compact=%v no_reset_hook=%v async=%v silent=%v", branchKey, parentKey, req.Text, req.NoCompact, req.NoResetHook, req.Async, req.Silent)

		wakeCtx := agent.WithTrigger(d.ctx, "wake")
		if req.NoCompact {
			wakeCtx = agent.WithNoCompact(wakeCtx)
		}

		if req.Async {
			asyncDispatch(w, inst, wakeCtx, branchKey, req.Text, "wake", d.botMgr, req.Silent)
			return
		}

		resp, err := inst.ag.HandleMessage(wakeCtx, branchKey, req.Text)
		if err != nil {
			log.Errorf("wake", "error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": resp}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	})

	// WebSocket voice endpoint
	endpointList := "/send, /status, /command, /wake"
	if d.cfg.Voice.WSEnabled && d.voiceAPIKey != "" && d.sttProvider != nil {
		voiceCfg := voice.HandlerConfig{
			APIKey: d.voiceAPIKey,
			ListAgents: func() []voice.AgentInfo {
				var infos []voice.AgentInfo
				for _, id := range d.agentOrder {
					inst := d.agents[id]
					infos = append(infos, voice.AgentInfo{
						ID:    id,
						Name:  inst.agentCfg.Name,
						Emoji: inst.agentCfg.Emoji,
					})
				}
				return infos
			},
			HandleMessage: func(msgCtx context.Context, agentID, sessionKey, text string) (string, error) {
				inst, ok := d.agents[agentID]
				if !ok {
					return "", fmt.Errorf("unknown agent: %q", agentID)
				}
				return inst.ag.HandleMessage(agent.WithTrigger(msgCtx, "voice"), sessionKey, text)
			},
			SessionExists: func(key string) bool {
				msgs, err := d.sessions.Load(key)
				return err == nil && msgs != nil
			},
			STT: d.sttProvider,
			AgentTTS: func(agentID string) voice.TTS {
				if d.ttsProvider == nil {
					return nil
				}
				inst, ok := d.agents[agentID]
				if !ok {
					return d.ttsProvider
				}
				return voice.WithRate(d.ttsProvider, inst.agentCfg.TTSRate)
			},
		}
		mux.HandleFunc("/voice", voice.Handler(voiceCfg))
		endpointList += ", /voice (ws)"
		log.Infof("http", "/voice WebSocket endpoint enabled")
	}

	// POST /-/reload-credentials — hot-reload API credentials from secrets.toml
	if d.reloadCredentials != nil {
		mux.HandleFunc("/-/reload-credentials", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := d.reloadCredentials(); err != nil {
				log.Errorf("http", "POST /-/reload-credentials: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "credentials reloaded"})
		})
		endpointList += ", /-/reload-credentials"
	}

	log.Infof("http", "registered endpoints: %s", endpointList)
}

// asyncDispatch handles async fire-and-forget requests: sends the agent message
// in a goroutine, writes a 202 response, and optionally delivers the result via Telegram.
func asyncDispatch(w http.ResponseWriter, inst *agentInstance, ctx context.Context,
	sessionKey, text, logTag string, botMgr *telegram.BotManager, silent bool) {
	go func() {
		resp, err := inst.ag.HandleMessage(ctx, sessionKey, text)
		if err != nil {
			log.Errorf(logTag, "async error: %v", err)
			return
		}
		if resp != "" && !silent {
			if bot := botMgr.BotForSessionOrPrimary(sessionKey, inst.id); bot != nil {
				if err := bot.SendText(resp); err != nil {
					log.Errorf(logTag, "async telegram delivery: %v", err)
				}
			}
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
}

func main() {
	// Handle "foci auth" subcommand before normal flag parsing
	if len(os.Args) >= 2 && os.Args[1] == "auth" {
		authFlags := flag.NewFlagSet("auth", flag.ExitOnError)
		configFile := authFlags.String("config", "", "path to foci.toml (to find secrets.toml)")
		_ = authFlags.Parse(os.Args[2:])

		cfgPath := *configFile
		if cfgPath == "" {
			cfgPath = config.ParseFlags()
		}
		secretsPath := filepath.Join(filepath.Dir(cfgPath), "secrets.toml")
		authStore, err := secrets.Load(secretsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load secrets: %v\n", err)
			os.Exit(1)
		}
		if err := anthropic.RunSetupTokenFlow(authStore); err != nil {
			fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Setup token saved to %s\n", secretsPath)
		os.Exit(0)
	}

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

	// Log rotation
	if cfg.Logging.LogRotation {
		rotPeriod, _ := time.ParseDuration(cfg.Logging.RotationPeriod)
		retPeriod, _ := time.ParseDuration(cfg.Logging.RetentionPeriod)
		maxLineSize, _ := config.ParseByteSize(cfg.Logging.RotationMaxLineSize)
		archiveDir := cfg.Logging.ArchiveDir
		if archiveDir == "" {
			archiveDir = filepath.Join(filepath.Dir(cfg.Logging.EventFile), "archive")
		}
		var files []string
		for _, p := range []string{cfg.Logging.EventFile, cfg.Logging.APIFile, cfg.Logging.PayloadFile} {
			if p != "" {
				files = append(files, p)
			}
		}
		stopRotation := log.StartRotation(log.RotationConfig{
			Period:      rotPeriod,
			Retention:   retPeriod,
			MaxLineSize: maxLineSize,
			ArchiveDir:  archiveDir,
			Files:       files,
		})
		defer stopRotation()
	}

	// API call log (SQLite)
	if cfg.Logging.APIDB != "" {
		if err := log.InitAPIDB(cfg.Logging.APIDB); err != nil {
			log.Fatalf("main", "init API db: %v", err)
		}
		defer log.CloseAPIDB()
	}

	// Conversation log (SQLite)
	if cfg.Logging.ConversationFile != "" {
		if err := log.InitConversation(cfg.Logging.ConversationFile); err != nil {
			log.Fatalf("main", "init conversation log: %v", err)
		}
		defer log.CloseConversation()
	}

	// Seed default prompts to ~/shared/prompts/ for user customisation
	if home, err := os.UserHomeDir(); err == nil {
		seedDefaultPrompts(filepath.Join(home, "shared", "prompts"))
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
	if len(cfg.Agents) > 1 && !store.HasAgentRestrictions() {
		log.Warnf("security", "multiple agents but no allowed_agents/denied_agents in secrets.toml — all agents can access all secrets")
	}

	// Auto-generate HTTP API key if not present
	httpAPIKey, _ := store.Get("http.api_key")
	if httpAPIKey == "" {
		generated, err := secrets.GeneratePassphrase(5)
		if err != nil {
			log.Fatalf("main", "generate HTTP API key: %v", err)
		}
		store.Set("http.api_key", generated)
		if err := store.Save(); err != nil {
			log.Fatalf("main", "save HTTP API key: %v", err)
		}
		httpAPIKey = generated
		log.Infof("main", "generated HTTP API key — add FOCI_API_KEY to crontab: %s", httpAPIKey)
	}

	// Wire child process group-dropping into the command package
	// (so script commands also drop supplementary groups).
	command.ChildSysProcAttr = tools.ChildSysProcAttr

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	anthropicClient, usageClient, credHolder := resolveCredentials(cfg, store, ctx)
	log.Debugf("main", "anthropic client ready")

	// Gemini client (created lazily only if any agent uses it)
	var geminiClient provider.Client
	geminiClientOnce := sync.OnceFunc(func() {
		apiKey, _ := store.Get("gemini.api_key")
		if apiKey == "" {
			log.Errorf("main", "gemini.api_key not found in secrets — gemini provider unavailable")
			return
		}
		httpTimeout, err := time.ParseDuration(cfg.Gemini.HTTPTimeout)
		if err != nil {
			httpTimeout = 120 * time.Second
		}
		gc, err := gemini.NewClient(ctx, apiKey, gemini.WithHTTPTimeout(httpTimeout))
		if err != nil {
			log.Errorf("main", "create gemini client: %v", err)
			return
		}
		geminiClient = gc
		log.Infof("main", "gemini client ready")
	})

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

	// Shared: Session index (SQLite-backed metadata index of all session files)
	sessionIndexPath := cfg.DataPath("session_index.db")
	sessionIndex, err := session.NewSessionIndex(sessionIndexPath)
	if err != nil {
		log.Errorf("main", "create session index: %v (session index disabled)", err)
	} else {
		defer func() { _ = sessionIndex.Close() }()
		// Rebuild index from disk on startup
		if n, err := sessionIndex.Rebuild(sessions); err != nil {
			log.Warnf("main", "rebuild session index: %v", err)
		} else {
			log.Infof("main", "session index: %d sessions indexed", n)
		}
		// Wire lifecycle events from session store to index
		sessions.OnSessionEvent(func(e session.SessionEvent) {
			switch e.Status {
			case session.SessionStatusActive:
				sessionIndex.Upsert(session.SessionIndexEntry{
					SessionKey:       e.Key,
					FilePath:         e.FilePath,
					CreatedAt:        e.CreatedAt,
					ParentSessionKey: e.ParentKey,
					SessionType:      e.Type,
					Status:           session.SessionStatusActive,
				})
			case session.SessionStatusCompacted:
				// Session was compacted (Replace): the current file is still active,
				// and the archive file is a new compacted entry.
				if e.ArchivePath != "" {
					// Derive archive key from file path
					rel, err := filepath.Rel(cfg.Sessions.Dir, e.ArchivePath)
					if err == nil {
						archiveKey := strings.TrimSuffix(rel, ".jsonl")
						archiveKey = strings.ReplaceAll(archiveKey, string(filepath.Separator), ":")
						sessionIndex.Upsert(session.SessionIndexEntry{
							SessionKey:       archiveKey,
							FilePath:         e.ArchivePath,
							CreatedAt:        time.Now().UTC(),
							ParentSessionKey: e.Key,
							SessionType:      e.Type,
							Status:           session.SessionStatusCompacted,
						})
					}
				}
				// Keep the current session as active (don't change its status)
			case session.SessionStatusCleared:
				sessionIndex.SetStatus(e.Key, session.SessionStatusCleared)
			}
		})

		// Start archive sweep goroutine
		archiveAfter, err := time.ParseDuration(cfg.Sessions.ArchiveAfter)
		if err != nil {
			log.Warnf("main", "invalid sessions.archive_after %q: %v (archive sweep disabled)", cfg.Sessions.ArchiveAfter, err)
		} else {
			archiveStop := make(chan struct{})
			archiveTicker := time.NewTicker(6 * time.Hour)
			go func() {
				// Run immediately on startup
				if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
					log.Warnf("main", "archive sweep: %v", err)
				} else if n > 0 {
					log.Infof("main", "archive sweep: archived %d idle session(s)", n)
				}
				for {
					select {
					case <-archiveTicker.C:
						if n, err := session.ArchiveSweep(sessions, sessionIndex, archiveAfter); err != nil {
							log.Warnf("main", "archive sweep: %v", err)
						} else if n > 0 {
							log.Infof("main", "archive sweep: archived %d idle session(s)", n)
						}
					case <-archiveStop:
						return
					}
				}
			}()
			defer func() {
				archiveTicker.Stop()
				close(archiveStop)
			}()
		}
	}

	// Shared: State persistence (JSON file in data dir)
	statePath := cfg.DataPath("state.json")
	stateStore := state.New(statePath)
	if err := stateStore.Load(); err != nil {
		log.Errorf("main", "load state: %v", err)
	}

	// ========== Memory system ==========
	mem := initMemorySystem(cfg)
	defer mem.cleanup()

	// Shared: Tool call detail persistence (for show_tool_calls=full inline keyboard expansion)
	toolDetailDbPath := cfg.DataPath("tool_details.db")
	toolDetailStore, err := telegram.NewToolDetailStore(toolDetailDbPath)
	if err != nil {
		log.Errorf("main", "create tool detail store: %v (inline keyboard expansion will not persist)", err)
	} else {
		// Expire old entries and vacuum on startup (cleanup from previous run)
		toolDetailStore.ExpireAndVacuum()
		defer func() {
			toolDetailStore.ExpireAndVacuum()
			_ = toolDetailStore.Close()
		}()
	}

	// Shared: Voice providers
	groqKey, _ := store.Get("groq.api_key")
	openrouterKey, _ := store.Get("openrouter.api_key")
	braveKey, _ := store.Get("brave.api_key")
	voiceAPIKey, _ := store.Get("voice.api_key")
	sttProvider, ttsProvider := initVoice(cfg, groqKey, openrouterKey)

	startTime := time.Now()

	// Bot manager — owns all Telegram bots
	botMgr := telegram.NewBotManager()

	// ========== Per-agent setup ==========
	agents := make(map[string]*agentInstance, len(cfg.Agents))
	var agentOrder []string // preserve config order

	// Closure for resolving agent instances by ID — resolved at call time.
	agentResolverFn := func(agentID string) *agentInstance {
		return agents[agentID]
	}

	// Closure for /agents command — captures agents/agentOrder, resolved at call time.
	agentListFn := func() []command.AgentInfo {
		var infos []command.AgentInfo
		for _, id := range agentOrder {
			inst := agents[id]
			sk := inst.defaultSessionKey()
			var mc int
			var lastAct string
			if sk != "" {
				mc, _ = inst.ag.Sessions.MessageCount(sk)
				lastAct = inst.ag.Sessions.LastActivity(sk)
			}
			infos = append(infos, command.AgentInfo{
				ID:           id,
				SessionKey:   sk,
				Model:        inst.ag.Model,
				Busy:         inst.ag.IsProcessing(),
				MessageCount: mc,
				LastActivity: lastAct,
			})
		}
		return infos
	}

	for _, acfg := range cfg.Agents {
		// Resolve memory backends: per-agent (if configured) or shared
		var agentBackends map[string]memory.Searcher
		if ab, ok := mem.agentBackends[acfg.ID]; ok {
			agentBackends = ab
		} else {
			agentBackends = mem.sharedBackends
		}

		// Resolve per-agent provider client
		var agentClient provider.Client
		switch acfg.Provider {
		case "gemini":
			geminiClientOnce()
			if geminiClient == nil {
				log.Errorf("main", "agent %q: gemini provider requested but client unavailable", acfg.ID)
				continue
			}
			agentClient = geminiClient
		default:
			agentClient = anthropicClient
		}

		inst := setupAgent(setupParams{
			acfg:            acfg,
			cfg:             cfg,
			configPath:      configPath,
			client:          agentClient,
			sessions:        sessions,
			store:           store,
			bwStore:         bwStore,
			stateStore:      stateStore,
			memBackends:     agentBackends,
			reminderStore:   mem.reminderStore,
			scratchpadStore: mem.scratchpadStore,
			todoStore:       mem.todoStore,
			toolDetailStore: toolDetailStore,
			sessionIndex:    sessionIndex,
			sttProvider:     sttProvider,
			ttsProvider:     ttsProvider,
			braveKey:        braveKey,
			usageClient:     usageClient,
			botMgr:          botMgr,
			startTime:       startTime,
			ctx:             ctx,
			agentListFn:     agentListFn,
			agentResolverFn: agentResolverFn,
		})
		agents[acfg.ID] = inst
		agentOrder = append(agentOrder, acfg.ID)

		// Keepalive & background work runner (per-agent config, falls back to global)
		if acfg.Keepalive.Enabled || acfg.Background.Enabled || hasMemoryFormation(acfg.MemoryFormation) || acfg.InjectAgentWarnings {
			kaOrientPrompt := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt)
			branchFn := buildBranchFunc(
				acfg.ID, inst.ag, sessions, inst.defaultSessionKey,
				func(branchKey, parentKey, branchType string) string {
					return buildBranchOrientation(kaOrientPrompt, branchKey, parentKey, branchType, false, inst.promptSearchDirs)
				},
				ctx,
			)

			// Mana monitor for background work throttling
			manaStaleness, err := time.ParseDuration(acfg.Background.ManaStalenessTimeout)
			if err != nil || manaStaleness <= 0 {
				manaStaleness = 10 * time.Minute
			}
			manaMonitor := mana.NewMonitor(usageClient, manaStaleness)

			// Proactive warning dispatcher
			var warningDispatcher *warnings.Dispatcher
			if acfg.InjectAgentWarnings {
				warningActiveInterval, _ := time.ParseDuration(cfg.Logging.WarningProactiveActiveInterval)
				warningInactiveInterval, _ := time.ParseDuration(cfg.Logging.WarningProactiveInactiveInterval)
				warningActivityThreshold, _ := time.ParseDuration(cfg.Logging.WarningProactiveActivityThreshold)

				agentID := acfg.ID
				agentInst := inst
				warningDispatcher = warnings.NewDispatcher(warnings.DispatcherConfig{
					Queue: inst.ag.Warnings,
					FormatFn: func(body string) string {
						return prompts.FormatInjectedMessage("PROACTIVE WARNINGS", time.Now(), body)
					},
					DispatchFn: func(warningText string) {
						sk := agentInst.defaultSessionKey()
						if sk == "" {
							log.Warnf("keepalive", "no default session for proactive warning dispatch on agent %q", agentID)
							return
						}
						resp, err := agentInst.ag.HandleMessage(agent.WithTrigger(ctx, "proactive_warning"), sk, warningText)
						if err != nil {
							log.Errorf("keepalive", "proactive warning turn error: %v", err)
							return
						}
						if resp == "" {
							return
						}
						if bot := botMgr.BotForSessionOrPrimary(sk, agentID); bot != nil {
							if err := bot.SendText(resp); err != nil {
								log.Errorf("keepalive", "proactive warning telegram delivery: %v", err)
							}
						}
					},
					ActiveInterval:    warningActiveInterval,
					InactiveInterval:  warningInactiveInterval,
					ActivityThreshold: warningActivityThreshold,
					LastUserMessageTimeFn: func() time.Time {
						sk := agentInst.defaultSessionKey()
						if sk == "" {
							return time.Time{}
						}
						return agentInst.ag.LastUserMessageTime(sk)
					},
				})
			}

			inst.kaRunner = keepalive.New(keepalive.RunnerConfig{
				AgentID:           acfg.ID,
				Keepalive:         acfg.Keepalive,
				Background:        acfg.Background,
				MemoryFormation:   acfg.MemoryFormation,
				PromptSearchDirs:  inst.promptSearchDirs,
				TodoStore:         mem.todoStore,
				StateStore:        stateStore,
				BranchFunc:        branchFn,
				ManaMonitor:       manaMonitor,
				WarningDispatcher: warningDispatcher,
			})
			inst.kaRunner.Start(ctx)

			// Wire Telegram bot callbacks to keepalive runner
			if bot := botMgr.PrimaryBot(acfg.ID); bot != nil {
				runner := inst.kaRunner
				bot.OnUserMessage = func() {
					runner.NotifyInteraction()
				}
				bot.OnTurnComplete = func() {
					runner.NotifyCacheWarmed()
				}
			}

			log.Infof("main", "agent %q keepalive runner started (ka=%v bg=%v)", acfg.ID, acfg.Keepalive.Enabled, acfg.Background.Enabled)
		}

		log.Infof("main", "agent %q ready (model=%s, workspace=%s)", acfg.ID, acfg.Model, acfg.Workspace)
	}

	// Shared multiball pool — fallback bots available to any agent.
	// Created after all agents so we can use the first agent's instance for initial binding.
	// Bots are re-wired to the acquiring agent at fork time via SetAgentAndCommands.
	if len(cfg.Telegram.MultiballBots) > 0 && len(agentOrder) > 0 {
		firstInst := agents[agentOrder[0]]
		for _, botName := range cfg.Telegram.MultiballBots {
			mbToken := config.ResolveBotToken(botName, "", store)
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
			configureMultiballBot(mbBot, multiballBotConfig{
				sttProvider:     sttProvider,
				ttsProvider:     ttsProvider,
				stopAliases:     cfg.Telegram.StopAliases,
				enableStopAlias: cfg.Telegram.EnableStopAliases,
				acfg:            firstInst.agentCfg,
				cfg:             cfg,
				toolDetailStore: toolDetailStore,
				stateStore:      stateStore,
			})
			botMgr.AddSharedMultiball(mbBot)
		}
		if pool := botMgr.SharedPool(); pool != nil && pool.Size() > 0 {
			ttl, _ := time.ParseDuration(cfg.Telegram.MultiballSessionTTL)
			if ttl > 0 {
				pool.SetSessionTTL(ttl, sessions)
			}
			pool.ReclaimHook = func(sessionKey string) {
				// Determine agent from session key for session-end memory
				for _, id := range agentOrder {
					inst := agents[id]
					prefix := "agent:" + id + ":"
					if strings.HasPrefix(sessionKey, prefix) {
						orientPath := resolveOrientPath(inst.agentCfg.BranchOrientationHeadlessPrompt, cfg.Sessions.BranchOrientationHeadlessPrompt, inst.agentCfg.BranchOrientationPrompt, cfg.Sessions.BranchOrientationPrompt)
						fireSessionEndMemory(inst.ag, sessions, sessionKey, inst.agentCfg.MemoryFormation, func(bk, pk, bt string) string {
							return buildBranchOrientation(orientPath, bk, pk, bt, false, inst.promptSearchDirs)
						}, inst.promptSearchDirs, ctx)
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
			log.SetWarnHook(func(level log.Level, component string, msg string) {
				for _, inst := range agents {
					if inst.ag.Warnings != nil {
						inst.ag.Warnings.Push(level.String(), component, msg)
					}
				}
			})
			log.Infof("main", "warning injection into agent sessions enabled")
		}
	}

	// Tmux memory monitor — checks RSS of tmux server, notifies/kills at thresholds
	hasTmux := false
	if _, err := exec.LookPath("tmux"); err == nil {
		hasTmux = true
	}
	if hasTmux && cfg.Tools.TmuxMemoryCheckInterval != "0" {
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

	// System memory guard — monitors total RSS for foci user, warns/kills under pressure
	if cfg.Resources.MemoryGuardEnabled {
		guardInterval, _ := time.ParseDuration(cfg.Resources.MemoryGuardInterval)
		if guardInterval > 0 {
			memGuard := resources.NewMemoryGuard(
				resources.MemoryGuardConfig{
					Interval:          guardInterval,
					WarnPercent:       cfg.Resources.MemoryWarnPercent,
					KillPercent:       cfg.Resources.MemoryKillPercent,
					PressureThreshold: cfg.Resources.MemoryPressureThreshold,
				},
				// Warning callback: push to all agents with warning injection enabled
				func(msg string) {
					for _, inst := range agents {
						if inst.ag.Warnings != nil {
							inst.ag.Warnings.Push("WARN", "memory_guard", msg)
						}
					}
				},
			)
			memGuard.Start(ctx)
			defer memGuard.Stop()
		}
	}

	// Periodic tool detail cleanup — expire old entries when all users are idle
	if toolDetailStore != nil {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Only run cleanup when all users are idle (>10min since last message)
					allIdle := true
					for _, id := range agentOrder {
						inst := agents[id]
						sk := inst.defaultSessionKey()
						if sk == "" {
							continue
						}
						if t := inst.ag.LastUserMessageTime(sk); !t.IsZero() && time.Since(t) < 10*time.Minute {
							allIdle = false
							break
						}
					}
					if allIdle {
						toolDetailStore.ExpireAndVacuum()
					}
				}
			}
		}()
	}

	// Intercept SIGINT/SIGTERM before starting bots.
	// Must be registered before any goroutine that could trigger a signal
	// (e.g. /restart via Telegram), otherwise Go's default handler
	// terminates the process with no graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Restore multiball sessions from persisted state.
	// For each secondary bot, check if a saved session key exists and the session
	// file is still active. If so, restore the session key and re-wire the agent.
	if stateStore != nil {
		restoreMultiballSessions(botMgr, stateStore, sessions, agents, agentOrder, cfg)
	}

	// Start all bots
	botMgr.StartAll(ctx)

	// Diagnose restart type (clean/crash/reboot) and gather diagnostics
	var diagnosis *startup.DiagnosisResult
	logsDir := filepath.Dir(cfg.Logging.EventFile)
	if logsDir == "" || logsDir == "." {
		if home, err := os.UserHomeDir(); err == nil {
			logsDir = filepath.Join(home, "logs")
		}
	}
	diagnosis = startup.DiagnoseRestart(stateStore, startTime, logsDir)
	if diagnosis.Class != startup.ClassClean && diagnosis.Class != startup.ClassUnknown {
		log.Infof("startup", "restart classified as %s: %s", diagnosis.Class, diagnosis.Summary)
	}

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
				bot.SendStartupNotificationWithDiagnosis(id, diagnosis)
			}
		}
	}

	// ========== HTTP server ==========
	// Build credential reload function (only for static token auth, not OAuth fallback).
	var reloadCreds func() error
	if credHolder != nil {
		reloadCreds = func() error {
			st, err := secrets.Load(secretsPath)
			if err != nil {
				return fmt.Errorf("reload secrets.toml: %w", err)
			}
			token, _ := st.Get("anthropic.setup_token")
			if token == "" {
				token, _ = st.Get("anthropic.api_key")
			}
			if token == "" {
				return fmt.Errorf("no setup_token or api_key found in secrets.toml after reload")
			}
			credHolder.Set(token)
			log.Infof("main", "credentials hot-reloaded from secrets.toml")
			return nil
		}
	}

	mux := http.NewServeMux()
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:            agents,
		agentOrder:        agentOrder,
		stateStore:        stateStore,
		sessions:          sessions,
		botMgr:            botMgr,
		cfg:               cfg,
		ctx:               ctx,
		voiceAPIKey:       voiceAPIKey,
		sttProvider:       sttProvider,
		ttsProvider:       ttsProvider,
		reloadCredentials: reloadCreds,
	})

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Bind, cfg.HTTP.Port)
	var httpServer *http.Server
	var httpMu sync.Mutex
	go func() {
		for ctx.Err() == nil {
			srv := &http.Server{Addr: addr, Handler: authMiddleware(httpAPIKey, mux)}
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
			msg := prompts.FormatInjectedMessage("SYSTEM UPDATE", time.Now(), content)
			if _, err := inst.ag.HandleMessage(restartCtx, sk, msg); err != nil {
				log.Errorf("main", "restart turn failed: %v", err)
			}
		}()
	}

	// Check for first-run onboarding — inject prompt for new agents
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(stateStore, inst.agentCfg); msg != "" {
			agentID := agentID // capture for goroutine
			go func() {
				sk := inst.defaultSessionKey()
				if sk == "" {
					// On first run, no Telegram message has arrived yet so
					// there's no default session. Construct one from the
					// first allowed user ID in config.
					users := inst.agentCfg.AllowedUsers
					if len(users) == 0 {
						users = cfg.Telegram.AllowedUsers
					}
					if len(users) > 0 {
						if chatID, err := strconv.ParseInt(users[0], 10, 64); err == nil {
							sk = telegram.SessionKeyForChat(agentID, chatID)
						}
					}
				}
				if sk == "" {
					log.Warnf("main", "no default session for first-run injection on %s, skipping", agentID)
					return
				}
				firstRunCtx := agent.WithTrigger(ctx, "first_run")
				firstRunCtx = agent.WithNoCompact(firstRunCtx)
				if _, err := inst.ag.HandleMessage(firstRunCtx, sk, msg); err != nil {
					log.Errorf("main", "first-run turn for %s failed: %v", agentID, err)
					return
				}
				// Mark first-run as completed after one successful turn
				if err := stateStore.Set("agent:"+agentID+":first_run_completed", true); err != nil {
					log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
				}
				log.Infof("main", "first-run onboarding completed for %s", agentID)
			}()
		}
	}

	// Wait for signal
	<-sigCh

	// Record clean shutdown immediately — before graceful shutdown wait.
	// If a second SIGTERM arrives during graceful shutdown (e.g. systemd
	// TimeoutStopSec), Go's default handler kills the process. Recording
	// now ensures the next startup sees this as a clean shutdown.
	if err := startup.RecordCleanShutdown(stateStore); err != nil {
		log.Warnf("main", "record clean shutdown: %v", err)
	}

	log.Infof("main", "shutting down...")

	// Stop keepalive runners — prevents new timer-triggered branches
	for _, inst := range agents {
		if inst.kaRunner != nil {
			inst.kaRunner.Stop()
		}
	}

	// Close HTTP server — prevents new HTTP-triggered turns
	httpMu.Lock()
	if httpServer != nil {
		_ = httpServer.Close()
	}
	httpMu.Unlock()

	// Wait for in-flight agent turns to complete naturally.
	// The turn lock prevents new turns from starting on sessions that are
	// already processing. With HTTP closed, no new
	// turns will be initiated. We defer cancel() until AFTER this loop so
	// that in-flight turns (including exec subprocesses) aren't killed.
	shutdownTimeout, _ := time.ParseDuration(cfg.HTTP.GracefulShutdownTimeout)
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}
	gracefulShutdown(agents, shutdownTimeout)

	// Close MCP managers — disconnect from MCP servers
	for _, inst := range agents {
		if inst.mcpManager != nil {
			_ = inst.mcpManager.Close()
		}
	}

	// Now cancel the context — stops Telegram bots and cleans up goroutines
	cancel()

	// Wait for Telegram bots to finish cleanup (ack processed updates)
	botMgr.Wait()
}

// setupParams holds the shared resources needed by each agent.
type setupParams struct {
	acfg            config.AgentConfig
	cfg             *config.Config
	configPath      string
	client          provider.Client
	sessions        *session.Store
	store           *secrets.Store
	bwStore         *bitwarden.Store
	stateStore      *state.Store
	memBackends     map[string]memory.Searcher
	reminderStore   *memory.ReminderStore
	scratchpadStore *memory.Scratchpad
	todoStore       *memory.TodoStore
	toolDetailStore *telegram.ToolDetailStore
	sessionIndex    *session.SessionIndex
	sttProvider     voice.STT
	ttsProvider     voice.TTS
	braveKey        string
	usageClient     *anthropic.UsageClient
	botMgr          *telegram.BotManager
	startTime       time.Time
	ctx             context.Context
	agentListFn     func() []command.AgentInfo
	agentResolverFn func(agentID string) *agentInstance
}

// setupAgent wires up a single agent with its own tools, commands, bootstrap, and bot.
func setupAgent(p setupParams) *agentInstance {
	acfg := p.acfg

	// Prompt search directories: agent workspace first, then shared.
	// Used by ResolvePrompt when no explicit path is configured.
	promptSearchDirs := []string{
		filepath.Join(acfg.Workspace, "prompts"),
		filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts"),
	}

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

	// sessionKeyFromCtx resolves the session key from a command/tool context.
	// Priority: (1) tools.SessionKeyFromContext (set by agent tool execution),
	// (2) command.ChatIDKey (set by Telegram command dispatch),
	// (3) defaultSessionKey fallback.
	sessionKeyFromCtx := func(ctx context.Context) string {
		if sk := tools.SessionKeyFromContext(ctx); sk != "" {
			return sk
		}
		if chatID, ok := ctx.Value(command.ChatIDKey{}).(int64); ok && chatID != 0 {
			return telegram.SessionKeyForChat(acfg.ID, chatID)
		}
		return defaultSessionKey()
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

			// Resolve bot early so intermediate replies (ReplyFunc) can be delivered
			// during the turn, not just at the end.
			bot := p.botMgr.BotForSessionOrPrimary(target, acfg.ID)

			ctx := agent.WithTrigger(p.ctx, "async_notify")
			if bot != nil {
				ctx = agent.WithTurnCallbacks(ctx, &agent.TurnCallbacks{
					ReplyFunc: func(text string) {
						if err := bot.SendText(text); err != nil {
							log.Errorf("async_notify", "intermediate telegram delivery: %v", err)
						}
					},
				})
			}

			resp, err := ag.HandleMessage(ctx, target, message)
			if err != nil {
				log.Errorf("async_notify", "error: %v", err)
				return
			}
			log.Debugf("async_notify", "response length: %d", len(resp))
			if resp == "" {
				return
			}
			if bot == nil {
				log.Warnf("async_notify", "no bot for agent %s session %s, response not delivered", acfg.ID, target)
				return
			}
			if err := bot.SendText(resp); err != nil {
				log.Errorf("async_notify", "telegram delivery: %v", err)
			}
		}()
	})
	// Per-agent secrets view: agent-specific values overlay globals
	agentStore := p.store.ForAgent(acfg.ID)

	execAutoBg := resolveInt(acfg.ExecAutoBackground, p.cfg.Tools.ExecAutoBackground)
	maxUploadSize := resolveInt64(acfg.MaxUploadFileSize, p.cfg.Tools.MaxUploadFileSize)
	registry.Register(tools.NewExecTool(agentStore, p.bwStore, execAutoBg, notifier, acfg.Workspace, registry))

	// Only register tmux tool if tmux is available in PATH
	var tmuxTool *tools.Tool
	var tmuxClearAll func()
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxAutopilot := resolveBoolPtr(acfg.TmuxAutopilot, p.cfg.Tools.TmuxAutopilot)
		tmuxWatchThreshold := resolveString(acfg.TmuxWatchThreshold, p.cfg.Tools.TmuxWatchThreshold)
		tmuxWatchThresholdSec := 30
		if d, err := time.ParseDuration(tmuxWatchThreshold); err == nil {
			tmuxWatchThresholdSec = int(d.Seconds())
		}
		tmuxTool, tmuxClearAll = tools.NewTmuxTool(p.cfg.Tools.TmuxCols, p.cfg.Tools.TmuxRows, notifier, p.stateStore, "tmux:"+acfg.ID, tmuxAutopilot, tmuxWatchThresholdSec)
		registry.Register(tmuxTool)
	}
	blockedPaths := resolveBlockedPaths(acfg, p.cfg)
	if len(blockedPaths) > 0 {
		log.Infof("setup", "agent %s: %d blocked write/edit path(s) configured", acfg.ID, len(blockedPaths))
	}
	registry.Register(tools.NewReadTool(agentStore))
	registry.Register(tools.NewWriteTool(agentStore, blockedPaths))
	registry.Register(tools.NewEditTool(agentStore, blockedPaths))
	registry.Register(tools.NewSummaryTool(p.client, acfg.Model, p.cfg.Models.Aliases))
	registry.Register(tools.NewHTTPRequestTool(agentStore, p.bwStore, p.cfg.Tools.TempDir, execAutoBg, maxUploadSize, notifier))

	// Web search/fetch: server-side (Anthropic) or client-side (Brave/builtin) based on config.
	var serverTools []anthropic.ToolDef

	searchProvider := resolveString(acfg.SearchProvider, p.cfg.Tools.SearchProvider)
	if searchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_search_20250305", "web_search",
			p.cfg.Tools.WebSearchMaxUses, p.cfg.Tools.WebSearchAllowedDomains, p.cfg.Tools.WebSearchBlockedDomains))
	} else if searchProvider == "brave" && p.braveKey != "" {
		registry.Register(tools.NewWebSearchTool(p.braveKey))
	}

	fetchProvider := resolveString(acfg.FetchProvider, p.cfg.Tools.FetchProvider)
	if fetchProvider == "anthropic" {
		serverTools = append(serverTools, buildServerTool("web_fetch_20250910", "web_fetch",
			p.cfg.Tools.WebFetchMaxUses, p.cfg.Tools.WebFetchAllowedDomains, p.cfg.Tools.WebFetchBlockedDomains))
	} else {
		registry.Register(tools.NewWebFetchTool())
	}

	// Memory tools (shared stores, registered per-agent)
	if len(p.memBackends) > 0 {
		registry.Register(tools.NewMemorySearchTool(p.memBackends, p.cfg.Memory.SearchBackends))
	}
	if p.scratchpadStore != nil {
		registry.Register(tools.NewScratchpadTool(p.scratchpadStore, acfg.ID))
	}
	if p.todoStore != nil {
		registry.Register(tools.NewTodoTool(p.todoStore, acfg.ID))
	}

	// Bitwarden tools (if enabled)
	if p.bwStore != nil {
		registry.Register(tools.NewBitwardenSearchTool(p.bwStore))
		registry.Register(tools.NewBitwardenUnlockTool(p.bwStore))
	}

	// MCP servers (dynamic — re-reads mcp.toml on each tool call)
	mcpMgr := mcpkg.NewManagerForAgent(filepath.Dir(p.configPath), acfg.ID)
	if tool := mcpMgr.Tool(); tool != nil {
		registry.Register(tool)
	}

	// Per-agent workspace bootstrap
	bootstrap := workspace.NewBootstrap(acfg.Workspace, acfg.SystemFiles)
	bootstrap.SetSecretNames(agentStore.Names(), p.bwStore != nil)
	checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)

	// Per-agent skills (per-agent dirs override global)
	skillsDirs := p.cfg.Skills.Dirs
	if len(acfg.SkillsDirs) > 0 {
		skillsDirs = acfg.SkillsDirs
	}
	skillRegistry := skills.Load(skillsDirs)
	var extraSystemBlocks []anthropic.SystemBlock
	if skillRegistry.Len() > 0 {
		extraSystemBlocks = []anthropic.SystemBlock{
			{Type: "text", Text: skillRegistry.SystemBlock(acfg.Workspace)},
		}
		log.Infof("main", "agent %q: loaded %d skills", acfg.ID, skillRegistry.Len())
	}

	compactionThreshold := resolveFloat64Ptr(acfg.CompactionThreshold, p.cfg.Sessions.CompactionThreshold)
	preserveMessages := resolveIntPtr(acfg.CompactionPreserveMessages, p.cfg.Sessions.CompactionPreserveMessages)
	compactor := compaction.NewCompactor(p.client, p.sessions, acfg.Model, compactionThreshold)
	compactor.WithConfig(
		p.cfg.Sessions.CompactionMaxTokens,
		p.cfg.Sessions.CompactionMinMessages,
		preserveMessages,
	)
	if acfg.CompactionEffort != "" {
		compactor.WithEffort(acfg.CompactionEffort)
	}
	compactor.Scratchpad = p.scratchpadStore
	compactor.AgentID = acfg.ID

	// Per-agent send_telegram tool (closure captures this agent's bot)
	registry.Register(tools.NewSendTelegramTool(func(sessionKey string) tools.TelegramSender {
		bot := p.botMgr.BotForSessionOrPrimary(sessionKey, acfg.ID)
		if bot == nil {
			return nil
		}
		return bot
	}, p.ttsProvider))

	// send_to_session tool — inject messages into other sessions.
	// sessionNotifyFn handles reply_to="session": routes the target agent's
	// response to the target session's own Telegram chat.
	sessionNotifyFn := tools.SessionNotifyFn(func(targetSessionKey, message string) {
		go func() {
			// Parse agent ID from session key (agent:<id>:...)
			parts := strings.Split(targetSessionKey, ":")
			if len(parts) < 2 {
				log.Errorf("session_notify", "invalid session key: %s", targetSessionKey)
				return
			}
			targetAgentID := parts[1]

			inst := p.agentResolverFn(targetAgentID)
			if inst == nil {
				log.Errorf("session_notify", "unknown agent %q for session %s", targetAgentID, targetSessionKey)
				return
			}

			resp, err := inst.ag.HandleMessage(agent.WithTrigger(p.ctx, "session_notify"), targetSessionKey, message)
			if err != nil {
				log.Errorf("session_notify", "error: %v", err)
				return
			}
			if resp == "" {
				return
			}

			bot := p.botMgr.BotForSessionOrPrimary(targetSessionKey, targetAgentID)
			if bot == nil {
				log.Warnf("session_notify", "no bot for agent %s session %s, response not delivered", targetAgentID, targetSessionKey)
				return
			}

			// Extract chat ID from session key for targeted delivery.
			// Supports both "agent:X:chat:CHATID" and legacy "agent:X:CHATID".
			// Falls back to bot's default chat if no chat ID found.
			chatID := tools.ChatIDFromSessionKey(targetSessionKey)
			if chatID != 0 {
				if err := bot.SendTextToChat(chatID, resp); err != nil {
					log.Errorf("session_notify", "telegram delivery to chat %d: %v", chatID, err)
				}
			} else {
				if err := bot.SendText(resp); err != nil {
					log.Errorf("session_notify", "telegram delivery: %v", err)
				}
			}
		}()
	})
	registry.Register(tools.NewSendToSessionTool(p.sessions, notifier, sessionNotifyFn))

	// Per-agent environment block
	var envBlock string
	if p.cfg.Environment.Enabled {
		crontabCount := countCrontabJobs()
		envBlock = buildEnvironmentBlock(acfg, p.configPath, p.cfg, crontabCount)
	}

	// Per-agent agent struct
	ag = &agent.Agent{
		Log:                         log.NewComponentLogger("agent:" + acfg.ID),
		Client:                      p.client,
		Sessions:                    p.sessions,
		Tools:                       registry,
		ServerTools:                 serverTools,
		EnvironmentBlock:            envBlock,
		Bootstrap:                   bootstrap,
		Compactor:                   compactor,
		AsyncNotifier:               notifier,
		Reminders:                   p.reminderStore,
		DefaultSessionKey:           defaultSessionKey,
		AgentID:                     acfg.ID,
		Model:                       acfg.Model,
		ExtraSystemBlocks:           extraSystemBlocks,
		CacheStrategy:               p.cfg.Cache.Strategy,
		CacheBustDetect:             p.cfg.Logging.CacheBustDetect,
		CacheBustIdleThreshold:      time.Duration(p.cfg.Logging.CacheBustIdleMinutes) * time.Minute,
		DuplicateMessages:              acfg.DuplicateMessages,
		BatchPartialAssistantMessages:  acfg.BatchPartialAssistantMessages,
		BatchPartialJoiner:             acfg.BatchPartialJoiner,
		MaxResultChars:              resolveInt(acfg.MaxResultChars, p.cfg.Tools.MaxResultChars),
		ToolResultTempDir:           p.cfg.Tools.TempDir,
		ModelAliases:                p.cfg.Models.Aliases,
		SummaryContextTurns:         resolveInt(acfg.SummaryContextTurns, p.cfg.Tools.SummaryContextTurns),
		SummaryContextChars:         resolveInt(acfg.SummaryContextChars, p.cfg.Tools.SummaryContextChars),
		MaxSummaryChars:             resolveInt(acfg.MaxSummaryChars, p.cfg.Tools.MaxSummaryChars),
		AutoSummarise:               resolveBoolPtr(acfg.AutoSummarise, p.cfg.Tools.AutoSummarise),
		StateStore:                  p.stateStore,
		UsageClient:                 p.usageClient,
		MessageTransforms:           agent.CompileTransforms(resolveMessageTransforms(acfg, p.cfg)),
		CompactionSummaryPromptPath: resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
		CompactionHandoffMsg:        resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
		PromptSearchDirs:            promptSearchDirs,
		MaxToolLoops:                acfg.MaxToolLoops,
		MaxOutputTokens:             acfg.MaxOutputTokens,
		BraindeadWarningThreshold:   acfg.BraindeadThreshold,
		BraindeadWarningPrompt:      acfg.BraindeadPrompt,
		TurnLockWarnThreshold:       parseDurationDefault(acfg.TurnLockWarnThreshold, 3*time.Minute),
		Effort:                      acfg.Effort,
		Thinking:                    acfg.Thinking,
		ManaInvestInterval: func() time.Duration {
			d, err := time.ParseDuration(acfg.Background.InvestInterval)
			if err != nil {
				return 30 * time.Minute
			}
			return d
		}(),
	}
	if p.store != nil && p.bwStore != nil {
		ag.Redact = func(text string) string {
			text = agentStore.Redact(text)
			return p.bwStore.Redact(text)
		}
	} else if p.store != nil {
		ag.Redact = agentStore.Redact
	} else if p.bwStore != nil {
		ag.Redact = p.bwStore.Redact
	}
	// Restore per-session state and seed session meta for default session (if any).
	// These are no-ops if no default session exists yet (first startup).
	if sk := defaultSessionKey(); sk != "" {
		ag.RestoreVoiceMode(sk)
		ag.RestoreSessionOverrides(sk)
		ag.SeedSessionMeta(sk)
	}

	// Warning injection queue (if enabled per-agent)
	if acfg.InjectAgentWarnings {
		warningWindow, err := time.ParseDuration(p.cfg.Logging.WarningWindowDuration)
		if err != nil {
			warningWindow = 5 * time.Minute
		}
		ag.Warnings = warnings.NewQueue(p.cfg.Logging.WarningMaxPerWindow, warningWindow)
	}

	// Mana threshold warnings (per-agent thresholds override global)
	manaThresholds := p.cfg.ManaWarnings.Thresholds
	if len(acfg.UsageWarnings.Thresholds) > 0 {
		manaThresholds = acfg.UsageWarnings.Thresholds
	}
	if len(manaThresholds) > 0 {
		ag.ManaWatcher = agent.NewManaWatcher(p.cfg.ManaWarnings.Name, manaThresholds)
		ag.ManaWatcher.SetStore(p.stateStore)
		ag.ManaWatcher.Restore()
		// Mana restore notification: per-agent overrides global
		restoreThreshold := p.cfg.ManaWarnings.RestoreThreshold
		if acfg.UsageWarnings.RestoreThreshold != nil {
			restoreThreshold = *acfg.UsageWarnings.RestoreThreshold
		}
		ag.ManaWatcher.SetRestoreThreshold(restoreThreshold)
	}

	// Spawn tool — replaces request_model, adds inherit (self-fork) mode.
	// Uses lazy getter for agent since ag is assigned later in this function.
	spawnOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
	spawnDeps := tools.SpawnDeps{
		Client:          p.client,
		Bootstrap:       bootstrap,
		Registry:        registry,
		Sessions:        &sessionBranchAdapter{store: p.sessions},
		AgentID:         acfg.ID,
		Model:           acfg.Model,
		ModelAliases:    p.cfg.Models.Aliases,
		MaxInherit:      resolveInt(acfg.MaxConcurrentSpawns, p.cfg.Tools.MaxConcurrentSpawns),
		MaxToolLoops:    acfg.MaxToolLoops,
		ExploreMaxDepth: resolveInt(acfg.ExploreMaxDepth, p.cfg.Tools.ExploreMaxDepth),
		Notifier:        notifier,
		OrientationBuilder: func(branchKey, parentKey string) string {
			return buildBranchOrientation(spawnOrientPath, branchKey, parentKey, "spawn", false, promptSearchDirs)
		},
	}
	registry.Register(tools.NewSpawnTool(spawnDeps, func() tools.SpawnAgent { return ag }))

	// Per-agent scheduled wakes
	var wakesMu sync.Mutex
	wakes := make(map[int64]context.CancelFunc)
	wakeScheduleFn := func(id int64, delay time.Duration, message string) error {
		wakeCtx, wakeCancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-time.After(delay):
				log.Infof("remind", "firing wake id=%d after %v for agent %s: %q", id, delay, acfg.ID, message)
				if p.reminderStore != nil {
					_ = p.reminderStore.Dismiss(id)
				}
				sk := defaultSessionKey()
				if sk == "" {
					log.Warnf("remind", "no default session for agent %s, skipping", acfg.ID)
					return
				}
				resp, err := ag.HandleMessage(agent.WithTrigger(p.ctx, "scheduled_wake"), sk, prompts.FormatInjectedMessage("SCHEDULED WAKE", time.Now(), message))
				if err != nil {
					log.Errorf("remind", "error: %v", err)
				} else {
					log.Debugf("remind", "response: %s", resp)
				}
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			case <-wakeCtx.Done():
				if p.reminderStore != nil {
					_ = p.reminderStore.Dismiss(id)
				}
				wakesMu.Lock()
				delete(wakes, id)
				wakesMu.Unlock()
			}
		}()
		wakesMu.Lock()
		wakes[id] = wakeCancel
		wakesMu.Unlock()
		return nil
	}
	if p.reminderStore != nil {
		registry.Register(tools.NewRemindTool(p.reminderStore, acfg.ID, wakeScheduleFn))

		// Restore pending wakes from DB (survives restart)
		if pending, err := p.reminderStore.PendingWakes(acfg.ID); err != nil {
			log.Errorf("remind", "failed to load pending wakes for %s: %v", acfg.ID, err)
		} else if len(pending) > 0 {
			for _, r := range pending {
				delay := time.Until(r.DueAt)
				if delay < 0 {
					delay = 0
				}
				_ = wakeScheduleFn(r.ID, delay, r.Text)
			}
			log.Infof("remind", "restored %d pending wake(s) for agent %s", len(pending), acfg.ID)
		}
	}

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
			CompactThreshold: compactionThreshold,
		}
	}, p.cfg.Logging.APIFile))
	cmds.Register(command.NewCacheCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewLastCommand(p.cfg.Logging.APIFile))
	cmds.Register(command.NewCostCommand(p.cfg.Logging.APIFile))
	if tmuxTool != nil {
		cmds.Register(command.NewTmuxCommand(func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tmuxTool.Execute(ctx, params)
		}))
	}
	cmds.Register(command.NewContextCommand(p.cfg.Logging.APIFile, buildContextInfoFn(
		ag, bootstrap, registry, acfg, p.client, p.sessions, defaultSessionKey, compactionThreshold,
	)))
	cmds.Register(command.NewResetCommand(func() error {
		if ag.IsProcessing() {
			return fmt.Errorf("agent is processing — send /stop first, then /reset")
		}
		sk := defaultSessionKey()
		if sk == "" {
			return fmt.Errorf("no active session to reset")
		}
		resetOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		fireSessionEndMemory(ag, p.sessions, sk, acfg.MemoryFormation, func(bk, pk, bt string) string {
			return buildBranchOrientation(resetOrientPath, bk, pk, bt, false, promptSearchDirs)
		}, promptSearchDirs, p.ctx)
		if err := p.sessions.Clear(sk); err != nil {
			return err
		}
		bootstrap.Reload()
		return nil
	}))

	// Model resolution using config aliases
	var resolveModelFn func(string) string
	if len(p.cfg.Models.Aliases) > 0 {
		aliases := p.cfg.Models.Aliases
		resolveModelFn = func(input string) string {
			key := strings.ToLower(strings.TrimSpace(input))
			if resolved, ok := aliases[key]; ok {
				return resolved
			}
			if input == "" {
				if resolved, ok := aliases["sonnet"]; ok {
					return resolved
				}
			}
			return input
		}
	} else {
		resolveModelFn = func(input string) string { return input }
	}

	cmds.Register(command.NewModelCommand(
		func(ctx context.Context) string { return ag.SessionModel(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, m string) { ag.SetSessionModel(sessionKeyFromCtx(ctx), m) },
		resolveModelFn,
		p.cfg.Models.Aliases,
	))

	cmds.Register(command.NewEffortCommand(
		func(ctx context.Context) string { return ag.SessionEffort(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, e string) { ag.SetSessionEffort(sessionKeyFromCtx(ctx), e) },
	))
	cmds.Register(command.NewThinkingCommand(
		func(ctx context.Context) string { return ag.SessionThinking(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, t string) { ag.SetSessionThinking(sessionKeyFromCtx(ctx), t) },
	))
	cmds.Register(command.NewToolsCommand(func() []command.ToolInfo {
		var infos []command.ToolInfo
		for _, t := range registry.All() {
			infos = append(infos, command.ToolInfo{Name: t.Name, Description: t.Description})
		}
		return infos
	}))
	cmds.Register(command.NewConfigCommand(func(ctx context.Context, args string) (string, error) {
		dw, _ := ctx.Value(command.DisplayWidthKey{}).(int)
		switch strings.TrimSpace(strings.ToLower(args)) {
		case "toml":
			return config.FormatConfigTOML(p.cfg, acfg), nil
		case "table":
			return strings.Join(config.FormatConfigGrouped(p.cfg, acfg, dw), "\x00"), nil
		case "available":
			return "```\n" + config.FormatAvailable(p.cfg, acfg, dw) + "\n```", nil
		default:
			return "/config toml — raw TOML of running config (secrets redacted)\n/config table — formatted table of current config values\n/config available — unset options with defaults", nil
		}
	}))
	cmds.Register(command.NewPromptsCommand(command.PromptsCmdDeps{
		DataFn: func() command.PromptsData {
			dirs := promptSearchDirs

			// All file-based prompts
			allPrompts := []command.PromptInfo{
				resolvePromptInfo("compaction_summary",
					resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt),
					"compaction-summary.md", prompts.CompactionSummary(), dirs),
				resolvePromptInfo("branch_orient_multiball",
					resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt),
					"branch-orientation-multiball.md", prompts.BranchOrientationMultiball(), dirs),
				resolvePromptInfo("branch_orient_headless",
					resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt),
					"branch-orientation-headless.md", prompts.BranchOrientationHeadless(), dirs),
				resolvePromptInfo("keepalive",
					acfg.Keepalive.Prompt,
					"keepalive.md", prompts.Keepalive(), dirs),
				resolvePromptInfo("background",
					acfg.Background.Prompt,
					"background.md", prompts.Background(), dirs),
				resolvePromptInfo("memory_formation",
					acfg.MemoryFormation.IntervalPrompt,
					"memory-formation.md", prompts.MemoryFormation(), dirs),
				resolvePromptInfo("memory_consolidation",
					acfg.MemoryFormation.ConsolidationPrompt,
					"memory-consolidation.md", prompts.MemoryConsolidation(), dirs),
				resolvePromptInfo("memory_session_end",
					acfg.MemoryFormation.SessionEndPrompt,
					"memory-formation.md", prompts.MemoryFormation(), dirs),
			}

			// Inline prompts (not file-based)
			allPrompts = append(allPrompts,
				inlinePromptInfo("compaction_handoff",
					resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg),
					prompts.CompactionHandoff()),
				inlinePromptInfo("braindead_warning",
					acfg.BraindeadPrompt, ""),
			)

			// Embedded defaults (for reinstall)
			embedded := map[string]string{
				"compaction-summary.md":           prompts.CompactionSummary(),
				"compaction-handoff.md":           prompts.CompactionHandoff(),
				"branch-orientation-multiball.md": prompts.BranchOrientationMultiball(),
				"branch-orientation-headless.md":  prompts.BranchOrientationHeadless(),
				"keepalive.md":                    prompts.Keepalive(),
				"background.md":                   prompts.Background(),
				"memory-formation.md":             prompts.MemoryFormation(),
				"memory-consolidation.md":         prompts.MemoryConsolidation(),
			}

			// Resolved and default texts per label (for diff)
			type promptDef struct {
				label, configPath, filename string
				embeddedDefault             string
			}
			fileDefs := []promptDef{
				{"compaction_summary", resolveString(acfg.CompactionSummaryPrompt, p.cfg.Sessions.CompactionSummaryPrompt), "compaction-summary.md", prompts.CompactionSummary()},
				{"branch_orient_multiball", resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt), "branch-orientation-multiball.md", prompts.BranchOrientationMultiball()},
				{"branch_orient_headless", resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt), "branch-orientation-headless.md", prompts.BranchOrientationHeadless()},
				{"keepalive", acfg.Keepalive.Prompt, "keepalive.md", prompts.Keepalive()},
				{"background", acfg.Background.Prompt, "background.md", prompts.Background()},
				{"memory_formation", acfg.MemoryFormation.IntervalPrompt, "memory-formation.md", prompts.MemoryFormation()},
				{"memory_consolidation", acfg.MemoryFormation.ConsolidationPrompt, "memory-consolidation.md", prompts.MemoryConsolidation()},
				{"memory_session_end", acfg.MemoryFormation.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation()},
			}
			resolvedTexts := make(map[string]string, len(fileDefs)+2)
			defaultTexts := make(map[string]string, len(fileDefs)+2)
			for _, d := range fileDefs {
				resolvedTexts[d.label] = prompts.ResolvePrompt(d.configPath, d.filename, d.embeddedDefault, dirs...)
				defaultTexts[d.label] = d.embeddedDefault
			}
			// Inline prompts
			handoffVal := resolveString(acfg.CompactionHandoffMsg, p.cfg.Sessions.CompactionHandoffMsg)
			if handoffVal == "" {
				resolvedTexts["compaction_handoff"] = prompts.CompactionHandoff()
			} else if handoffVal != "none" {
				resolvedTexts["compaction_handoff"] = handoffVal
			}
			defaultTexts["compaction_handoff"] = prompts.CompactionHandoff()
			if acfg.BraindeadPrompt != "" && acfg.BraindeadPrompt != "none" {
				resolvedTexts["braindead_warning"] = acfg.BraindeadPrompt
			}
			defaultTexts["braindead_warning"] = ""

			// Build set of configured paths for tagging files
			configuredPaths := make(map[string]bool)
			for _, pi := range allPrompts {
				if pi.Path != "" {
					configuredPaths[pi.Path] = true
				}
			}

			// Scan prompt directories
			var promptDirs []string
			var files []command.PromptFile
			sharedDir := filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts")
			wsDir := filepath.Join(acfg.Workspace, "prompts")
			for _, dir := range []string{sharedDir, wsDir} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				promptDirs = append(promptDirs, dir)
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
						continue
					}
					fullPath := filepath.Join(dir, e.Name())
					files = append(files, command.PromptFile{
						Dir:        dir,
						Name:       e.Name(),
						Configured: configuredPaths[fullPath],
					})
				}
			}

			knownFilenames := make(map[string]bool, len(embedded)+1)
			for name := range embedded {
				knownFilenames[name] = true
			}
			knownFilenames["first-run.md"] = true

			return command.PromptsData{
				AgentID:             acfg.ID,
				Prompts:             allPrompts,
				PromptDirs:          promptDirs,
				Files:               files,
				KnownFilenames:      knownFilenames,
				WorkspacePromptsDir: filepath.Join(acfg.Workspace, "prompts"),
				EmbeddedPrompts:     embedded,
				ResolvedTexts:       resolvedTexts,
				DefaultTexts:        defaultTexts,
			}
		},
		SendDocFn: func(path string) error {
			bot := p.botMgr.PrimaryBot(acfg.ID)
			if bot == nil {
				return fmt.Errorf("no bot available")
			}
			return bot.SendDocument(path)
		},
		DiffSummaryFn: func(ctx context.Context, customText, defaultText, name string) (string, error) {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			prompt := fmt.Sprintf("Below are two versions of the %q prompt. These prompts are injected into AI agent sessions to guide agent behaviour during specific operations (compaction, keepalive, memory formation, etc).\n\n--- DEFAULT (embedded) ---\n%s\n\n--- CURRENT (resolved from config) ---\n%s\n\nConcisely summarise: 1) what the default version instructs the agent to do, 2) what the current version instructs, 3) key differences.", name, defaultText, customText)
			resp, err := p.client.SendMessage(callCtx, &provider.MessageRequest{
				Model:    "claude-haiku-4-5-20251001",
				MaxTokens: 1024,
				Messages: []provider.Message{{Role: "user", Content: provider.TextContent(prompt)}},
			})
			if err != nil {
				return "", err
			}
			return provider.TextOf(resp.Content), nil
		},
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

	// Dynamic mana command (configurable name: /mana, /juice, /credits, etc.)
	manaName := p.cfg.ManaWarnings.Name
	if manaName == "" {
		manaName = "mana"
	}
	manaEmoji := []string{"🔮", "✨", "🌙", "⚡", "🪄", "💎", "🌟", "🔥", "🧿", "🪬", "💫", "🌀", "🎇"}
	displayName := strings.ToUpper(manaName[:1]) + manaName[1:]
	manaFn := func(ctx context.Context) (string, error) {
		emoji := manaEmoji[rand.IntN(len(manaEmoji))]
		usage, err := p.usageClient.GetUsage(ctx)
		if err != nil {
			return fmt.Sprintf("%s Error fetching %s: %v", emoji, displayName, err), nil
		}
		percent := mana.FormatPercent(usage)
		if percent == "" {
			return fmt.Sprintf("%s %s: unknown", emoji, displayName), nil
		}
		result := fmt.Sprintf("%s %s: %s remaining", emoji, displayName, percent)
		if reset := mana.FormatReset(usage); reset != "" {
			result += fmt.Sprintf(" (resets %s)", reset)
		}
		return result, nil
	}
	cmds.Register(command.NewManaCommand(manaName, manaFn))

	// /usage — hidden alias for the mana command
	cmds.Register(&command.Command{
		Name:   "usage",
		Hidden: true,
		Execute: func(ctx context.Context, args string) (string, error) {
			return manaFn(ctx)
		},
	})

	// /m — short alias for the mana command
	if manaName != "m" {
		cmds.Register(&command.Command{
			Name:   "m",
			Hidden: true,
			Execute: func(ctx context.Context, args string) (string, error) {
				return manaFn(ctx)
			},
		})
	}

	// /reload command
	cmds.Register(command.NewReloadCommand(func() (string, error) {
		bootstrap.Reload()
		checkSystemPromptSizes(bootstrap, p.cfg.Sessions, acfg.ID)
		newSkillRegistry := skills.Load(skillsDirs)
		var newExtraSystemBlocks []anthropic.SystemBlock
		if newSkillRegistry.Len() > 0 {
			newExtraSystemBlocks = []anthropic.SystemBlock{
				{Type: "text", Text: newSkillRegistry.SystemBlock(acfg.Workspace)},
			}
		}
		ag.ExtraSystemBlocks = newExtraSystemBlocks
		msg := fmt.Sprintf("Reloaded:\n- workspace files (system prompt)\n- %d skills\n\nNote: foci.toml config changes require a service restart to take effect. Prompt file changes take effect immediately.", newSkillRegistry.Len())
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
		func(ctx context.Context) bool { return ag.VoiceMode(sessionKeyFromCtx(ctx)) },
		func(ctx context.Context, on bool) { ag.SetVoiceMode(sessionKeyFromCtx(ctx), on) },
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
		applyAgentDisplaySettings(secBot, acfg, p.cfg)

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

		orientPath := resolveOrientPath(acfg.BranchOrientationMultiballPrompt, p.cfg.Sessions.BranchOrientationMultiballPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		orientText := buildBranchOrientation(orientPath, branchKey, parentKey, "multiball", true, promptSearchDirs)
		if err := p.sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			OrientationMessage: orientText,
		}); err != nil {
			secBot.SetSessionKey("") // release back to pool
			return "", fmt.Errorf("create branch: %w", err)
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
		SecretNames: func() []string { return agentStore.Names() },
		ResolveModel: resolveModelFn,
	}
	cmds.Register(command.NewAgentsCommand(p.agentListFn, cmds, agentNewDeps))
	cmds.Register(command.NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
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
			if dryRun {
				ag.CompactionNotifyFunc(sk, "⏳ Running compaction dry-run...")
			} else {
				ag.CompactionNotifyFunc(sk, "⏳ Compacting context...")
			}
		}
		system := bootstrap.SystemBlocks()
		summaryPrompt := prompts.ResolvePrompt(ag.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), promptSearchDirs...)
		handoffMsg := ag.CompactionHandoffMsg
		if handoffMsg == "" {
			handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), promptSearchDirs...)
		}
		summary, err := ag.Compactor.Compact(ctx, sk, system, summaryPrompt, handoffMsg, dryRun)
		if err != nil {
			return 0, fmt.Errorf("compaction failed: %w", err)
		}
		if dryRun {
			// Dry-run: always send summary as document, skip reload/cache reset
			if ag.CompactionDebugFunc != nil && summary != "" {
				ag.CompactionDebugFunc(sk, summary)
			} else if summary != "" {
				// No debug func configured — send directly via primary bot
				if bot := p.botMgr.PrimaryBot(acfg.ID); bot != nil {
					f, tmpErr := os.CreateTemp("", "compaction-dryrun-*.md")
					if tmpErr == nil {
						if _, writeErr := f.WriteString(summary); writeErr == nil {
							_ = f.Close()
							if sendErr := bot.SendDocument(f.Name()); sendErr != nil {
								log.Warnf("agent", "dry-run: send document: %v", sendErr)
							}
						} else {
							_ = f.Close()
						}
						_ = os.Remove(f.Name())
					}
				}
			}
			if ag.CompactionNotifyFunc != nil {
				ag.CompactionNotifyFunc(sk, "✅ Dry-run complete — summary sent.")
			}
		} else {
			if ag.CompactionNotifyFunc != nil {
				ag.CompactionNotifyFunc(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
			}
			if ag.CompactionDebugFunc != nil && summary != "" {
				ag.CompactionDebugFunc(sk, summary)
			}
			bootstrap.Reload()
			// Reset cache baseline — compaction changed the prefix
			ag.ResetCacheBaseline(sk)
		}
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
		IndexFn: func(opts command.SessionIndexOpts) ([]command.SessionIndexInfo, error) {
			if p.sessionIndex == nil {
				return nil, fmt.Errorf("session index not available")
			}
			qopts := session.QueryOptions{
				SessionType: session.SessionType(opts.TypeFilter),
				Status:      session.SessionStatus(opts.StatusFilter),
				MaxAge:      opts.MaxAge,
				Limit:       50,
			}
			entries, err := p.sessionIndex.Query(qopts)
			if err != nil {
				return nil, err
			}
			var result []command.SessionIndexInfo
			for _, e := range entries {
				result = append(result, command.SessionIndexInfo{
					SessionKey:       e.SessionKey,
					CreatedAt:        e.CreatedAt,
					LastActivityAt:   e.LastActivityAt,
					ParentSessionKey: e.ParentSessionKey,
					SessionType:      string(e.SessionType),
					Status:           string(e.Status),
				})
			}
			return result, nil
		},
	}))
	cmds.Register(command.NewSecretsCommand(p.store))
	cmds.Register(command.NewBitwardenCommand(p.bwStore, p.cfg.Bitwarden.Enabled))
	cmds.Register(command.NewRestartCommand(nil))

	// Finalize exec tool description with dynamically-generated shell function list.
	registry.FinalizeExecDescription()

	// Log registered tools
	allTools := registry.All()
	toolNames := make([]string, len(allTools))
	for i, t := range allTools {
		toolNames[i] = t.Name
	}
	log.Infof("main", "agent %q: registered %d tools: [%s]", acfg.ID, len(toolNames), strings.Join(toolNames, ", "))
	if len(serverTools) > 0 {
		stNames := make([]string, len(serverTools))
		for i, st := range serverTools {
			stNames[i] = st.Name()
		}
		log.Infof("main", "agent %q: server tools: [%s]", acfg.ID, strings.Join(stNames, ", "))
	}

	// Resolve per-agent allowed users (falls back to global)
	allowedUsers := acfg.AllowedUsers
	if len(allowedUsers) == 0 {
		allowedUsers = p.cfg.Telegram.AllowedUsers
	}

	// Create and register Telegram bots via BotManager
	setupTelegram(p, acfg, ag, cmds, allowedUsers, lastMsgStore)

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
		agentCfg:          acfg,
		promptSearchDirs:  promptSearchDirs,
		tmuxClearAll:      tmuxClearAll,
		mcpManager:        mcpMgr,
	}
}

// setupTelegram creates and registers Telegram bots for an agent.
// If the primary bot fails to initialize, the agent continues without Telegram.
func setupTelegram(p setupParams, acfg config.AgentConfig, ag *agent.Agent, cmds *command.Registry, allowedUsers []string, lastMsgStore *command.LastMessageStore) {
	telegramToken := config.ResolveBotToken(acfg.TelegramBot, acfg.BotSecret, p.store)
	if telegramToken == "" {
		return
	}

	primaryBot, err := telegram.NewBot(telegramToken, allowedUsers, ag, cmds, lastMsgStore, acfg.ID)
	if err != nil {
		log.Errorf("main", "agent %q: create telegram bot: %v (agent will run without Telegram)", acfg.ID, err)
		return
	}

	if p.stateStore != nil {
		botKey := "bot:" + acfg.TelegramBot
		if botKey == "bot:" {
			botKey = "bot:" + acfg.ID
		}
		primaryBot.SetStateStore(p.stateStore, botKey)
	}
	if p.toolDetailStore != nil {
		primaryBot.SetToolDetailStore(p.toolDetailStore)
	}

	if p.sttProvider != nil {
		primaryBot.SetTranscriber(p.sttProvider)
	}
	if p.ttsProvider != nil {
		primaryBot.SetTTS(voice.WithRate(p.ttsProvider, acfg.TTSRate))
	}
	primaryBot.SetStopAliases(p.cfg.Telegram.StopAliases, p.cfg.Telegram.EnableStopAliases)
	primaryBot.SetToolCallPreviewChars(p.cfg.Tools.ToolCallPreviewChars)
	applyAgentDisplaySettings(primaryBot, acfg, p.cfg)

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
			log.Infof("mana", "%s", warn)
			primaryBot.SendNotification("⚠️ " + warn)
		}
	}

	// Wire rate limit notifications to Telegram
	ag.RateLimitFunc = func(retryAfter int) {
		msg := "I've run out of mana, it will reset in ~5 hours."
		if retryAfter > 0 {
			mins := (retryAfter + 59) / 60
			if mins >= 60 {
				msg = fmt.Sprintf("I've run out of mana, it will reset in ~%dh %dm.", mins/60, mins%60)
			} else {
				msg = fmt.Sprintf("I've run out of mana, it will reset in ~%d minutes.", mins)
			}
		}
		primaryBot.SendNotification("⚡ " + msg)
	}

	// Wire max_tokens warnings to Telegram
	ag.MaxTokensWarnFunc = func(warn string) {
		primaryBot.SendNotification("⚠️ " + warn)
	}

	// Wire compaction notifications to Telegram (default on)
	// Per-agent compaction_notify overrides global
	compactNotify := p.cfg.Sessions.CompactionNotify
	if acfg.CompactionNotify != nil {
		compactNotify = acfg.CompactionNotify
	}
	if compactNotify == nil || *compactNotify {
		ag.CompactionNotifyFunc = func(session string, msg string) {
			primaryBot.SendNotification(msg)
		}
	}

	// Wire session activity tracking for the session index.
	if p.sessionIndex != nil {
		sidx := p.sessionIndex // capture for closure
		ag.OnActivity = func(sessionKey string) {
			sidx.TouchActivity(sessionKey)
		}
	}

	// Wire compaction debug (send summary as file attachment)
	compactDebug := p.cfg.Sessions.CompactionDebug
	if acfg.CompactionDebug != nil {
		compactDebug = *acfg.CompactionDebug
	}
	if compactDebug {
		bot := primaryBot // capture for closure
		ag.CompactionDebugFunc = func(sessionKey, summary string) {
			f, err := os.CreateTemp("", "compaction-summary-*.md")
			if err != nil {
				log.Warnf("agent", "compaction debug: create temp file: %v", err)
				return
			}
			if _, err := f.WriteString(summary); err != nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
				log.Warnf("agent", "compaction debug: write temp file: %v", err)
				return
			}
			_ = f.Close()
			if err := bot.SendDocument(f.Name()); err != nil {
				log.Warnf("agent", "compaction debug: send document: %v", err)
			}
			_ = os.Remove(f.Name())
		}
	}

	p.botMgr.AddPrimary(acfg.ID, primaryBot)

	// Per-agent multiball bots (if configured)
	for _, botName := range acfg.MultiballBots {
		mbToken := config.ResolveBotToken(botName, "", p.store)
		if mbToken == "" {
			log.Errorf("main", "agent %q: multiball bot %q: token not found", acfg.ID, botName)
			continue
		}
		mbBot, err := telegram.NewBot(mbToken, allowedUsers, ag, cmds, lastMsgStore, "") // secondary: no agentID
		if err != nil {
			log.Errorf("main", "agent %q: create multiball bot %q: %v", acfg.ID, botName, err)
			continue
		}
		configureMultiballBot(mbBot, multiballBotConfig{
			sttProvider:     p.sttProvider,
			ttsProvider:     p.ttsProvider,
			stopAliases:     p.cfg.Telegram.StopAliases,
			enableStopAlias: p.cfg.Telegram.EnableStopAliases,
			acfg:            acfg,
			cfg:             p.cfg,
			toolDetailStore: p.toolDetailStore,
			stateStore:      p.stateStore,
		})
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
		reclaimOrientPath := resolveOrientPath(acfg.BranchOrientationHeadlessPrompt, p.cfg.Sessions.BranchOrientationHeadlessPrompt, acfg.BranchOrientationPrompt, p.cfg.Sessions.BranchOrientationPrompt)
		reclaimMfCfg := acfg.MemoryFormation
		reclaimSearchDirs := []string{
			filepath.Join(acfg.Workspace, "prompts"),
			filepath.Join(filepath.Dir(acfg.Workspace), "shared", "prompts"),
		}
		pool.ReclaimHook = func(sessionKey string) {
			fireSessionEndMemory(ag, p.sessions, sessionKey, reclaimMfCfg, func(bk, pk, bt string) string {
				return buildBranchOrientation(reclaimOrientPath, bk, pk, bt, false, reclaimSearchDirs)
			}, reclaimSearchDirs, p.ctx)
		}
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

// buildContextInfoFn returns the closure used by the /context command.
// It computes system prompt section sizes, message breakdown, and provides
// a CountTokensFn that fires parallel API calls to count tokens per section.
func buildContextInfoFn(
	ag *agent.Agent,
	bootstrap *workspace.Bootstrap,
	registry *tools.Registry,
	acfg config.AgentConfig,
	client provider.Client,
	sessions *session.Store,
	defaultSessionKey func() string,
	compactionThreshold float64,
) func() command.ContextInfo {
	// Token count cache (persists across calls, invalidated when context changes)
	var (
		tcCacheMu     sync.Mutex
		tcCacheCounts *command.TokenCounts
		tcCacheMsgCnt int
		tcCacheSysChr int
	)

	return func() command.ContextInfo {
		// System prompt section sizes from workspace files
		var sections []command.SystemSection
		for _, s := range bootstrap.SectionSizes() {
			sections = append(sections, command.SystemSection{Name: s.Name, Chars: s.Chars})
		}
		// Skills/extra system blocks character count
		var skillsChars int
		for _, b := range ag.ExtraSystemBlocks {
			skillsChars += len(b.Text)
		}
		// System chars total (used as cache key)
		totalSysChars := len(ag.EnvironmentBlock) + skillsChars
		for _, s := range sections {
			totalSysChars += s.Chars
		}
		// Load messages once (shared between breakdown and counting)
		sk := defaultSessionKey()
		var msgs []anthropic.Message
		if sk != "" {
			if loaded, err := sessions.LoadFull(sk); err == nil {
				msgs = loaded
			}
		}
		msgCount := len(msgs)
		// Message breakdown from loaded messages
		var mb command.MessageBreakdown
		for _, m := range msgs {
			chars := 0
			var hasToolResult bool
			for _, cb := range m.Content {
				switch cb.Type {
				case "text":
					chars += len(cb.Text)
				case "tool_use":
					chars += len(cb.Name) + len(cb.Input)
				case "tool_result":
					chars += len(cb.Content)
					hasToolResult = true
				}
			}
			switch {
			case hasToolResult:
				mb.ToolResultChars += chars
			case m.Role == "user":
				mb.UserChars += chars
				mb.UserCount++
			case m.Role == "assistant":
				mb.AssistantChars += chars
				mb.AssistantCount++
			}
		}
		return command.ContextInfo{
			SessionKey:       sk,
			Model:            ag.Model,
			CompactionThresh: compactionThreshold,
			ContextLimit:     compaction.ContextLimit(ag.Model),
			SystemSections:   sections,
			EnvironmentChars: len(ag.EnvironmentBlock),
			SkillsChars:      skillsChars,
			Messages:         mb,
			CountTokensFn: func(ctx context.Context) (*command.TokenCounts, error) {
				// Check cache
				tcCacheMu.Lock()
				if tcCacheCounts != nil && tcCacheMsgCnt == msgCount && tcCacheSysChr == totalSysChars {
					r := tcCacheCounts
					tcCacheMu.Unlock()
					return r, nil
				}
				tcCacheMu.Unlock()

				// Build full system blocks (same assembly as agent)
				bootstrapBlocks := bootstrap.SystemBlocks()
				bootstrapSizes := bootstrap.SectionSizes()
				// Strip cache_control from bootstrap blocks
				for i := range bootstrapBlocks {
					bootstrapBlocks[i].CacheControl = nil
				}

				var allSystem []anthropic.SystemBlock
				if ag.EnvironmentBlock != "" {
					allSystem = append(allSystem, anthropic.SystemBlock{Type: "text", Text: ag.EnvironmentBlock})
				}
				allSystem = append(allSystem, bootstrapBlocks...)
				var cleanExtra []anthropic.SystemBlock
				if len(ag.ExtraSystemBlocks) > 0 {
					cleanExtra = make([]anthropic.SystemBlock, len(ag.ExtraSystemBlocks))
					copy(cleanExtra, ag.ExtraSystemBlocks)
					for i := range cleanExtra {
						cleanExtra[i].CacheControl = nil
					}
					allSystem = append(allSystem, cleanExtra...)
				}

				// Build per-section list for individual counting
				type sectionDef struct {
					name   string
					blocks []anthropic.SystemBlock
				}
				var secs []sectionDef
				if ag.EnvironmentBlock != "" {
					secs = append(secs, sectionDef{
						name:   "Environment",
						blocks: []anthropic.SystemBlock{{Type: "text", Text: ag.EnvironmentBlock}},
					})
				}
				for i, sz := range bootstrapSizes {
					if i < len(bootstrapBlocks) {
						secs = append(secs, sectionDef{
							name:   sz.Name,
							blocks: []anthropic.SystemBlock{bootstrapBlocks[i]},
						})
					}
				}
				if len(cleanExtra) > 0 {
					secs = append(secs, sectionDef{name: "Skills", blocks: cleanExtra})
				}

				// Common request components
				dummyMsgs := []anthropic.Message{
					{Role: "user", Content: anthropic.TextContent(".")},
				}
				toolDefs := registry.ToolDefs()
				maxOutput := acfg.MaxOutputTokens
				if maxOutput <= 0 {
					maxOutput = 8192
				}
				messages := msgs
				if len(messages) == 0 {
					messages = dummyMsgs
				}

				// Parallel API calls
				var wg sync.WaitGroup
				var fullCount, systemCount, baselineCount int
				var fullErr, systemErr, baselineErr error
				rawSecCounts := make([]int, len(secs))
				rawSecErrs := make([]error, len(secs))

				wg.Add(3 + len(secs))

				go func() {
					defer wg.Done()
					fullCount, fullErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						System: allSystem, Messages: messages, Tools: toolDefs,
					})
				}()
				go func() {
					defer wg.Done()
					systemCount, systemErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						System: allSystem, Messages: dummyMsgs, Tools: toolDefs,
					})
				}()
				go func() {
					defer wg.Done()
					baselineCount, baselineErr = client.CountTokens(ctx, &anthropic.MessageRequest{
						Model: ag.Model, MaxTokens: maxOutput,
						Messages: dummyMsgs, Tools: toolDefs,
					})
				}()
				for i, sec := range secs {
					i, sec := i, sec
					go func() {
						defer wg.Done()
						rawSecCounts[i], rawSecErrs[i] = client.CountTokens(ctx, &anthropic.MessageRequest{
							Model: ag.Model, MaxTokens: maxOutput,
							System: sec.blocks, Messages: dummyMsgs, Tools: toolDefs,
						})
					}()
				}

				wg.Wait()

				if fullErr != nil {
					return nil, fullErr
				}
				if systemErr != nil {
					return nil, systemErr
				}
				if baselineErr != nil {
					return nil, baselineErr
				}

				tc := &command.TokenCounts{
					Total:        fullCount,
					System:       systemCount - baselineCount,
					Conversation: fullCount - systemCount,
					Tools:        baselineCount,
				}
				for i, sec := range secs {
					tokens := 0
					if rawSecErrs[i] == nil {
						tokens = rawSecCounts[i] - baselineCount
						if tokens < 0 {
							tokens = 0
						}
					}
					tc.Sections = append(tc.Sections, command.SectionTokens{
						Name: sec.name, Tokens: tokens,
					})
				}

				// Update cache
				tcCacheMu.Lock()
				tcCacheCounts = tc
				tcCacheMsgCnt = msgCount
				tcCacheSysChr = totalSysChars
				tcCacheMu.Unlock()

				return tc, nil
			},
		}
	}
}

// countCrontabJobs counts the number of active cron jobs for the current user
func countCrontabJobs() int {
	cmd := exec.Command("sh", "-c", "crontab -l 2>/dev/null | grep -v '^#' | grep -v '^$' | wc -l")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0
	}
	return count
}

// buildEnvironmentBlock generates the environment system block content
// from config values known at startup.
func buildEnvironmentBlock(acfg config.AgentConfig, configPath string, cfg *config.Config, crontabCount int) string {
	logDir := filepath.Dir(cfg.Logging.EventFile)

	var b strings.Builder
	b.WriteString("# Environment\n\n")
	b.WriteString("You are running on **foci**, an AI agent platform.\n\n")

	// Workspace
	b.WriteString("## Workspace\n")
	fmt.Fprintf(&b, "- Workspace: %s\n", acfg.Workspace)
	fmt.Fprintf(&b, "- Agent ID: %s\n", acfg.ID)
	b.WriteString("- Platform: foci (https://github.com/richardtkemp/foci)\n")
	if cfg.Environment.DocsPath != "" {
		fmt.Fprintf(&b, "- Platform docs: %s\n", cfg.Environment.DocsPath)
	}
	if acfg.TelegramBot != "" {
		b.WriteString("- Messaging: Telegram\n")
	}
	fmt.Fprintf(&b, "- You may schedule recurring tasks using crontab. You have %d jobs scheduled.\n", crontabCount)

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
	b.WriteString("- **mana** — remaining API quota percentage, followed by 🟢 (above invest threshold — safe for heavy work) or 🔴 (low — conserve, avoid expensive operations)\n")

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

	// Visibility: resolve effective show_tool_calls and show_thinking
	toolCalls := config.ToolCallOff
	if acfg.ShowToolCalls != nil {
		toolCalls = *acfg.ShowToolCalls
	} else if cfg.Defaults.ShowToolCalls != nil {
		toolCalls = *cfg.Defaults.ShowToolCalls
	}
	thinking := config.ShowThinkingOff
	if acfg.ShowThinking != nil {
		thinking = *acfg.ShowThinking
	} else if cfg.Defaults.ShowThinking != nil {
		thinking = *cfg.Defaults.ShowThinking
	}
	var toolDesc, thinkDesc string
	switch toolCalls {
	case config.ToolCallOff:
		toolDesc = "Tool calls are hidden from the user — narrate important actions in your replies."
	case config.ToolCallPreview:
		toolDesc = "Tool calls are shown as brief previews (tool name only) — the user sees what tools you use but not the details."
	case config.ToolCallFull:
		toolDesc = "Tool calls are fully visible — the user can see your tool inputs and outputs."
	}
	switch thinking {
	case config.ShowThinkingOff:
		thinkDesc = "Your thinking is hidden from the user."
	case config.ShowThinkingCompact:
		thinkDesc = "Your thinking is available behind a toggle button."
	case config.ShowThinkingTrue:
		thinkDesc = "Your thinking is shown inline before each response."
	}
	if toolDesc != "" || thinkDesc != "" {
		b.WriteString("\n## Visibility\n")
		if toolDesc != "" {
			b.WriteString(toolDesc + "\n")
		}
		if thinkDesc != "" {
			b.WriteString(thinkDesc + "\n")
		}
	}

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

// checkFirstRun determines whether a first-run onboarding prompt should be
// injected for an agent. Returns the prompt message if injection is needed,
// empty string otherwise. Uses state.json to track completion.
func checkFirstRun(stateStore *state.Store, acfg config.AgentConfig) string {
	if stateStore == nil {
		return ""
	}

	key := "agent:" + acfg.ID + ":first_run_completed"

	// Already completed — nothing to do
	var completed bool
	if stateStore.Get(key, &completed) && completed {
		return ""
	}

	// First run — inject the onboarding prompt
	prompt := prompts.FirstRun()
	if prompt == "" {
		return ""
	}

	log.Infof("main", "agent %s: first run detected, injecting onboarding prompt", acfg.ID)
	return prompt
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

// memoryResult holds the outputs of initMemorySystem.
type memoryResult struct {
	sharedBackends  map[string]memory.Searcher            // backend name -> searcher (shared mode)
	agentBackends   map[string]map[string]memory.Searcher // agentID -> backend name -> searcher
	sharedFTS5      *memory.Index                         // for conversation hook (shared mode)
	agentFTS5       map[string]*memory.Index              // for conversation hook (per-agent mode)
	reminderStore   *memory.ReminderStore
	scratchpadStore *memory.Scratchpad
	todoStore       *memory.TodoStore
	cleanup         func()
}

// initMemorySystem sets up memory indices, reminder/scratchpad/todo stores,
// and conversation hooks. Returns a memoryResult with a cleanup function
// that closes all opened resources.
func initMemorySystem(cfg *config.Config) memoryResult {
	var closers []io.Closer
	result := memoryResult{
		sharedBackends: make(map[string]memory.Searcher),
		agentBackends:  make(map[string]map[string]memory.Searcher),
		agentFTS5:      make(map[string]*memory.Index),
		cleanup:        func() {},
	}

	// Build global source map from [memory] config
	globalMemSources := make(map[string]memory.SourceConfig)
	for _, src := range cfg.Memory.Sources {
		globalMemSources[src.Name] = memory.SourceConfig{Dir: src.Dir, Weight: src.Weight}
	}

	// Parse debounce delay
	var memDebounce time.Duration
	if cfg.Memory.ReindexDebounce != "" {
		var err error
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

	memoryEnabled := len(globalMemSources) > 0 || hasPerAgentMemory
	if !memoryEnabled {
		return result
	}

	// Parse sweep interval ("0" disables)
	var sweepInterval time.Duration
	if cfg.Memory.SweepInterval != "" && cfg.Memory.SweepInterval != "0" {
		var err error
		sweepInterval, err = time.ParseDuration(cfg.Memory.SweepInterval)
		if err != nil {
			log.Fatalf("main", "invalid sweep_interval: %v", err)
		}
	}

	wantFTS5 := cfg.Memory.HasBackend("fts5")
	wantBleve := cfg.Memory.HasBackend("bleve")

	// initBackends creates FTS5 and/or bleve backends for a given set of sources,
	// returning the backend map and (optionally) the FTS5 index for conversation hooks.
	initBackends := func(label string, sources map[string]memory.SourceConfig, dbPrefix string, blevePrefix string) (map[string]memory.Searcher, *memory.Index) {
		backends := make(map[string]memory.Searcher)
		var fts5Idx *memory.Index

		if wantFTS5 {
			dbPath := cfg.DataPath(dbPrefix)
			idx, err := memory.NewIndex(dbPath, sources, memDebounce, cfg.Memory.ConversationWeight)
			if err != nil {
				log.Fatalf("main", "create FTS5 index (%s): %v", label, err)
			}
			closers = append(closers, idx)
			if err := idx.Reindex(); err != nil {
				log.Errorf("main", "reindex FTS5 (%s): %v", label, err)
			}
			if memDebounce > 0 || len(sources) > 0 {
				if err := idx.Watch(); err != nil {
					log.Errorf("main", "start FTS5 file watching (%s): %v", label, err)
				}
			}
			if sweepInterval > 0 {
				idx.StartSweep(30*time.Second, sweepInterval)
			}
			backends["fts5"] = idx
			fts5Idx = idx
		}

		if wantBleve {
			blevePath := cfg.DataPath(blevePrefix)
			bidx, err := memory.NewBleveIndex(blevePath, sources, memDebounce)
			if err != nil {
				log.Fatalf("main", "create bleve index (%s): %v", label, err)
			}
			closers = append(closers, bidx)
			if err := bidx.Reindex(); err != nil {
				log.Errorf("main", "reindex bleve (%s): %v", label, err)
			}
			if memDebounce > 0 || len(sources) > 0 {
				if err := bidx.Watch(); err != nil {
					log.Errorf("main", "start bleve file watching (%s): %v", label, err)
				}
			}
			if sweepInterval > 0 {
				bidx.StartSweep(30*time.Second, sweepInterval)
			}
			backends["bleve"] = bidx
		}

		return backends, fts5Idx
	}

	if hasPerAgentMemory {
		// Per-agent indices: each agent gets global + agent-specific sources
		for _, acfg := range cfg.Agents {
			combined := buildAgentMemorySources(globalMemSources, acfg.Memory.Sources)
			if len(combined) == 0 {
				continue
			}
			backends, fts5Idx := initBackends(
				fmt.Sprintf("agent %s", acfg.ID),
				combined,
				fmt.Sprintf("memory-%s.db", acfg.ID),
				fmt.Sprintf("memory-%s.bleve", acfg.ID),
			)
			result.agentBackends[acfg.ID] = backends
			if fts5Idx != nil {
				result.agentFTS5[acfg.ID] = fts5Idx
			}
			log.Infof("main", "agent %q: memory backends %v with %d sources", acfg.ID, cfg.Memory.SearchBackends, len(combined))
		}

		// Conversation hook: route to agent's FTS5 index by session key prefix
		if wantFTS5 {
			log.ConversationHook = func(text, session string) {
				for agentID, idx := range result.agentFTS5 {
					if strings.HasPrefix(session, "agent:"+agentID+":") {
						idx.IndexConversation(text, session)
						return
					}
				}
			}
		}
	} else {
		// Shared indices (backward compat — no agent has per-agent memory)
		backends, fts5Idx := initBackends("shared", globalMemSources, "memory.db", "memory.bleve")
		result.sharedBackends = backends
		result.sharedFTS5 = fts5Idx

		if fts5Idx != nil {
			log.ConversationHook = fts5Idx.IndexConversation
		}
	}

	// Reminder store (shared across agents)
	reminderDbPath := cfg.DataPath("reminders.db")
	var err error
	result.reminderStore, err = memory.NewReminderStore(reminderDbPath)
	if err != nil {
		log.Fatalf("main", "create reminder store: %v", err)
	}
	closers = append(closers, result.reminderStore)

	// Scratchpad (shared across agents)
	scratchpadDbPath := cfg.DataPath("scratchpad.db")
	result.scratchpadStore, err = memory.NewScratchpad(scratchpadDbPath)
	if err != nil {
		log.Fatalf("main", "create scratchpad: %v", err)
	}
	closers = append(closers, result.scratchpadStore)

	// Todo list (shared across agents, agent_id scoped per-agent)
	todoDbPath := cfg.DataPath("todo.db")
	result.todoStore, err = memory.NewTodoStore(todoDbPath)
	if err != nil {
		log.Fatalf("main", "create todo store: %v", err)
	}
	closers = append(closers, result.todoStore)

	result.cleanup = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i].Close()
		}
	}
	return result
}

// resolveCredentials resolves the Anthropic API client and usage client.
//
// API client priority: (1) setup-token, (2) API key, (3) Claude Code credentials.
// Usage client: always from CC credentials (polls ~/.claude/.credentials.json).
//
// For static tokens, the client uses a tokenFunc backed by a tokenHolder,
// enabling hot-reload via /-/reload-credentials.
// Returns the tokenHolder (nil for CC-backed client, which polls the file).
func resolveCredentials(cfg *config.Config, store *secrets.Store, ctx context.Context) (*anthropic.Client, *anthropic.UsageClient, *tokenHolder) {
	setupToken, _ := store.Get("anthropic.setup_token")
	apiKey, _ := store.Get("anthropic.api_key")
	httpTimeout, err := time.ParseDuration(cfg.Anthropic.HTTPTimeout)
	if err != nil {
		log.Warnf("main", "invalid anthropic.http_timeout, using default: %v", err)
		httpTimeout = 120 * time.Second
	}
	ccPollInterval, err := time.ParseDuration(cfg.Anthropic.CCCredentialsPollInterval)
	if err != nil {
		log.Warnf("main", "invalid anthropic.cc_credentials_poll_interval, using default: %v", err)
		ccPollInterval = 30 * time.Second
	}

	const ccCredsFile = "~/.claude/.credentials.json"

	// CC token source — shared between usage client and (optionally) main client.
	// Created once; polls the file, never refreshes tokens.
	var ccSrc *anthropic.CCTokenSource
	if src, err := anthropic.NewCCTokenSource(ccCredsFile, ccPollInterval); err == nil {
		src.OnExpired(func() {
			log.Warnf("main", "CC credentials expired — starting claude to refresh")
			go startClaudeForRefresh()
		})
		src.Start(ctx)
		ccSrc = src
		log.Infof("main", "CC token source configured (%s, poll %s)", ccCredsFile, ccPollInterval)
	}

	// Usage client — always from CC credentials (required for /api/oauth/usage).
	var usageClient *anthropic.UsageClient
	if ccSrc != nil {
		usageClient = anthropic.NewUsageClientWithFunc(ccSrc.Token)
		log.Infof("main", "usage client configured (CC credentials)")
	}

	// Source 1: setup-token (from `foci auth` / `claude setup-token`)
	if setupToken != "" {
		log.Infof("main", "using setup-token from secrets.toml")
		holder := &tokenHolder{token: setupToken}
		return anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout),
			usageClient, holder
	}

	// Source 2: Anthropic API key
	if apiKey != "" {
		log.Infof("main", "using API key from secrets.toml")
		holder := &tokenHolder{token: apiKey}
		return anthropic.NewClientWithTokenFunc(holder.Get, httpTimeout),
			usageClient, holder
	}

	// Source 3: Claude Code credentials (passive — poll file, never refresh)
	if ccSrc != nil {
		log.Infof("main", "using CC credentials from %s (passive, poll-based)", ccCredsFile)
		return anthropic.NewClientWithTokenFunc(ccSrc.Token, httpTimeout),
			usageClient, nil
	}

	log.Errorf("main", "no Anthropic token found — run: foci auth")
	os.Exit(1)
	return nil, nil, nil // unreachable
}

// startClaudeForRefresh sends a trivial query via Claude Code to force a
// token refresh. `claude auth status` doesn't actually refresh tokens —
// only a real API call does. Fire-and-forget — logs errors but never blocks.
func startClaudeForRefresh() {
	cmd := exec.Command("claude",
		"--model", "haiku",
		"--system-prompt", "",
		"--print",
		"--effort", "low",
		"1+1",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		log.Warnf("main", "claude token refresh failed (CC may not be installed): %v", err)
	} else {
		log.Infof("main", "claude token refresh completed")
	}
}

// resolveInt returns the per-agent value if non-zero, otherwise global.
func resolveInt(perAgent, global int) int {
	if perAgent != 0 {
		return perAgent
	}
	return global
}

// resolveBoolPtr returns the per-agent value if non-nil, otherwise the global default.
func resolveBoolPtr(perAgent *bool, global bool) bool {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// parseDurationDefault parses a Go duration string, returning fallback on error or empty.
func parseDurationDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// initVoice sets up STT and TTS providers based on config and available API keys.
func initVoice(cfg *config.Config, groqKey, openrouterKey string) (voice.STT, voice.TTS) {
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

	return sttProvider, ttsProvider
}

// buildServerTool constructs an anthropic server tool config map with optional
// max_uses, allowed_domains, and blocked_domains fields.
func buildServerTool(toolType, toolName string, maxUses int, allowed, blocked []string) anthropic.ToolDef {
	cfg := map[string]interface{}{
		"type": toolType,
		"name": toolName,
	}
	if maxUses > 0 {
		cfg["max_uses"] = maxUses
	}
	if len(allowed) > 0 {
		cfg["allowed_domains"] = allowed
	}
	if len(blocked) > 0 {
		cfg["blocked_domains"] = blocked
	}
	return anthropic.NewServerTool(cfg)
}

// resolveInt64 returns the per-agent value if non-zero, otherwise global.
func resolveInt64(perAgent, global int64) int64 {
	if perAgent != 0 {
		return perAgent
	}
	return global
}

// resolveOrientPath resolves the branch orientation prompt path for a given variant.
// Precedence: specific per-agent → specific global → deprecated per-agent → deprecated global.
func resolveOrientPath(specificAgent, specificGlobal, deprecatedAgent, deprecatedGlobal string) string {
	if specificAgent != "" {
		return specificAgent
	}
	if specificGlobal != "" {
		return specificGlobal
	}
	return resolveString(deprecatedAgent, deprecatedGlobal)
}

// resolveIntPtr returns *perAgent if non-nil, otherwise global.
func resolveIntPtr(perAgent *int, global int) int {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// resolveFloat64Ptr returns *perAgent if non-nil, otherwise global.
func resolveFloat64Ptr(perAgent *float64, global float64) float64 {
	if perAgent != nil {
		return *perAgent
	}
	return global
}

// resolveString returns the per-agent value if non-empty, otherwise global.
func resolveString(perAgent, global string) string {
	if perAgent != "" {
		return perAgent
	}
	return global
}

// resolveMessageTransforms returns per-agent message transforms if set, otherwise global.
func resolveMessageTransforms(acfg config.AgentConfig, cfg *config.Config) []config.MessageTransform {
	if len(acfg.MessageTransforms) > 0 {
		return acfg.MessageTransforms
	}
	return cfg.MessageTransforms
}

// resolveBlockedPaths returns per-agent blocked paths if set, otherwise global.
func resolveBlockedPaths(acfg config.AgentConfig, cfg *config.Config) []config.BlockedPath {
	if len(acfg.BlockedPaths) > 0 {
		return acfg.BlockedPaths
	}
	return cfg.BlockedPaths
}

// hasMemoryFormation returns true if any memory formation feature is enabled.
// All three default to true (nil *bool = true), so returns false only when
// all are explicitly disabled.
func hasMemoryFormation(mf config.MemoryFormationConfig) bool {
	intervalEnabled := mf.IntervalEnabled == nil || *mf.IntervalEnabled
	consolidationEnabled := mf.ConsolidationEnabled == nil || *mf.ConsolidationEnabled
	sessionEndEnabled := mf.SessionEndEnabled == nil || *mf.SessionEndEnabled
	return intervalEnabled || consolidationEnabled || sessionEndEnabled
}

// seedDefaultPrompts writes embedded prompt files to dir if they don't already
// exist. This gives users editable copies they can customise.
func seedDefaultPrompts(dir string) {
	promptFiles := map[string]func() string{
		"keepalive.md":                    prompts.Keepalive,
		"background.md":                   prompts.Background,
		"memory-formation.md":             prompts.MemoryFormation,
		"memory-consolidation.md":         prompts.MemoryConsolidation,
		"compaction-summary.md":           prompts.CompactionSummary,
		"compaction-handoff.md":           prompts.CompactionHandoff,
		"branch-orientation-headless.md":  prompts.BranchOrientationHeadless,
		"branch-orientation-multiball.md": prompts.BranchOrientationMultiball,
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Warnf("main", "seed prompts: mkdir %s: %v", dir, err)
		return
	}

	for name, fn := range promptFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // already exists
		}
		if err := os.WriteFile(path, []byte(fn()+"\n"), 0644); err != nil {
			log.Warnf("main", "seed prompts: write %s: %v", path, err)
			continue
		}
		log.Infof("main", "seeded default prompt: %s", path)
	}
}

// buildBranchFunc creates a keepalive.BranchFunc that dispatches branch sessions
// using the provided agent infrastructure. This is the bridge between the keepalive
// package and the main package's agent/session handling.
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	buildOrientation func(branchKey, parentKey, branchType string) string,
	ctx context.Context,
) keepalive.BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf("keepalive", "no default session for agent %q, skipping %s", agentID, branchType)
			return
		}

		branchID := fmt.Sprintf("%s-%d", branchType, time.Now().Unix())
		branchKey := fmt.Sprintf("agent:%s:cron:%s", agentID, branchID)

		orientText := buildOrientation(branchKey, parentKey, branchType)
		err := sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        true,
			OrientationMessage: orientText,
		})
		if err != nil {
			log.Errorf("keepalive", "%s branch error: %v", branchType, err)
			return
		}

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			turnCtx = agent.WithNoCompact(turnCtx)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Errorf("keepalive", "%s turn error: %v", branchType, err)
			return
		}
		_ = resp // keepalive/background responses are not delivered to user
	}
}

// buildBranchOrientation constructs orientation text for a branch session.
// Resolves the prompt through ResolvePrompt: explicit path → search dirs → embedded default.
// Template variables: {branch_key}, {parent_key}, {branch_type}, {direct_chat}.
func buildBranchOrientation(promptPath, branchKey, parentKey, branchType string, directChat bool, searchDirs []string) string {
	var filename, embedded string
	if directChat {
		filename = "branch-orientation-multiball.md"
		embedded = prompts.BranchOrientationMultiball()
	} else {
		filename = "branch-orientation-headless.md"
		embedded = prompts.BranchOrientationHeadless()
	}
	text := prompts.ResolvePrompt(promptPath, filename, embedded, searchDirs...)
	return prompts.ReplaceVars(text, map[string]string{
		"branch_key":  branchKey,
		"parent_key":  parentKey,
		"branch_type": branchType,
		"direct_chat": fmt.Sprintf("%v", directChat),
	})
}

// resolvePromptInfo builds a PromptInfo for a file-path-based prompt, comparing
// the resolved text against the embedded default via md5 to detect customisation.
func resolvePromptInfo(label, configPath, filename, embeddedDefault string, searchDirs []string) command.PromptInfo {
	if configPath == "none" {
		return command.PromptInfo{Label: label, Filename: filename, Disabled: true}
	}

	resolved := prompts.ResolvePrompt(configPath, filename, embeddedDefault, searchDirs...)
	isDefault := md5.Sum([]byte(resolved)) == md5.Sum([]byte(embeddedDefault))

	// Find the actual file path being used
	path := configPath
	if path == "" || path == "default" {
		// Search dirs — find which file was used
		for _, dir := range searchDirs {
			fp := filepath.Join(dir, filename)
			if _, err := os.Stat(fp); err == nil {
				path = fp
				break
			}
		}
	}

	if path == "" || path == "default" {
		// Using embedded default, no file on disk
		return command.PromptInfo{Label: label, Filename: filename, Default: isDefault}
	}

	_, err := os.Stat(path)
	return command.PromptInfo{Label: label, Path: path, Filename: filename, Exists: err == nil, Default: isDefault}
}

// inlinePromptInfo builds a PromptInfo for an inline prompt value,
// comparing against the embedded default via md5.
func inlinePromptInfo(label, value, embeddedDefault string) command.PromptInfo {
	if value == "" {
		return command.PromptInfo{Label: label, Inline: embeddedDefault, Default: true}
	}
	if value == "none" {
		return command.PromptInfo{Label: label, Disabled: true}
	}
	isDefault := md5.Sum([]byte(value)) == md5.Sum([]byte(embeddedDefault))
	return command.PromptInfo{Label: label, Inline: value, Default: isDefault}
}

// fireSessionEndMemory runs memory formation on the expiring session before it is cleared.
// Creates an async branch from the session so the caller can proceed immediately.
// Checks BranchMeta.NoResetHook and memory_formation.session_end_enabled.
func fireSessionEndMemory(ag *agent.Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, buildOrientation func(branchKey, parentKey, branchType string) string, searchDirs []string, parentCtx context.Context) {
	if mfCfg.SessionEndEnabled != nil && !*mfCfg.SessionEndEnabled {
		return
	}

	prompt := prompts.ResolvePrompt(mfCfg.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	// Check branch metadata for NoResetHook
	meta, err := sessions.GetBranchMeta(sessionKey)
	if err != nil {
		log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
	}
	if meta != nil && meta.NoResetHook {
		log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
		return
	}

	// Branch from expiring session so the memory formation job has conversation history.
	// The caller proceeds immediately to clear the main session.
	branchID := fmt.Sprintf("session-end-%d", time.Now().Unix())
	branchKey := sessionKey + ":branch:" + branchID
	orientText := buildOrientation(branchKey, sessionKey, "session-end-memory")
	if err := sessions.CreateBranchWithOptions(sessionKey, branchKey, session.BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		log.Errorf("session-end-memory", "branch error: %v", err)
		return
	}

	log.Infof("session-end-memory", "firing for %s → %s", sessionKey, branchKey)

	go func() {
		hookCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
		hookCtx = agent.WithTrigger(hookCtx, "session_end_memory")
		hookCtx = agent.WithNoCompact(hookCtx)
		if _, err := ag.HandleMessage(hookCtx, branchKey, prompt); err != nil {
			log.Warnf("session-end-memory", "failed for %s: %v", branchKey, err)
		}
	}()
}

// sessionBranchAdapter wraps session.Store to implement tools.SessionBrancher.
type sessionBranchAdapter struct {
	store *session.Store
}

func (a *sessionBranchAdapter) CreateBranch(parentKey, branchKey string, opts tools.BranchOptions) error {
	return a.store.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		NoResetHook:        opts.NoResetHook,
		OrientationMessage: opts.OrientationMessage,
	})
}

func (a *sessionBranchAdapter) SessionPath(key string) (string, error) {
	return a.store.SessionPath(key)
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
	cfg *config.Config,
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
				_ = stateStore.Delete("multiball:" + username)
				return
			}

			// Restore session key (bypass callback — already persisted)
			bot.SetSessionKeyDirect(savedKey)

			// Re-wire agent if we can identify it from the session key
			agentID := extractAgentID(savedKey)
			if inst, ok := agents[agentID]; ok {
				bot.SetAgentAndCommands(inst.ag, inst.cmds)
				applyAgentDisplaySettings(bot, inst.agentCfg, cfg)
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

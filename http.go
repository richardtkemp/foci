package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"foci/agent"
	"foci/config"
	"foci/log"
	"foci/session"
	"foci/state"
	"foci/telegram"
	"foci/voice"
)

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

// checkActivityGate checks activity conditions (if_active/if_inactive) and returns false if the request should be skipped.
// Returns true if the request should proceed, false if it should be skipped (response already written).
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
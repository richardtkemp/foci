package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/voice"
)

// agentResolver returns the agent instance for the given ID, or the first agent if empty.
type agentResolver func(agentID string) (*agentInstance, bool)

// activityChecker checks whether a real user has interacted with the agent within a duration.
type activityChecker func(agentID string, within time.Duration) bool

// buildResolvers creates the resolveAgent and isAgentActive helpers from handler deps.
func buildResolvers(d httpHandlerDeps) (agentResolver, activityChecker) {
	resolveAgent := func(agentID string) (*agentInstance, bool) {
		if agentID == "" && len(d.agentOrder) > 0 {
			return d.agents[d.agentOrder[0]], true
		}
		inst, ok := d.agents[agentID]
		return inst, ok
	}

	isAgentActive := func(agentID string, within time.Duration) bool {
		if d.stateStore == nil {
			return true
		}
		var ts int64
		if !d.stateStore.Get("agent/"+agentID+"/last_user_activity", &ts) {
			return false
		}
		return time.Since(time.Unix(ts, 0)) <= within
	}

	return resolveAgent, isAgentActive
}

// handleSend returns the handler for POST /send.
func handleSend(d httpHandlerDeps, resolveAgent agentResolver, isAgentActive activityChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent      string `json:"agent"`
			Session    string `json:"session"`
			Text       string `json:"text"`
			IfActive   string `json:"if_active"`
			IfInactive string `json:"if_inactive"`
			Async      bool   `json:"async"`
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
			// HTTP named sessions are deterministic — same name yields same session
			sessionKey = session.NamedIndependentSessionKey(inst.id, req.Session)
		}
		if sessionKey == "" {
			log.Warnf("http", "POST /send: no default session for agent %q", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		log.Infof("http", "send (agent=%s, session=%s): %s", inst.id, sessionKey, req.Text)

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
			asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, req.Text, "http", false)
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
	}
}

// handleStatus returns the handler for GET /status.
func handleStatus(d httpHandlerDeps, resolveAgent agentResolver) http.HandlerFunc { // nolint:unparam
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

// handleCommand returns the handler for POST /command.
func handleCommand(d httpHandlerDeps, resolveAgent agentResolver) http.HandlerFunc { // nolint:unparam
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

// handleWake returns the handler for POST /wake.
func handleWake(d httpHandlerDeps, resolveAgent agentResolver, isAgentActive activityChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Agent       string `json:"agent"`
			Text        string `json:"text"`
			NoCompact   bool   `json:"no_compact"`
			NoResetHook bool   `json:"no_reset_hook"`
			IfActive    string `json:"if_active"`
			IfInactive  string `json:"if_inactive"`
			Async       bool   `json:"async"`
			Silent      bool   `json:"silent"`
		}
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

		parentKey := inst.defaultSessionKey()
		if parentKey == "" {
			log.Warnf("wake", "no default session for agent %q, skipping", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}
		branchKey, err := session.BranchFromSession(parentKey)
		if err != nil {
			log.Warnf("http", "POST /run-cron-hook create branch key: %v", err)
			http.Error(w, fmt.Sprintf("create branch key: %v", err), http.StatusInternalServerError)
			return
		}

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
			inst.ag.SetSessionNoCompact(branchKey, true)
		}

		if req.Async {
			asyncDispatch(w, inst, d.connMgr, wakeCtx, branchKey, req.Text, "wake", req.Silent)
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
	}
}

// buildVoiceConfig creates the voice.HandlerConfig from handler deps.
func buildVoiceConfig(d httpHandlerDeps) voice.HandlerConfig {
	return voice.HandlerConfig{
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
		STT: resolveSTT(d.sttMap, d.cfg.STT, "", d.cfg.Defaults.STTReplacements),
		AgentTTS: func(agentID string) voice.TTS {
			inst, ok := d.agents[agentID]
			if !ok {
				return resolveTTS(d.ttsMap, d.cfg.TTS, "", 0, d.cfg.Defaults.TTSReplacements)
			}
			ttsRepls := voice.MergeReplacements(d.cfg.Defaults.TTSReplacements, inst.agentCfg.TTSReplacements)
			return resolveTTS(d.ttsMap, d.cfg.TTS, inst.agentCfg.TTS, inst.agentCfg.TTSRate, ttsRepls)
		},
	}
}

// handleReloadCredentials returns the handler for POST /-/reload-credentials.
func handleReloadCredentials(d httpHandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	}
}

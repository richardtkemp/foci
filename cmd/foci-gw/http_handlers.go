package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/voice"
	"foci/shared/prompts"
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
		if d.sessionIndex == nil {
			return true
		}
		raw, err := d.sessionIndex.GetAgentMetadata(agentID, "last_user_activity")
		if err != nil || raw == "" {
			return false
		}
		ts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
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
			Model      string `json:"model"`
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

		if req.Model != "" {
			if err := applyModelOverride(inst, sessionKey, req.Model, d.cfg.Models); err != nil {
				http.Error(w, fmt.Sprintf("bad model: %v", err), http.StatusBadRequest)
				return
			}
		}

		log.Infof("http", "send (agent=%s, session=%s): %s", inst.id, sessionKey, req.Text)

		if strings.HasPrefix(req.Text, "/") {
			cmdReq := command.RequestFromText(req.Text, sessionKey, "", 0)
			if result, ok, _ := inst.cmds.Dispatch(d.ctx, cmdReq, inst.cc); ok {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{"response": result.Text}); err != nil {
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
		cmdReq := command.RequestFromText("/status", inst.defaultSessionKey(), "", 0)
		result, ok, _ := inst.cmds.Dispatch(context.Background(), cmdReq, inst.cc)
		if !ok {
			http.Error(w, "status command not available", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": result.Text}); err != nil {
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
		cmdReq := command.RequestFromText(req.Command, inst.defaultSessionKey(), "", 0)
		result, ok, _ := inst.cmds.Dispatch(context.Background(), cmdReq, inst.cc)
		if !ok {
			http.Error(w, "unknown command", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": result.Text}); err != nil {
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
			Model       string `json:"model"`
			Session     string `json:"session"`
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
		if req.Session != "" {
			parentKey = session.NamedIndependentSessionKey(inst.id, req.Session)
		}
		if parentKey == "" {
			log.Warnf("wake", "no default session for agent %q, skipping", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}
		orientPath := config.DerefStr(config.First(inst.agentCfg.Sessions.BranchOrientationHeadlessPrompt, d.cfg.Sessions.BranchOrientationHeadlessPrompt))
		orientTemplate := prompts.ResolveOrientationTemplate(orientPath, false, inst.promptSearchDirs...)
		branchKey, err := d.sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
			NoResetHook:         req.NoResetHook,
			BranchType:          "cron",
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf("wake", "branch error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if req.Model != "" {
			if err := applyModelOverride(inst, branchKey, req.Model, d.cfg.Models); err != nil {
				http.Error(w, fmt.Sprintf("bad model: %v", err), http.StatusBadRequest)
				return
			}
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
		STT: resolveSTT(d.sttMap, d.cfg.STT, "", d.cfg.Voice.STTReplacements),
		AgentTTS: func(agentID string) voice.TTS {
			inst, ok := d.agents[agentID]
			if !ok {
				return resolveTTS(d.ttsMap, d.cfg.TTS, "", 0, d.cfg.Voice.TTSReplacements)
			}
			vc := inst.resolved.Voice
			ttsRepls := voice.MergeReplacements(d.cfg.Voice.TTSReplacements, inst.agentCfg.Voice.TTSReplacements)
			return resolveTTS(d.ttsMap, d.cfg.TTS, vc.TTS, vc.TTSRate, ttsRepls)
		},
	}
}

// webhookMaxBodyBytes is the maximum request body size for webhook payloads (1 MB).
const webhookMaxBodyBytes = 1 << 20

// handleWebhook returns the handler for POST /webhook/{agent}/{hookid}.
// The hookid must be declared in the agent's webhooks config map, which maps
// hook IDs to prompt file paths. The prompt is resolved via ResolvePrompt,
// the request body is read as a webhook payload, and the combined message
// is sent to the agent. Async (202) by default; ?sync=true for synchronous.
func handleWebhook(d httpHandlerDeps, resolveAgent agentResolver, isAgentActive activityChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse /webhook/{agent}/{hookid} from the URL path.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/webhook/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "bad request: path must be /webhook/{agent}/{hookid}", http.StatusBadRequest)
			return
		}
		agentID, hookID := parts[0], parts[1]

		inst, ok := resolveAgent(agentID)
		if !ok {
			log.Warnf("http", "POST /webhook: unknown agent %q", agentID)
			http.Error(w, fmt.Sprintf("unknown agent: %q", agentID), http.StatusBadRequest)
			return
		}

		// Look up hookID in the agent's configured webhooks.
		promptPath, ok := inst.webhooks[hookID]
		if !ok {
			http.Error(w, fmt.Sprintf("unknown webhook: %q", hookID), http.StatusNotFound)
			return
		}

		q := r.URL.Query()
		if !checkActivityGate(w, inst.id, q.Get("if_active"), q.Get("if_inactive"), isAgentActive, "http", "/webhook") {
			return
		}

		// Resolve the prompt file. Absolute paths read directly; bare filenames
		// search agent workspace/prompts then shared/prompts.
		var resolvedPath, resolvedFilename string
		if filepath.IsAbs(promptPath) {
			resolvedPath = promptPath
			resolvedFilename = filepath.Base(promptPath)
		} else {
			resolvedFilename = promptPath
		}
		promptText := prompts.ResolvePrompt(resolvedPath, resolvedFilename, "", inst.promptSearchDirs...)
		if promptText == "" {
			http.Error(w, fmt.Sprintf("prompt file not found for webhook %q", hookID), http.StatusNotFound)
			return
		}

		// Read webhook payload from request body (capped at 1 MB).
		body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		// Combine prompt + payload.
		var combined string
		payload := strings.TrimSpace(string(body))
		if payload != "" {
			combined = promptText + "\n\n## Webhook Payload\n\n" + payload
		} else {
			combined = promptText
		}

		sessionKey := inst.defaultSessionKey()
		if s := q.Get("session"); s != "" {
			sessionKey = session.NamedIndependentSessionKey(inst.id, s)
		}
		if sessionKey == "" {
			log.Warnf("http", "POST /webhook: no default session for agent %q", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		log.Infof("http", "webhook (agent=%s, hook=%s, payload=%d bytes)", inst.id, hookID, len(payload))

		sendCtx := agent.WithTrigger(d.ctx, "webhook")
		sync := q.Get("sync") == "true"
		if !sync {
			asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, combined, "http", false)
			return
		}

		resp, err := inst.ag.HandleMessage(sendCtx, sessionKey, combined)
		if err != nil {
			log.Errorf("http", "webhook error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"response": resp}); err != nil {
			log.Errorf("http", "encode response: %v", err)
		}
	}
}

// applyModelOverride resolves a model value (group name, alias, or developer/model_id)
// and sets it as a per-session override on the agent instance.
func applyModelOverride(inst *agentInstance, sessionKey, value string, models map[string]config.ModelConfig) error {
	// Check if value is a group name (built-in or user-defined)
	if resolved := inst.ag.GroupResolver.ResolveGroup(value); resolved != nil {
		client := inst.ag.ClientProvider.ResolveEndpointClient(resolved.Endpoint, resolved.Format)
		model := resolved.Developer + "/" + resolved.ModelID
		inst.ag.SetSessionModel(sessionKey, model, resolved.Endpoint, resolved.Format, client)
		return nil
	}

	// Resolve as alias or developer/model_id
	resolved, err := config.ResolveModel(value, "", models)
	if err != nil {
		return err
	}
	model := resolved.Developer + "/" + resolved.ModelID
	client := inst.ag.ClientProvider.ResolveEndpointClient(resolved.Endpoint, resolved.Format)
	inst.ag.SetSessionModel(sessionKey, model, resolved.Endpoint, resolved.Format, client)
	return nil
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

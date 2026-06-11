package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/voice"
	"foci/shared/prompts"
)

// agentResolver returns the agent instance for the given ID, or the first agent if empty.
type agentResolver func(agentID string) (*agentInstance, bool)

// userActivityChecker reports whether a real user has interacted with the agent
// (Telegram/Discord inbound) within the given duration. Reads `last_user_activity`
// from agent_metadata. This is the narrow signal used by --if-user-active /
// --if-user-inactive — independent of any agent turns triggered by cron, CLI,
// webhook, or the agent itself.
type userActivityChecker func(agentID string, within time.Duration) bool

// sessionActivityChecker reports whether the session at the given base has
// executed any turn within the given duration. Reads `last_activity` from
// session_metadata, which is written by OrchestrateFullTurn for every
// turn-init path (TODO #753). This is the broad signal used by --if-active /
// --if-inactive together with the in-flight short-circuit applied at the
// gate site.
type sessionActivityChecker func(sessionBase string, within time.Duration) bool

// activityGateInputs bundles the per-request facts the gate needs to evaluate
// the four activity conditions. AgentID and SessionBase scope the lookups;
// InFlight is the runtime "doing something now" signal computed by the
// handler (Agent.IsTurnInFlight(SessionBase)). The four If* strings are
// duration values from the request body or query string ("" = condition not
// applied).
type activityGateInputs struct {
	AgentID        string
	SessionBase    string
	InFlight       bool
	IfUserActive   string
	IfUserInactive string
	IfActive       string
	IfInactive     string
	LogTag         string
	Endpoint       string
}

// gateEvaluator evaluates an activity gate against inputs and writes the
// skip response if the gate trips. Returns true if the request should
// proceed, false if it has been short-circuited (response already written).
type gateEvaluator func(w http.ResponseWriter, in activityGateInputs) bool

// buildResolvers creates the resolveAgent and gate helpers from handler deps.
// The returned gate closure captures both activity checkers and applies the
// four-condition gate logic; handlers compute SessionBase + InFlight and
// pass them in via activityGateInputs.
func buildResolvers(d httpHandlerDeps) (agentResolver, gateEvaluator) {
	resolveAgent := func(agentID string) (*agentInstance, bool) {
		if agentID == "" && len(d.agentOrder) > 0 {
			return d.agents[d.agentOrder[0]], true
		}
		inst, ok := d.agents[agentID]
		return inst, ok
	}

	isUserActive := func(agentID string, within time.Duration) bool {
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

	isSessionActive := func(sessionBase string, within time.Duration) bool {
		if d.sessionIndex == nil || sessionBase == "" {
			return false
		}
		raw, err := d.sessionIndex.GetSessionMetadata(sessionBase, "last_activity")
		if err != nil || raw == "" {
			return false
		}
		ts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return false
		}
		return time.Since(time.Unix(ts, 0)) <= within
	}

	gate := func(w http.ResponseWriter, in activityGateInputs) bool {
		return checkActivityGate(w, in, isUserActive, isSessionActive)
	}

	return resolveAgent, gate
}

// handleSend returns the handler for POST /send.
func handleSend(d httpHandlerDeps, resolveAgent agentResolver, gate gateEvaluator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent          string `json:"agent"`
			Session        string `json:"session"`
			Text           string `json:"text"`
			Model          string `json:"model"`
			IfUserActive   string `json:"if_user_active"`
			IfUserInactive string `json:"if_user_inactive"`
			IfActive       string `json:"if_active"`
			IfInactive     string `json:"if_inactive"`
			Async          bool   `json:"async"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, jsonMaxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			if bodyTooLarge(err) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request: need {\"text\": \"...\"}", http.StatusBadRequest)
			return
		}

		inst, ok := resolveAgent(req.Agent)
		if !ok {
			log.Warnf("http", "POST /send: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}

		// Resolve session before gating so the activity gate can consult
		// last_activity / IsTurnInFlight for the session this request
		// actually targets (TODO #753 — per-session granularity).
		sessionKey := mostRecentSessionKey(inst.ag, d.connMgr, inst.id)
		if req.Session != "" {
			// HTTP named sessions are deterministic — same name yields same session
			sk, err := session.NamedIndependentSessionKey(inst.id, req.Session)
			if err != nil {
				log.Warnf("http", "POST /send: %v", err)
				http.Error(w, fmt.Sprintf("invalid session name: %v", err), http.StatusBadRequest)
				return
			}
			sessionKey = sk
		}
		if sessionKey == "" {
			log.Warnf("http", "POST /send: no default session for agent %q", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		sessionBase := session.SessionKeyBase(sessionKey)
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    sessionBase,
			InFlight:       inst.ag.IsTurnInFlight(sessionBase),
			IfUserActive:   req.IfUserActive,
			IfUserInactive: req.IfUserInactive,
			IfActive:       req.IfActive,
			IfInactive:     req.IfInactive,
			LogTag:         "http",
			Endpoint:       "/send",
		}) {
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
			cmdCtx := tools.WithSessionKey(d.ctx, sessionKey)
			if result, ok, _ := inst.cmds.Dispatch(cmdCtx, cmdReq, inst.cc); ok {
				writeJSONResponse(w, result.Text)
				return
			}
		}

		sendCtx := agent.WithTrigger(d.ctx, "user")
		if req.Async {
			asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, req.Text, "http", false)
			return
		}

		resp, err := runAgentBuffered(sendCtx, inst.ag, sessionKey, req.Text)
		if err != nil {
			log.Errorf("http", "send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, resp)
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
		sk := mostRecentSessionKey(inst.ag, d.connMgr, inst.id)
		cmdReq := command.RequestFromText("/status", sk, "", 0)
		result, ok, _ := inst.cmds.Dispatch(tools.WithSessionKey(r.Context(), sk), cmdReq, inst.cc)
		if !ok {
			http.Error(w, "status command not available", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, result.Text)
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
		r.Body = http.MaxBytesReader(w, r.Body, jsonMaxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
			if bodyTooLarge(err) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request: need {\"command\": \"/ping\"}", http.StatusBadRequest)
			return
		}
		inst, ok := resolveAgent(req.Agent)
		if !ok {
			log.Warnf("http", "POST /command: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		sk := mostRecentSessionKey(inst.ag, d.connMgr, inst.id)
		cmdReq := command.RequestFromText(req.Command, sk, "", 0)
		result, ok, _ := inst.cmds.Dispatch(tools.WithSessionKey(r.Context(), sk), cmdReq, inst.cc)
		if !ok {
			http.Error(w, "unknown command", http.StatusNotFound)
			return
		}
		if result.DocPath != "" {
			if conn := d.connMgr.ForSessionOrPrimary(sk, inst.id); conn != nil {
				if err := conn.SendDocument(result.DocPath, ""); err != nil {
					log.Warnf("http", "POST /command: send document: %v", err)
				}
			}
			_ = os.Remove(result.DocPath)
		}
		writeJSONResponse(w, result.Text)
	}
}

// handleWake returns the handler for POST /wake.
func handleWake(d httpHandlerDeps, resolveAgent agentResolver, gate gateEvaluator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Agent          string `json:"agent"`
			Text           string `json:"text"`
			Model          string `json:"model"`
			Session        string `json:"session"`
			NoCompact      bool   `json:"no_compact"`
			NoResetHook    bool   `json:"no_reset_hook"`
			IfUserActive   string `json:"if_user_active"`
			IfUserInactive string `json:"if_user_inactive"`
			IfActive       string `json:"if_active"`
			IfInactive     string `json:"if_inactive"`
			Async          bool   `json:"async"`
			Silent         bool   `json:"silent"`
		}
		if r.ContentLength > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, jsonMaxBodyBytes)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				if bodyTooLarge(err) {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
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

		if req.Text == "" {
			req.Text = "[WAKE]"
		}

		// Resolve parent session before gating so the activity gate can
		// consult the parent's last_activity / IsTurnInFlight. Branches
		// share the parent's SessionKeyBase, so a turn running in any
		// branch correctly registers as in-flight under the parent.
		parentKey := mostRecentSessionKey(inst.ag, d.connMgr, inst.id)
		if req.Session != "" {
			pk, err := session.NamedIndependentSessionKey(inst.id, req.Session)
			if err != nil {
				log.Warnf("wake", "POST /wake: %v", err)
				http.Error(w, fmt.Sprintf("invalid session name: %v", err), http.StatusBadRequest)
				return
			}
			parentKey = pk
		}
		if parentKey == "" {
			log.Warnf("wake", "no default session for agent %q, skipping", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		parentBase := session.SessionKeyBase(parentKey)
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    parentBase,
			InFlight:       inst.ag.IsTurnInFlight(parentBase),
			IfUserActive:   req.IfUserActive,
			IfUserInactive: req.IfUserInactive,
			IfActive:       req.IfActive,
			IfInactive:     req.IfInactive,
			LogTag:         "wake",
			Endpoint:       "/wake",
		}) {
			return
		}

		// Delegated agents (e.g. Claude Code backend) don't support
		// /wake's branching semantics — CC owns its session lifecycle and
		// foci can't fork it. Fall through to /send semantics: deliver
		// the text to the parent session directly. We log a warning so
		// callers know the requested isolation (no_compact, no_reset_hook,
		// silent, fresh-branch context) did not happen.
		if inst.ag.DelegatedManager != nil {
			log.Warnf("wake", "agent %q is delegated — falling through to send (branching options ignored: no_compact=%v no_reset_hook=%v silent=%v)", inst.id, req.NoCompact, req.NoResetHook, req.Silent)
			if req.Model != "" {
				if err := applyModelOverride(inst, parentKey, req.Model, d.cfg.Models); err != nil {
					http.Error(w, fmt.Sprintf("bad model: %v", err), http.StatusBadRequest)
					return
				}
			}
			sendCtx := agent.WithTrigger(d.ctx, "wake")
			if req.Async {
				asyncDispatch(w, inst, d.connMgr, sendCtx, parentKey, req.Text, "wake", req.Silent)
				return
			}
			resp, err := runAgentBuffered(sendCtx, inst.ag, parentKey, req.Text)
			if err != nil {
				log.Errorf("wake", "send fallback error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			writeJSONResponse(w, resp)
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

		resp, err := runAgentBuffered(wakeCtx, inst.ag, branchKey, req.Text)
		if err != nil {
			log.Errorf("wake", "error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, resp)
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
			return runAgentBuffered(agent.WithTrigger(msgCtx, "voice"), inst.ag, sessionKey, text)
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
		MaxFrameBytes:      int64(intPtrOr(d.cfg.Voice.MaxFrameBytes, config.DefaultVoiceMaxFrameBytes)),
		MaxAudioBytes:      intPtrOr(d.cfg.Voice.MaxAudioBytes, config.DefaultVoiceMaxAudioBytes),
		MaxConcurrentTurns: intPtrOr(d.cfg.Voice.MaxConcurrentTurns, config.DefaultVoiceMaxConcurrentTurns),
	}
}

// intPtrOr dereferences p, falling back to def when p is nil. Used to resolve
// optional pointer config values that ApplyTagDefaults normally fills but a
// directly-constructed config may leave unset.
func intPtrOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

// webhookMaxBodyBytes is the maximum request body size for webhook payloads (1 MB).
const webhookMaxBodyBytes = 1 << 20

// jsonMaxBodyBytes caps the request body of the JSON control endpoints
// (/send, /command, /wake). These carry short control messages, so 1 MB is
// generous; the cap stops an unbounded body from being buffered into memory.
const jsonMaxBodyBytes = 1 << 20

// bodyTooLarge reports whether err is the http.MaxBytesReader overflow error,
// so a handler can answer 413 instead of a generic 400.
func bodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// handleWebhook returns the handler for POST /webhook/{agent}/{hookid}.
// The hookid must be declared in the agent's webhooks config map, which maps
// hook IDs to prompt file paths. The prompt is resolved via ResolvePrompt,
// the request body is read as a webhook payload, and the combined message
// is sent to the agent. Async (202) by default; ?sync=true for synchronous.
func handleWebhook(d httpHandlerDeps, resolveAgent agentResolver, gate gateEvaluator) http.HandlerFunc {
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

		// Resolve session before gating so the activity gate can consult
		// last_activity / IsTurnInFlight for the targeted session.
		webhookSessionKey := mostRecentSessionKey(inst.ag, d.connMgr, inst.id)
		if s := q.Get("session"); s != "" {
			wk, err := session.NamedIndependentSessionKey(inst.id, s)
			if err != nil {
				log.Warnf("http", "POST /webhook: %v", err)
				http.Error(w, fmt.Sprintf("invalid session name: %v", err), http.StatusBadRequest)
				return
			}
			webhookSessionKey = wk
		}
		if webhookSessionKey == "" {
			log.Warnf("http", "POST /webhook: no default session for agent %q", inst.id)
			http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
			return
		}

		webhookSessionBase := session.SessionKeyBase(webhookSessionKey)
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    webhookSessionBase,
			InFlight:       inst.ag.IsTurnInFlight(webhookSessionBase),
			IfUserActive:   q.Get("if_user_active"),
			IfUserInactive: q.Get("if_user_inactive"),
			IfActive:       q.Get("if_active"),
			IfInactive:     q.Get("if_inactive"),
			LogTag:         "http",
			Endpoint:       "/webhook",
		}) {
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

		// Reuse the session resolved before the gate.
		sessionKey := webhookSessionKey

		log.Infof("http", "webhook (agent=%s, hook=%s, payload=%d bytes)", inst.id, hookID, len(payload))

		sendCtx := agent.WithTrigger(d.ctx, "webhook")
		sync := q.Get("sync") == "true"
		if !sync {
			asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, combined, "http", false)
			return
		}

		resp, err := runAgentBuffered(sendCtx, inst.ag, sessionKey, combined)
		if err != nil {
			log.Errorf("http", "webhook error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, resp)
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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/defersend"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/voice"
	"foci/shared/prompts"
)

// logBodyPreviewChars bounds how much of a prompt body previewForLog keeps.
const logBodyPreviewChars = 120

// previewForLog returns a short, bounded, single-line preview of a prompt
// body for a service-log line — never the full text. #1838: the "[http]
// send" INFO line used to write the entire body inline, unbounded; a routine
// window-grep of foci.log (the standard first move in almost every
// investigation) could pull KBs of private prompt content into an unrelated
// agent's context purely by substring coincidence. Keeping a short prefix
// plus the total byte length preserves greppability/auditability without
// turning the service log into a message store — the full text still lives
// wherever payloads are already logged (e.g. conversation.db).
func previewForLog(body string) string {
	if utf8.RuneCountInString(body) <= logBodyPreviewChars {
		return fmt.Sprintf("%dB %q", len(body), body)
	}
	runes := []rune(body)
	return fmt.Sprintf("%dB %q…", len(body), string(runes[:logBodyPreviewChars]))
}

// agentResolver returns the agent instance for the given ID, or the first agent if empty.
type agentResolver func(agentID string) (*agentInstance, bool)

// userActivityChecker reports whether a real user has interacted with the agent
// within the given duration. Reads the derived max of session_index
// last_user_activity_at (written only on real-time interactive turns —
// telegram/app/discord/voice — or an HTTP request declared human). This is
// the narrow signal used by --if-user-active / --if-user-inactive —
// independent of any agent turns triggered by /send, cron, webhook, or the
// agent itself.
type userActivityChecker func(agentID string, within time.Duration) bool

// sessionActivityChecker reports whether the session at the given base has had
// its cached context touched by any turn within the given duration. Reads
// session_index.last_cache_touch, bumped by OrchestrateFullTurn on every
// turn-init path (memory turns included — they warm the cache). This is the
// broad cache-freshness signal used by --if-active / --if-inactive together
// with the in-flight short-circuit applied at the gate site.
type sessionActivityChecker func(sessionBase string, within time.Duration) bool

// activityGateInputs bundles the per-request facts the gate needs to evaluate
// the four activity conditions. AgentID and SessionBase scope the lookups;
// InFlight is the runtime "doing something now" signal computed by the
// handler (Agent.IsTurnInFlight(SessionBase)). The four If* strings are
// duration values from the request body or query string ("" = condition not
// applied).
type activityGateInputs struct {
	AgentID     string
	SessionBase string
	InFlight    bool
	// LastTurnEnd is the wall-clock moment the most recent turn on SessionBase
	// finished (Agent.LastTurnEnd) — zero if none since process start. Together
	// with InFlight it measures CONTINUOUS dead time: a session whose last turn
	// ended within the wait window still counts as active, so a deferred send is
	// not released into the gap between back-to-back turns. Zero value is
	// ignored (falls back to the durable last_cache_touch signal).
	LastTurnEnd    time.Time
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

	isUserActive, isSessionActive := buildActivityCheckers(d)
	gate := func(w http.ResponseWriter, in activityGateInputs) bool {
		return checkActivityGate(w, in, isUserActive, isSessionActive)
	}

	return resolveAgent, gate
}

// buildActivityCheckers returns the user- and session-activity probes used by
// both the one-shot if-gate and the wait evaluator. Both are scoped to the
// RESOLVED target session (not the agent-wide max): "did a human/turn touch
// THIS session recently", matching what the caller is about to send to.
func buildActivityCheckers(d httpHandlerDeps) (userActivityChecker, sessionActivityChecker) {
	isUserActive := func(sessionKey string, within time.Duration) bool {
		if d.sessionIndex == nil {
			return true
		}
		if sessionKey == "" {
			return false
		}
		last, ok := d.sessionIndex.LastUserActivity(sessionKey)
		if !ok {
			return false
		}
		return time.Since(last) <= within
	}
	isSessionActive := func(sessionBase string, within time.Duration) bool {
		if d.sessionIndex == nil || sessionBase == "" {
			return false
		}
		touched, ok := d.sessionIndex.LastCacheTouch(sessionBase)
		if !ok {
			return false
		}
		return time.Since(touched) <= within
	}
	return isUserActive, isSessionActive
}

// notifyHumanInteraction stamps the agent's in-process interaction receipt for
// a human-declared HTTP request (#1130) — the same stamp the platforms'
// OnUserMessage hook leaves — so the request counts as attention for the
// periodic idle checks even when its turn is queued behind in-flight work or
// it starts no turn at all. It belongs to the DISPATCH boundary: call it only
// once the request is actually running or queued, never while a gate or a
// validation error can still refuse it — a request that fails to dispatch is
// not user attention. No-op when the agent has no periodic runner (the HTTP
// test harness wires none).
func notifyHumanInteraction(inst *agentInstance) {
	if inst.kaRunner != nil {
		inst.kaRunner.NotifyInteraction()
	}
}

// stampHumanCommandDispatch stamps BOTH user-activity receipts for a
// human-declared request whose dispatch starts no turn — a /command dispatch,
// or a /send whose text is a slash command (#1130): no turn ever runs for
// them, so nothing else would write last_user_activity_at for the session,
// and the in-process receipt mirrors what a platform slash command leaves
// (the app's command path fires OnUserMessage too). Call it only once a
// command actually HANDLED the request — a name the registry answers still
// counts, a registered command that fails to dispatch does not. No-op with
// no session index or an empty key.
func stampHumanCommandDispatch(d httpHandlerDeps, inst *agentInstance, sessionKey string) {
	notifyHumanInteraction(inst)
	if d.sessionIndex != nil && sessionKey != "" {
		d.sessionIndex.TouchUserActivity(sessionKey, time.Now())
	}
}

// resolveTargetSession resolves an endpoint's (agent, session-selector) pair
// through the single route.Resolver ladder, writing the appropriate HTTP error
// on failure. Every endpoint that takes a session selector resolves here, so
// /send, /branch, /command, and /webhook behave identically: exact key → named
// session → chat alias → create-named; empty selector → the agent's default
// session. Returns ok=false after an error response has been written.
func resolveTargetSession(d httpHandlerDeps, w http.ResponseWriter, agentID, selector, policy, endpoint string) (route.Resolution, route.Receipt, bool) {
	// CreateDefault is set here and nowhere else in the HTTP layer: /send,
	// /branch and /webhook are delivery paths, so an agent whose every
	// conversation is archived gets one minted rather than a 412 (#1859).
	r := &route.Resolver{Index: d.sessionIndex, PreferredPlatform: d.cfg.DefaultPlatformFor, CreateDefault: d.createDefault}
	t := route.Target{Agent: agentID, Rest: selector, Create: true, Policy: route.PolicyFallback}
	if strings.Contains(selector, "?") {
		// Selector may carry embedded params (create=/policy=) — parse the
		// full canonical target form. A selector that literally names a chat
		// alias keeps its '?' (#2158).
		parsed, err := r.ParseTarget(agentID + "/" + selector)
		if err != nil {
			httpLog.Warnf("POST %s: %v", endpoint, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return route.Resolution{}, route.Receipt{}, false
		}
		t = parsed
	}
	if policy != "" {
		// An explicit request-level policy field overrides.
		p, err := route.ParsePolicy(policy)
		if err != nil {
			httpLog.Warnf("POST %s: %v", endpoint, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return route.Resolution{}, route.Receipt{}, false
		}
		t.Policy = p
	}
	res, err := r.Resolve(t)
	if err == nil {
		rcpt := res.ReceiptFor(t)
		if t.Policy != route.PolicyFallback {
			rcpt.Policy = string(t.Policy)
		}
		return res, rcpt, true
	}
	httpLog.Warnf("POST %s: %v", endpoint, err)
	switch {
	case errors.Is(err, route.ErrNoSession):
		http.Error(w, "no active session — send a message to the bot first", http.StatusPreconditionFailed)
	case errors.Is(err, session.ErrAliasAmbiguous):
		http.Error(w, fmt.Sprintf("chat alias %q matches multiple chats — rename to disambiguate", selector), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
	return route.Resolution{}, route.Receipt{}, false
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
			Policy         string `json:"policy"` // strict | fallback | broadcast (delivery policy)
			Text           string `json:"text"`
			Model          string `json:"model"`
			Human          bool   `json:"human"` // #1130: caller declares a human sent this
			IfUserActive   string `json:"if_user_active"`
			IfUserInactive string `json:"if_user_inactive"`
			IfActive       string `json:"if_active"`
			IfInactive     string `json:"if_inactive"`
			waitRequest
			Async bool `json:"async"`
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
			httpLog.Warnf("POST /send: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}

		// Resolve session before gating so the activity gate can consult
		// last_activity / IsTurnInFlight for the session this request
		// actually targets (TODO #753 — per-session granularity).
		res, rcpt, ok := resolveTargetSession(d, w, inst.id, req.Session, req.Policy, "/send")
		if !ok {
			return
		}
		sessionKey := res.SessionKey

		sessionBase := sessionKey
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    sessionBase,
			InFlight:       inst.ag.IsTurnInFlight(sessionBase),
			LastTurnEnd:    inst.ag.LastTurnEnd(sessionBase),
			IfUserActive:   req.IfUserActive,
			IfUserInactive: req.IfUserInactive,
			IfActive:       req.IfActive,
			IfInactive:     req.IfInactive,
			LogTag:         "http",
			Endpoint:       "/send",
		}) {
			return
		}

		// Wait/defer gate. Unless the caller opts out (wait_none) or the caller
		// gave no gate flag at all — in which case /send defaults to wait_cold=1m
		// to avoid interleaving with an active session — a wait condition that
		// does not hold now enqueues the send for later delivery by the sweep
		// (persisted, restart-surviving) instead of blocking or dropping.
		// This default is /send-only: /branch and /command run now when no
		// wait field is given (#1272).
		wc := req.conds()
		noIfGate := req.IfActive == "" && req.IfInactive == "" && req.IfUserActive == "" && req.IfUserInactive == ""
		if !wc.none && !wc.any() && noIfGate {
			wc.cold = "1m"
		}
		deferredRec := defersend.Record{
			Kind: defersend.KindSend, AgentID: inst.id, SessionKey: sessionKey,
			Text: req.Text, Policy: string(res.Policy), Model: req.Model,
		}

		// Rate-limit gate (#1417): a session whose endpoint is currently
		// rate-limited must never be dispatched now — that would be a
		// guaranteed-fail API call and, for an async send, a silently dropped
		// message (deliverBufferedQueued just logs "async error" and returns).
		// Unlike the activity conditions below, this is a hard capacity
		// constraint rather than a scheduling preference, so it applies even
		// under wait_none/--no-gate. It folds into the SAME persisted
		// defer-then-sweep mechanism as the activity wait gates
		// (enqueueDeferred / deferSweeper.sweep, which independently
		// withholds delivery while the gate stays closed) — the existing 10s
		// sweep tick becomes the "dispatch one at a time once the gate opens"
		// replay for /send, mirroring the rate limit gate's own per-endpoint
		// queue used for system-triggered work.
		if limited, reason := inst.ag.SessionRateLimited(sessionKey); limited {
			if d.deferStore != nil {
				enqueueDeferred(w, d, deferredRec, wc, rcpt)
				return
			}
			// Store unavailable: degrade to immediate send (a user-trigger
			// recovery probe) rather than failing outright.
			deferLog.Warnf("%s but defer store unavailable — sending now (agent=%s session=%s)", reason, inst.id, sessionKey)
		}

		if deferUnmetWait(w, d, activityGateInputs{
			AgentID:     inst.id,
			SessionBase: sessionBase,
			InFlight:    inst.ag.IsTurnInFlight(sessionBase),
			LastTurnEnd: inst.ag.LastTurnEnd(sessionBase),
		}, wc, deferredRec, rcpt) {
			return
		}

		if req.Model != "" {
			if err := applyModelOverride(inst, sessionKey, req.Model, d.cfg.Models); err != nil {
				http.Error(w, fmt.Sprintf("bad model: %v", err), http.StatusBadRequest)
				return
			}
		}

		// #1130: a human declaration's receipts start here, at the dispatch
		// boundary — every gate has passed and the request runs now. Every
		// earlier return (skip, defer, rate limit, bad model) records
		// nothing, so a request's own declaration can never satisfy or block
		// its own if_user_*/wait_user_* gate, which was read earlier in this
		// same invocation. A request refused before its turn starts (full
		// inbox) records nothing. A turn that starts and then fails has
		// already stamped the durable last_user_activity_at at turn entry
		// (recordTurnActivity, before inference); only the in-process receipt
		// below is skipped (#2237).

		httpLog.Infof("send (agent=%s, session=%s): %s", inst.id, sessionKey, previewForLog(req.Text))

		if strings.HasPrefix(req.Text, "/") {
			cmdReq := command.RequestFromText(req.Text, sessionKey, "", 0)
			cmdCtx := tools.WithSessionKey(d.ctx, sessionKey)
			if result, ok, _ := inst.cmds.Dispatch(cmdCtx, cmdReq, inst.cc); ok {
				// A dispatched slash command starts no turn, so nothing else
				// would stamp either receipt — write both here (#1130).
				if req.Human {
					stampHumanCommandDispatch(d, inst, sessionKey)
				}
				writeJSONReceipt(w, result.Text, rcpt)
				return
			}
		}

		app.DeliverExternalPrompt(sessionKey, req.Text)

		sendCtx := agent.WithTrigger(d.ctx, "user")
		if req.Human {
			// The turn's entry write (recordTurnActivity) reads this marker
			// and bumps last_user_activity_at on the turn's own session key —
			// for sync AND async sends alike, both derive from sendCtx.
			sendCtx = agent.WithHumanSource(sendCtx)
		}
		// NOTE (#1385): this is the sync/async fork for /send delivery shape.
		// async → asyncDispatch → deliverBufferedQueued streams to the resolved
		// chat AS THE TURN RUNS (typing indicator + StreamingSink/SessionSink —
		// see deliverBufferedQueued's doc comment in http.go). sync (below) →
		// runAgentQueued stays on the buffered path: it returns the turn's
		// FinalText as this HTTP call's response body and does NOT also push it
		// to the chat (no PolicyBroadcast) — a blocking API caller is asking for
		// a value back, not asking the chat to show anything, so there's no
		// live surface to stream into here by default. If we ever want sync to
		// BOTH return the text to the caller AND stream/show it in chat, the way
		// to do that is a tee sink (a BufferSink for the return value +
		// StreamingSink/SessionSink for chat delivery, run off the same turn) —
		// deliberately not built now (Dick, #1385: skip the tee).
		if req.Async {
			// #1130: stamp the in-process receipt only once the request is
			// actually queued — a full inbox refused it and nothing ran. The
			// durable timestamp stays the queued turn's own entry write.
			if asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, req.Text, "http", false, res.Policy, rcpt) && req.Human {
				notifyHumanInteraction(inst)
			}
			return
		}

		resp, err := runAgentQueued(sendCtx, inst.ag, sessionKey, req.Text)
		if err != nil {
			httpLog.Errorf("send error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// #1130: the turn ran — its entry write already stamped the durable
		// timestamp via the marker; stamp the in-process receipt alongside.
		if req.Human {
			notifyHumanInteraction(inst)
		}
		// PolicyBroadcast: the caller gets the response in the body AND every
		// live surface for the agent gets it delivered.
		if res.Policy == route.PolicyBroadcast {
			if cleaned := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(resp)); cleaned != "" {
				broadcastResponse(d.connMgr, inst.id, sessionKey, cleaned, "http")
			}
		}
		writeJSONReceipt(w, resp, rcpt)
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
			httpLog.Warnf("GET /status: unknown agent %q", agentID)
			http.Error(w, fmt.Sprintf("unknown agent: %q", agentID), http.StatusBadRequest)
			return
		}
		sk := defaultSessionKey(d, inst.id)
		cmdReq := command.RequestFromText("/status", sk, "", 0)
		result, ok, _ := inst.cmds.Dispatch(tools.WithSessionKey(r.Context(), sk), cmdReq, inst.cc)
		if !ok {
			http.Error(w, "status command not available", http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, result.Text)
	}
}

// dispatchAgentCommand runs one slash command through the agent's registry —
// the single dispatch site shared by handleCommand (immediate) and the
// deferred sweep (delivery time), DocPath platform send included. ok is false
// only for a registered command with no Execute function; unknown names are
// answered by the registry itself (found=true, "Unknown command" text).
func dispatchAgentCommand(d httpHandlerDeps, inst *agentInstance, ctx context.Context, sessionKey, text string) (command.Response, bool) {
	cmdReq := command.RequestFromText(text, sessionKey, "", 0)
	result, ok, _ := inst.cmds.Dispatch(tools.WithSessionKey(ctx, sessionKey), cmdReq, inst.cc)
	if !ok {
		return result, false
	}
	if err := platform.SendDocAndRemove(d.connMgr.ForSessionOrPrimary(sessionKey, inst.id), 0, result.DocPath, ""); err != nil {
		httpLog.Warnf("command dispatch: send document: %v", err)
	}
	return result, true
}

// handleCommand returns the handler for POST /command.
func handleCommand(d httpHandlerDeps, resolveAgent agentResolver, gate gateEvaluator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent          string `json:"agent"`
			Command        string `json:"command"`
			Human          bool   `json:"human"` // #1130: caller declares a human sent this
			IfUserActive   string `json:"if_user_active"`
			IfUserInactive string `json:"if_user_inactive"`
			IfActive       string `json:"if_active"`
			IfInactive     string `json:"if_inactive"`
			waitRequest
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
			httpLog.Warnf("POST /command: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		sk := defaultSessionKey(d, inst.id)

		// Activity gate (TODO #753): a gated command — e.g. an overnight
		// `/reset` cron with --if-inactive — is skipped when the target
		// session ran a turn recently or has one in flight. Gating here, on
		// the session this command actually targets, keeps it from
		// interrupting active or mid-turn work.
		sessionBase := sk
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    sessionBase,
			InFlight:       inst.ag.IsTurnInFlight(sessionBase),
			LastTurnEnd:    inst.ag.LastTurnEnd(sessionBase),
			IfUserActive:   req.IfUserActive,
			IfUserInactive: req.IfUserInactive,
			IfActive:       req.IfActive,
			IfInactive:     req.IfInactive,
			LogTag:         "http",
			Endpoint:       "/command",
		}) {
			return
		}

		// Wait/defer gate (#1272): unlike /send there is NO default — a
		// command with no wait field dispatches now, exactly as before. An
		// unmet wait defers the command (persisted, restart-surviving) for
		// the sweep to dispatch once the condition holds; the caller gets the
		// 202 deferred receipt instead of a dispatch.
		if deferUnmetWait(w, d, activityGateInputs{
			AgentID:     inst.id,
			SessionBase: sessionBase,
			InFlight:    inst.ag.IsTurnInFlight(sessionBase),
			LastTurnEnd: inst.ag.LastTurnEnd(sessionBase),
		}, req.conds(), defersend.Record{
			Kind: defersend.KindCommand, AgentID: inst.id, SessionKey: sk, Text: req.Command,
		}, route.Receipt{SessionKey: sk}) {
			return
		}

		result, ok := dispatchAgentCommand(d, inst, r.Context(), sk, req.Command)
		if !ok {
			http.Error(w, "unknown command", http.StatusNotFound)
			return
		}

		// #1130: a command starts no turn, so a human declaration stamps both
		// receipts itself. Only now: after every gate passed AND a command
		// actually handled the request, so a skipped, deferred or
		// unexecutable command records nothing.
		if req.Human {
			stampHumanCommandDispatch(d, inst, sk)
		}
		writeJSONResponse(w, result.Text)
	}
}

// handleBranch returns the handler for POST /branch.
func handleBranch(d httpHandlerDeps, resolveAgent agentResolver, gate gateEvaluator) http.HandlerFunc {
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
			Human          bool   `json:"human"` // #1130: caller declares a human sent this
			NoCompact      bool   `json:"no_compact"`
			NoResetHook    bool   `json:"no_reset_hook"`
			IfUserActive   string `json:"if_user_active"`
			IfUserInactive string `json:"if_user_inactive"`
			IfActive       string `json:"if_active"`
			IfInactive     string `json:"if_inactive"`
			waitRequest
			Async  bool `json:"async"`
			Silent bool `json:"silent"`
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
			httpLog.Warnf("POST /branch: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}

		if req.Text == "" {
			req.Text = "[BRANCH]"
		}

		// Resolve parent session before gating so the activity gate can
		// consult the parent's last_activity / IsTurnInFlight. Branch turns
		// record last_activity against their parent root, so a turn running
		// in any branch correctly registers as activity under the parent.
		branchRes, branchRcpt, ok := resolveTargetSession(d, w, inst.id, req.Session, "", "/branch")
		if !ok {
			return
		}
		parentKey := branchRes.SessionKey

		parentBase := parentKey
		gateIn := activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    parentBase,
			InFlight:       inst.ag.IsTurnInFlight(parentBase),
			LastTurnEnd:    inst.ag.LastTurnEnd(parentBase),
			IfUserActive:   req.IfUserActive,
			IfUserInactive: req.IfUserInactive,
			IfActive:       req.IfActive,
			IfInactive:     req.IfInactive,
			LogTag:         "branch",
			Endpoint:       "/branch",
		}
		if !gate(w, gateIn) {
			return
		}

		// Wait/defer gate (#1272): unlike /send there is NO default — a
		// branch with no wait field runs now, exactly as before. An unmet
		// wait (evaluated against the resolved PARENT session) stores the
		// request and the sweep creates the branch once the condition holds —
		// forking from the parent's state at DELIVERY time. Deferred requests
		// are inherently async: --sync callers get the 202 deferred receipt
		// now too.
		if deferUnmetWait(w, d, gateIn, req.conds(), defersend.Record{
			Kind: defersend.KindBranch, AgentID: inst.id, SessionKey: parentKey, Text: req.Text,
			Model: req.Model, NoCompact: req.NoCompact, NoResetHook: req.NoResetHook, Silent: req.Silent,
		}, branchRcpt) {
			return
		}

		// #1130: a human declaration takes effect only now — every gate has
		// passed and the branch runs. The durable timestamp is written by the
		// branch turn's own entry write (runBranchTurn carries the human flag
		// to the turn context), landing on the NEW branch session key; the
		// in-process receipt waits for the run below, so a bad model, fork
		// error or full inbox stamps nothing.
		result, err := runBranchTurn(d, inst, parentKey, branchRcpt, branchTurnOptions{
			Text: req.Text, Model: req.Model,
			NoCompact: req.NoCompact, NoResetHook: req.NoResetHook, Silent: req.Silent,
		}, !req.Async, req.Human)
		if err == nil {
			if req.Human {
				notifyHumanInteraction(inst)
			}
			if req.Async {
				writeAccepted(w, result.Receipt)
				return
			}
			writeJSONReceipt(w, result.Resp, result.Receipt)
			return
		}
		switch {
		case errors.Is(err, errBranchBadModel):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, errBranchInboxFull):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			// createBranchSession / runBranchTurn already logged the cause.
			http.Error(w, "internal error", http.StatusInternalServerError)
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
			vc := inst.LiveConfig().Voice
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
// (/send, /command, /branch). These carry short control messages, so 1 MB is
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
			httpLog.Warnf("POST /webhook: unknown agent %q", agentID)
			http.Error(w, fmt.Sprintf("unknown agent: %q", agentID), http.StatusBadRequest)
			return
		}

		// Look up hookID in the agent's configured webhooks.
		promptPath, ok := inst.LiveConfig().Webhooks[hookID]
		if !ok {
			http.Error(w, fmt.Sprintf("unknown webhook: %q", hookID), http.StatusNotFound)
			return
		}

		q := r.URL.Query()

		// A webhook is an external event source, not a human conversation — its
		// turns must not land in the user's chat. Absent an explicit ?session=,
		// default to a per-hook INDEPENDENT session (agent/i<hookID>), created
		// lazily and classified as session_type "independent". An explicit
		// ?session= still wins (e.g. to route a hook into a named session).
		sessionSel := q.Get("session")
		if sessionSel == "" {
			sessionSel = hookID
		}

		// Resolve session before gating so the activity gate can consult
		// last_activity / IsTurnInFlight for the targeted session.
		webhookRes, webhookRcpt, ok := resolveTargetSession(d, w, inst.id, sessionSel, "", "/webhook")
		if !ok {
			return
		}
		webhookSessionKey := webhookRes.SessionKey

		webhookSessionBase := webhookSessionKey
		if !gate(w, activityGateInputs{
			AgentID:        inst.id,
			SessionBase:    webhookSessionBase,
			InFlight:       inst.ag.IsTurnInFlight(webhookSessionBase),
			LastTurnEnd:    inst.ag.LastTurnEnd(webhookSessionBase),
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

		httpLog.Infof("webhook (agent=%s, hook=%s, payload=%d bytes)", inst.id, hookID, len(payload))

		sendCtx := agent.WithTrigger(d.ctx, "webhook")
		sync := q.Get("sync") == "true"
		if !sync {
			asyncDispatch(w, inst, d.connMgr, sendCtx, sessionKey, combined, "http", false, route.PolicyFallback, webhookRcpt)
			return
		}

		resp, err := runAgentQueued(sendCtx, inst.ag, sessionKey, combined)
		if err != nil {
			httpLog.Errorf("webhook error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSONReceipt(w, resp, webhookRcpt)
	}
}

// applyModelOverride resolves a model value (group name, alias, or developer/model_id)
// and sets it as a per-session override on the agent instance.
func applyModelOverride(inst *agentInstance, sessionKey, value string, models map[string]config.ModelConfig) error {
	// Per-session model override is an API-agent feature: it resolves through the
	// model groups / aliases and swaps the provider client. Delegated
	// (claude-code) agents route all LLM work through the backend, have no
	// GroupResolver, and carry no resolvable models — so the override has no
	// meaning for them. Reject cleanly rather than fall through to a confusing
	// "model not found" resolver error.
	if inst.ag.DelegatedManager != nil {
		httpLog.Infof("model override rejected for delegated agent %q (session=%s, value=%q): not supported for claude-code backends", inst.id, sessionKey, value)
		return fmt.Errorf("per-session model override is not supported for claude-code (delegated) agents")
	}

	// Check if value is a group name (built-in or user-defined).
	if inst.ag.GroupResolver != nil {
		if resolved := inst.ag.GroupResolver.ResolveGroup(value); resolved != nil {
			client := inst.ag.ClientProvider.ResolveEndpointClient(resolved.Endpoint, resolved.Format)
			model := resolved.Developer + "/" + resolved.ModelID
			inst.ag.SetSessionModel(sessionKey, model, resolved.Endpoint, resolved.Format, client)
			return nil
		}
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
			httpLog.Errorf("POST /-/reload-credentials: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "credentials reloaded"})
	}
}

// handlePprofToggle returns the handler for GET/POST /-/pprof.
// GET returns the current state; POST {"enabled": true/false} toggles it.
func handlePprofToggle(gate *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": gate.Load()})
		case http.MethodPost:
			var body struct {
				Enabled *bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			if body.Enabled != nil {
				gate.Store(*body.Enabled)
				httpLog.Infof("pprof gate toggled: %v", *body.Enabled)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": gate.Load()})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/defersend"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/voice"
)

// httpHandlerDeps holds shared state needed by HTTP endpoint handlers.
type httpHandlerDeps struct {
	agents            map[string]*agentInstance
	agentOrder        []string
	sessionIndex      *session.SessionIndex
	sessions          *session.Store
	cfg               *config.Config
	ctx               context.Context
	ttsMap            map[string]voice.TTS
	sttMap            map[string]voice.STT
	connMgr           platform.ConnectionManager
	reloadCredentials func() error
	pprofGate         *atomic.Bool // live-toggle gate for /debug/pprof/*
	deferStore        *defersend.Store
}

// checkActivityGate evaluates the four activity gate conditions and returns
// false if the request should be skipped (response already written).
//
// Two domains of activity are tracked separately:
//
//   - **User attention** — `last_user_activity` written by primary
//     Telegram/Discord inbound paths. Reflects a real user reaching out to
//     the agent. Read via isUserActive(agentID, within). Used by
//     --if-user-active / --if-user-inactive.
//   - **Session activity** — `last_activity` written by OrchestrateFullTurn
//     for every turn-init path (user, cron, CLI, webhook, agent-to-agent,
//     system-injected). Read via isSessionActive(sessionBase, within). Used
//     by --if-active / --if-inactive together with the in-flight
//     short-circuit.
//
// **In-flight short-circuit applies to both gates.** A turn currently
// executing on the target session counts as "active" for both --if-active
// and --if-user-active evaluations. The principle is "never queue a
// duplicate when something is already running" — keepalive crons piling up
// behind a long-running turn are exactly the bug this fixes (TODO #753).
//
// Returns true if the request should proceed, false if it has been
// short-circuited (response already written).
func checkActivityGate(w http.ResponseWriter, in activityGateInputs,
	isUserActive userActivityChecker, isSessionActive sessionActivityChecker) bool {

	// The in-flight short-circuit (a running turn counts as active) is applied
	// inside activityProbes, shared with the wait evaluator.
	userActiveWithin, sessionActiveWithin := activityProbes(in, isUserActive, isSessionActive)

	// The four if_(user_)?(in)?active gates share one shape: parse the duration,
	// 400 on a bad value, else skip (with a canned JSON reply) when the activity
	// state matches. They differ only in which activity check applies, whether
	// "active" or "inactive" triggers the skip, and the skip message. Evaluated
	// in declaration order; the first gate that says skip wins.
	gates := []struct {
		value          string
		label          string
		subject        string
		skipResp       string
		active         func(time.Duration) bool
		skipWhenActive bool
	}{
		{in.IfUserActive, "if_user_active", in.SessionBase, "skipped: no recent user activity", userActiveWithin, false},
		{in.IfUserInactive, "if_user_inactive", in.SessionBase, "skipped: user recently active", userActiveWithin, true},
		{in.IfActive, "if_active", in.SessionBase, "skipped: no recent activity", sessionActiveWithin, false},
		{in.IfInactive, "if_inactive", in.SessionBase, "skipped: session recently active", sessionActiveWithin, true},
	}
	for _, g := range gates {
		if g.value == "" {
			continue
		}
		dur, err := time.ParseDuration(g.value)
		if err != nil {
			http.Error(w, fmt.Sprintf("bad %s duration: %v", g.label, err), http.StatusBadRequest)
			return false
		}
		if g.active(dur) == g.skipWhenActive {
			log.NewComponentLogger(in.LogTag).Debugf("POST %s: skipping %s=%s (subject %s)", in.Endpoint, g.label, g.value, g.subject)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"response": g.skipResp})
			return false
		}
	}
	return true
}

// authMiddleware returns an HTTP middleware that requires a valid API key on
// all endpoints including /voice.
// Checks Authorization: Bearer header first, then falls back to an api_key query
// param — but ONLY on /voice, where the browser WebSocket API can't set request
// headers. A credential in the URL leaks via proxy logs, browser history, and
// access logs, so it is not accepted on the JSON endpoints. Uses constant-time
// comparison.
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /app/* endpoints self-authenticate against per-device tokens (the app hub's
		// for the JSON handlers, ServeWS for the socket), each with its own
		// per-IP rate limiting. The outer http.api_key gate would shadow them:
		// app device tokens != http.api_key, so every app bearer would 403 here before
		// reaching the handler (and never log). Skip the outer gate for /app/.
		if strings.HasPrefix(r.URL.Path, "/app/") {
			httpLog.Debugf("auth: %s %s from %s — /app/ self-authenticates downstream, skipping outer gate", r.Method, r.URL.Path, r.RemoteAddr)
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization: Bearer header
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = auth[len("Bearer "):]
		}

		// Fallback: api_key query param — WebSocket compat only (/voice).
		if token == "" && r.URL.Path == "/voice" {
			token = r.URL.Query().Get("api_key")
		}

		// Per-request trace at DEBUG. NEVER log the token value — only its
		// presence and length, enough to tell "no credential" from "wrong
		// credential" (the distinction that made the /app/ 403 hard to diagnose).
		if token == "" {
			httpLog.Debugf("auth: %s %s from %s — no credential, 401", r.Method, r.URL.Path, r.RemoteAddr)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			httpLog.Debugf("auth: %s %s from %s — bearer (len %d) does not match http.api_key, 403", r.Method, r.URL.Path, r.RemoteAddr, len(token))
			http.Error(w, "invalid credentials", http.StatusForbidden)
			return
		}

		httpLog.Debugf("auth: %s %s from %s — http.api_key OK", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// registerHTTPHandlers registers all HTTP endpoints (/send, /status, /command, /branch, /webhook, /voice).
func registerHTTPHandlers(mux *http.ServeMux, d httpHandlerDeps) {
	resolveAgent, gate := buildResolvers(d)

	mux.HandleFunc("/send", handleSend(d, resolveAgent, gate))
	mux.HandleFunc("/status", handleStatus(d, resolveAgent))
	mux.HandleFunc("/command", handleCommand(d, resolveAgent, gate))
	mux.HandleFunc("/branch", handleBranch(d, resolveAgent, gate))
	mux.HandleFunc("/webhook/", handleWebhook(d, resolveAgent, gate))

	endpointList := "/send, /status, /command, /branch, /webhook/{agent}/{hookid}"
	if d.cfg.HTTP.WSEnabled && len(d.sttMap) > 0 {
		mux.HandleFunc("/voice", voice.Handler(buildVoiceConfig(d)))
		endpointList += ", /voice (ws)"
		httpLog.Infof("/voice WebSocket endpoint enabled")
	}

	// App provider WebSocket (FAP v1). Self-authenticating (Bearer device token);
	// registered whenever the app provider is configured with a key.
	if app.Enabled() {
		mux.HandleFunc("/app/ws", app.WSHandler())
		mux.HandleFunc("/app/blob", app.BlobUploadHandler())            // POST: upload
		mux.HandleFunc("/app/blob/", app.BlobDownloadHandler())         // GET /app/blob/<id>
		mux.HandleFunc("/app/pair", app.PairHandler())                  // POST: mint device token (single-use pairing key)
		mux.HandleFunc("/app/pair/revoke", app.RevokeHandler())         // POST: revoke a device (device token)
		mux.HandleFunc("/app/devices", app.DevicesHandler())            // GET: list devices (device token)
		mux.HandleFunc("/app/push/register", app.PushRegisterHandler()) // POST: refresh FCM token
		mux.HandleFunc("/app/history", app.HistoryHandler())            // GET: restart reconciliation
		mux.HandleFunc("/app/replay", app.ReplayHandler())              // GET: durable content backfill
		mux.HandleFunc("/app/avatar/", app.AvatarHandler())             // GET /app/avatar/<agentId>: agent avatar image
		endpointList += ", /app/ws (ws), /app/blob, /app/pair, /app/devices, /app/push/register, /app/history, /app/replay, /app/avatar"
		httpLog.Infof("/app/ws + /app/blob + /app/pair + /app/devices + /app/push/register + /app/history + /app/replay + /app/avatar endpoints enabled")
	}

	if d.reloadCredentials != nil {
		mux.HandleFunc("/-/reload-credentials", handleReloadCredentials(d))
		endpointList += ", /-/reload-credentials"
	}

	// pprof endpoints are always registered but gated by a live-toggleable
	// atomic bool (seeded from [debug] enable_pprof at startup). This avoids
	// the ServeMux limitation that handlers can't be unregistered — the gate
	// is checked per-request, so toggling is instant without a mux rebuild.
	ppGate := d.pprofGate
	if ppGate == nil {
		ppGate = &atomic.Bool{}
	}
	gated := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !ppGate.Load() {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/debug/pprof/", gated(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", gated(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", gated(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", gated(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", gated(pprof.Trace))
	mux.HandleFunc("/-/pprof", handlePprofToggle(ppGate))
	endpointList += ", /debug/pprof/* (gated), /-/pprof"

	httpLog.Infof("registered endpoints: %s", endpointList)
}

// runAgentBuffered attaches a BufferSink to ctx, calls HandleMessage, and
// returns the captured FinalText. Used by sync HTTP callers (runAgentQueued),
// voice, and the silent/PolicyBroadcast carve-outs of deliverBufferedQueued
// (http.go) — callers that need the final response text back rather than a
// streamed chat delivery. The alternative is repeating the
// buf-sink-handle-finaltext four-line dance at every site, which makes
// adding a new caller error-prone.
func runAgentBuffered(ctx context.Context, ag *agent.Agent, sessionKey, text string) (string, error) {
	buf := turnevent.NewBufferSink()
	ctx = turnevent.WithSink(ctx, buf)
	if err := ag.HandleMessage(ctx, sessionKey, []string{text}, nil); err != nil {
		return "", err
	}
	return buf.FinalText(), nil
}

// runAgentQueued routes a system-initiated turn through the session's inbox
// worker (Envelope.Inject) and blocks for the buffered response. Unlike
// runAgentBuffered — which drives the turn on the calling goroutine and can
// therefore race an in-flight turn — the queued turn serialises with the
// session's platform turns and defers behind a pending foci_ask: system
// input (HTTP /send, branch fall-through, webhook) never steers running work,
// it waits gracefully for turn completion. The InjectMeta trigger is taken
// from the ctx trigger label.
//
// Callers already running on the session's inbox worker (Inject.Run
// closures) must keep using runAgentBuffered — see Agent.EnqueueInjectWait.
func runAgentQueued(ctx context.Context, ag *agent.Agent, sessionKey, text string) (string, error) {
	var resp string
	var runErr error
	if err := ag.EnqueueInjectWait(ctx, sessionKey, agent.TriggerFromContext(ctx), func() {
		resp, runErr = runAgentBuffered(ctx, ag, sessionKey, text)
	}); err != nil {
		return "", err
	}
	return resp, runErr
}

// writeJSONResponse writes a {"response": text} JSON envelope to w. Encoding
// errors are logged but not returned because by this point the status line
// has already been committed and there is nothing the caller can do.
func writeJSONResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"response": text}); err != nil {
		httpLog.Errorf("encode response: %v", err)
	}
}

// writeJSONReceipt writes a {"response", "session", "resolved_via"} envelope:
// the response text plus the routing receipt, so external senders (cron,
// scripts) can verify WHERE their message actually landed instead of trusting
// silent fallbacks.
func writeJSONReceipt(w http.ResponseWriter, text string, rcpt route.Receipt) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"response":     text,
		"target":       rcpt.Target,
		"session":      rcpt.SessionKey,
		"resolved_via": string(rcpt.Via),
	}); err != nil {
		httpLog.Errorf("encode response: %v", err)
	}
}

// policyOrFallback normalises an unset policy to the default.
func policyOrFallback(p route.Policy) route.Policy {
	if p == "" {
		return route.PolicyFallback
	}
	return p
}

// broadcastResponse delivers a turn's response text to every live connection
// for the agent (PolicyBroadcast): the session's own chat gets it via
// SendToSession; every other surface gets it via SendText (its default chat).
func broadcastResponse(connMgr platform.ConnectionManager, agentID, sessionKey, text, logTag string) {
	sessionConn := connMgr.ForSession(sessionKey)
	delivered := 0
	for _, conn := range route.Broadcast(connMgr, agentID) {
		var err error
		if conn == sessionConn {
			err = conn.SendToSession(sessionKey, text)
		} else {
			err = conn.SendText(text)
		}
		if err != nil {
			log.NewComponentLogger(logTag).Errorf("broadcast delivery via %s: %v", conn.PlatformName(), err)
			continue
		}
		delivered++
	}
	log.NewComponentLogger(logTag).Infof("broadcast response for session %s delivered to %d connection(s)", sessionKey, delivered)
}

// defaultSessionKey resolves an agent's default session, tolerating "no
// session yet" as an empty key (handlers that dispatch commands accept that).
func defaultSessionKey(d httpHandlerDeps, agentID string) string {
	r := &route.Resolver{Index: d.sessionIndex, PreferredPlatform: d.cfg.DefaultPlatformFor}
	res, err := r.Resolve(route.Target{Agent: agentID})
	if err != nil {
		return ""
	}
	return res.SessionKey
}

// asyncDispatch handles async fire-and-forget requests: queues the agent
// message on the session's inbox worker, writes a 202 response, and
// delivers the result via platform. Queueing (rather than a detached
// goroutine) means the turn serialises with the session's platform turns and
// waits behind any in-flight turn — system input never steers running work.
//
// deliverBufferedQueued queues a turn on the session's inbox and streams its
// output to the resolved connection AS THE TURN RUNS — the same
// turnSinkForConn selection deliverToSessionChat (agents_notify.go) uses for
// every other injected delivery (wakes, restart changelogs, send_to_session):
// a StreamingSink-equivalent for connections whose Driver can build one (the
// app), or a SessionSink (typing indicator + per-block delivery) otherwise.
//
// Telegram/Discord land on SessionSink, not true per-token StreamingSink,
// because neither platform's Bot.NewTurnSink can build one for an injected
// turn: both require a live env.Original platform message (*gotgbot.Message /
// *discordgo.Message) to seed the renderer — used only to read its chat/
// channel ID — and an HTTP-injected /send, /branch, /webhook, or
// deferred-send has no such originating message to attach. Closing that gap
// (letting NewTurnSink build a renderer from a bare chat/channel ID) is
// platform-driver surgery, flagged here rather than done speculatively as
// part of this fix (#1385).
//
// #1385: before this, the async path ran the turn behind a bare
// turnevent.BufferSink (internal/agent/turnevent/sinks.go — discards every
// event but TurnComplete) and did exactly one flat SendToSession call at the
// end: no typing indicator ever fired and nothing was visible until the
// whole turn finished — the "basic sink / fallback" the bug report described.
//
// Two cases deliberately keep the old buffer-then-forward-once shape:
//   - silent (branch --silent): no chat delivery happens at all, so there is
//     nothing to stream into.
//   - PolicyBroadcast: the same final text fans out to every live connection
//     for the agent — there's no single chat to stream into, and teeing a
//     capture sink alongside a delivery sink was explicitly ruled out (Dick,
//     #1385) rather than built for this one case.
//
// Both still use runAgentBuffered/BufferSink, whose FinalText() is the only
// thing they need — Usage/Cost/Model were never captured or used by any
// caller of this function (async /send and async /branch return their HTTP
// response before the turn runs; the deferred-send sweep discards the return
// value outright — verified at every call site), so dropping the capture
// costs nothing for the streamed case either.
//
// Returns false if the inbox is full. Shared by the async /send path,
// /branch (both the backend-fork success and send-fallback branches),
// /webhook, and the deferred-send sweep (wait_defer.go); the caller owns any
// HTTP reply.
func deliverBufferedQueued(inst *agentInstance, connMgr platform.ConnectionManager,
	ctx context.Context, sessionKey, text, logTag string, silent bool, policy route.Policy) bool {
	run := func() {
		if silent || policy == route.PolicyBroadcast {
			resp, err := runAgentBuffered(ctx, inst.ag, sessionKey, text)
			if err != nil {
				log.NewComponentLogger(logTag).Errorf("async error: %v", err)
				return
			}
			if silent || connMgr == nil {
				return
			}
			cleaned := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(resp))
			if cleaned == "" {
				return
			}
			broadcastResponse(connMgr, inst.id, sessionKey, cleaned, logTag)
			return
		}

		turnCtx := ctx
		var cleanup func()
		if connMgr != nil {
			if conn, outcome := route.ConnFor(connMgr, inst.id, sessionKey, policyOrFallback(policy)); conn != nil {
				if outcome == route.DeliveredViaPrimary {
					log.NewComponentLogger(logTag).Infof("session %s has no live connection — delivering via agent %s primary", sessionKey, inst.id)
				}
				var sink turnevent.Sink
				sink, cleanup = turnSinkForConn(conn, sessionKey, logTag)
				turnCtx = turnevent.WithSink(ctx, sink)
			} else {
				log.NewComponentLogger(logTag).Warnf("no connection for session %s (policy=%s), async response not delivered", sessionKey, policyOrFallback(policy))
			}
		}
		if cleanup != nil {
			defer cleanup()
		}
		if err := inst.ag.HandleMessage(turnCtx, sessionKey, []string{text}, nil); err != nil {
			log.NewComponentLogger(logTag).Errorf("async error: %v", err)
		}
	}
	return inst.ag.Enqueue(agent.Envelope{
		SessionKey: sessionKey,
		Inject:     &agent.InjectMeta{Trigger: agent.TriggerFromContext(ctx), Run: run},
	})
}

func asyncDispatch(w http.ResponseWriter, inst *agentInstance, connMgr platform.ConnectionManager,
	ctx context.Context, sessionKey, text, logTag string, silent bool, policy route.Policy, rcpt route.Receipt) {
	if !deliverBufferedQueued(inst, connMgr, ctx, sessionKey, text, logTag, silent, policy) {
		http.Error(w, "session inbox full", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":       "queued",
		"target":       rcpt.Target,
		"session":      rcpt.SessionKey,
		"resolved_via": string(rcpt.Via),
	})
}

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
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

	// userActiveWithin: did the user touch this agent within the duration,
	// OR is a turn currently in flight? In-flight counts as user attention
	// because the agent is currently engaged — sending another message
	// would queue behind, which is what --if-user-inactive wants to avoid.
	userActiveWithin := func(within time.Duration) bool {
		if in.InFlight {
			return true
		}
		return isUserActive(in.AgentID, within)
	}

	// sessionActiveWithin: did this session execute a turn within the
	// duration, OR is one in flight now?
	sessionActiveWithin := func(within time.Duration) bool {
		if in.InFlight {
			return true
		}
		return isSessionActive(in.SessionBase, within)
	}

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
		{in.IfUserActive, "if_user_active", in.AgentID, "skipped: no recent user activity", userActiveWithin, false},
		{in.IfUserInactive, "if_user_inactive", in.AgentID, "skipped: user recently active", userActiveWithin, true},
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
			log.Debugf(in.LogTag, "POST %s: skipping %s=%s (subject %s)", in.Endpoint, g.label, g.value, g.subject)
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
			log.Debugf("http", "auth: %s %s from %s — /app/ self-authenticates downstream, skipping outer gate", r.Method, r.URL.Path, r.RemoteAddr)
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
			log.Debugf("http", "auth: %s %s from %s — no credential, 401", r.Method, r.URL.Path, r.RemoteAddr)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			log.Debugf("http", "auth: %s %s from %s — bearer (len %d) does not match http.api_key, 403", r.Method, r.URL.Path, r.RemoteAddr, len(token))
			http.Error(w, "invalid credentials", http.StatusForbidden)
			return
		}

		log.Debugf("http", "auth: %s %s from %s — http.api_key OK", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// registerHTTPHandlers registers all HTTP endpoints (/send, /status, /command, /wake, /webhook, /voice).
func registerHTTPHandlers(mux *http.ServeMux, d httpHandlerDeps) {
	resolveAgent, gate := buildResolvers(d)

	mux.HandleFunc("/send", handleSend(d, resolveAgent, gate))
	mux.HandleFunc("/status", handleStatus(d, resolveAgent))
	mux.HandleFunc("/command", handleCommand(d, resolveAgent, gate))
	mux.HandleFunc("/wake", handleWake(d, resolveAgent, gate))
	mux.HandleFunc("/webhook/", handleWebhook(d, resolveAgent, gate))

	endpointList := "/send, /status, /command, /wake, /webhook/{agent}/{hookid}"
	if d.cfg.HTTP.WSEnabled && len(d.sttMap) > 0 {
		mux.HandleFunc("/voice", voice.Handler(buildVoiceConfig(d)))
		endpointList += ", /voice (ws)"
		log.Infof("http", "/voice WebSocket endpoint enabled")
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
		log.Infof("http", "/app/ws + /app/blob + /app/pair + /app/devices + /app/push/register + /app/history + /app/replay + /app/avatar endpoints enabled")
	}

	if d.reloadCredentials != nil {
		mux.HandleFunc("/-/reload-credentials", handleReloadCredentials(d))
		endpointList += ", /-/reload-credentials"
	}

	// pprof exposes profiling/goroutine-dump endpoints. Gated behind an explicit
	// [debug] enable_pprof opt-in (default off) even though the server is
	// auth-gated — profiling shouldn't be reachable on a normal deployment.
	if config.DerefBool(d.cfg.Debug.EnablePprof) {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		endpointList += ", /debug/pprof/*"
	}

	log.Infof("http", "registered endpoints: %s", endpointList)
}

// runAgentBuffered attaches a BufferSink to ctx, calls HandleMessage, and
// returns the captured FinalText. Used by HTTP/voice/async/notify callers
// that just need the final response text rather than streaming events —
// the alternative is repeating the buf-sink-handle-finaltext four-line
// dance at every site, which makes adding a new caller error-prone.
func runAgentBuffered(ctx context.Context, ag *agent.Agent, sessionKey, text string) (string, error) {
	buf := turnevent.NewBufferSink()
	ctx = turnevent.WithSink(ctx, buf)
	if err := ag.HandleMessage(ctx, sessionKey, []string{text}, nil); err != nil {
		return "", err
	}
	return buf.FinalText(), nil
}

// writeJSONResponse writes a {"response": text} JSON envelope to w. Encoding
// errors are logged but not returned because by this point the status line
// has already been committed and there is nothing the caller can do.
func writeJSONResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"response": text}); err != nil {
		log.Errorf("http", "encode response: %v", err)
	}
}

// asyncDispatch handles async fire-and-forget requests: sends the agent message
// in a goroutine, writes a 202 response, and optionally delivers the result via platform.
//
// Silencing: the captured FinalText is routed through
// platform.StripSilencingSuffix before being forwarded to
// Connection.SendToSession — this both suppresses fully-silent text (strips to
// "") and removes a trailing sentinel an agent appended to a real reply. This
// is the convergence point for the BufferSink→platform forwarding class —
// StreamingSink/SessionSink own their own gates (renderer chokepoints and
// SessionSink.Emit respectively), so asyncDispatch is the one path where a
// silencing sentinel reaches the user without an upstream gate unless we apply
// one here.
func asyncDispatch(w http.ResponseWriter, inst *agentInstance, connMgr platform.ConnectionManager,
	ctx context.Context, sessionKey, text, logTag string, silent bool) {
	go func() {
		resp, err := runAgentBuffered(ctx, inst.ag, sessionKey, text)
		if err != nil {
			log.Errorf(logTag, "async error: %v", err)
			return
		}
		cleaned := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(resp))
		if !silent && cleaned != "" && connMgr != nil {
			if conn := connMgr.ForSessionOrPrimary(sessionKey, inst.id); conn != nil {
				if err := conn.SendToSession(sessionKey, cleaned); err != nil {
					log.Errorf(logTag, "async platform delivery: %v", err)
				}
			}
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
}

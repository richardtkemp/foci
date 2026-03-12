package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/voice"
)

// httpHandlerDeps holds shared state needed by HTTP endpoint handlers.
type httpHandlerDeps struct {
	agents            map[string]*agentInstance
	agentOrder        []string
	stateStore        *state.Store
	sessions          *session.Store
	cfg               *config.Config
	ctx               context.Context
	ttsMap            map[string]voice.TTS
	sttMap            map[string]voice.STT
	connMgr           platform.ConnectionManager
	reloadCredentials func() error
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
// all endpoints including /voice.
// Checks Authorization: Bearer header first, then falls back to api_key query
// param (for WebSocket compat). Uses constant-time comparison.
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	resolveAgent, isAgentActive := buildResolvers(d)

	mux.HandleFunc("/send", handleSend(d, resolveAgent, isAgentActive))
	mux.HandleFunc("/status", handleStatus(d, resolveAgent))
	mux.HandleFunc("/command", handleCommand(d, resolveAgent))
	mux.HandleFunc("/wake", handleWake(d, resolveAgent, isAgentActive))

	endpointList := "/send, /status, /command, /wake"
	if d.cfg.HTTP.WSEnabled && len(d.sttMap) > 0 {
		mux.HandleFunc("/voice", voice.Handler(buildVoiceConfig(d)))
		endpointList += ", /voice (ws)"
		log.Infof("http", "/voice WebSocket endpoint enabled")
	}

	if d.reloadCredentials != nil {
		mux.HandleFunc("/-/reload-credentials", handleReloadCredentials(d))
		endpointList += ", /-/reload-credentials"
	}

	log.Infof("http", "registered endpoints: %s", endpointList)
}

// asyncDispatch handles async fire-and-forget requests: sends the agent message
// in a goroutine, writes a 202 response, and optionally delivers the result via platform.
func asyncDispatch(w http.ResponseWriter, inst *agentInstance, connMgr platform.ConnectionManager,
	ctx context.Context, sessionKey, text, logTag string, silent bool) {
	go func() {
		resp, err := inst.ag.HandleMessage(ctx, sessionKey, text)
		if err != nil {
			log.Errorf(logTag, "async error: %v", err)
			return
		}
		if resp != "" && !silent {
			if conn := connMgr.ForSessionOrPrimary(sessionKey, inst.id); conn != nil {
				if err := conn.SendTextToSession(sessionKey, resp); err != nil {
					log.Errorf(logTag, "async platform delivery: %v", err)
				}
			}
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
}

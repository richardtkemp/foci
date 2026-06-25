package app

import (
	"net/http"
	"sync"
)

// activeHub is the hub of the configured app provider, set at Init. The HTTP
// layer (cmd/foci-gw) reaches it through Enabled/WSHandler without importing
// provider internals or plumbing the API key around — the hub authenticates.
var (
	activeMu  sync.RWMutex
	activeHub *Hub
)

func setActiveHub(h *Hub) {
	activeMu.Lock()
	activeHub = h
	activeMu.Unlock()
}

// Enabled reports whether the app provider is configured and able to serve the
// WebSocket endpoint (a hub exists with a non-empty API key).
func Enabled() bool {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeHub != nil && activeHub.apiKey != ""
}

// WSHandler returns the /app/ws HTTP handler. It authenticates each upgrade
// (Bearer app.api_key) inside ServeWS, so it needs no shared auth middleware.
func WSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		activeMu.RLock()
		h := activeHub
		activeMu.RUnlock()
		if h == nil {
			http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
			return
		}
		h.ServeWS(w, r)
	}
}

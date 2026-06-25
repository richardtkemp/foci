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

// withHub returns an http.HandlerFunc that resolves the active hub (503 if
// unconfigured) and delegates to fn. Each hub method does its own Bearer auth,
// so no shared middleware is needed.
func withHub(fn func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		activeMu.RLock()
		h := activeHub
		activeMu.RUnlock()
		if h == nil {
			http.Error(w, "app endpoint not configured", http.StatusServiceUnavailable)
			return
		}
		fn(h, w, r)
	}
}

// WSHandler returns the /app/ws handler (authenticates inside ServeWS).
func WSHandler() http.HandlerFunc { return withHub((*Hub).ServeWS) }

// BlobUploadHandler returns the POST /app/blob handler.
func BlobUploadHandler() http.HandlerFunc { return withHub((*Hub).ServeBlobPost) }

// BlobDownloadHandler returns the GET /app/blob/<id> handler.
func BlobDownloadHandler() http.HandlerFunc { return withHub((*Hub).ServeBlobGet) }

// PairHandler returns the POST /app/pair handler (mint a device token).
func PairHandler() http.HandlerFunc { return withHub((*Hub).ServePair) }

// DevicesHandler returns the GET /app/devices handler (list paired devices).
func DevicesHandler() http.HandlerFunc { return withHub((*Hub).ServeDevices) }

// RevokeHandler returns the POST /app/pair/revoke handler (revoke a device).
func RevokeHandler() http.HandlerFunc { return withHub((*Hub).ServeRevoke) }

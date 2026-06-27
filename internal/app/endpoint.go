package app

import (
	"errors"
	"net/http"
	"sync"
	"time"
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
// app endpoints (a hub exists). Authentication is per-device-token (#862); there
// is no shared key whose presence gates the endpoint.
func Enabled() bool {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeHub != nil
}

// MintActivePairKey mints a single-use, short-TTL pairing key on the live app
// hub (#862) and returns it with its expiry. A device exchanges this key at
// POST /app/pair for its own revocable token. The key lives only in memory and
// is delivered to the user out-of-band (the /android wizard or the `foci` CLI);
// it is never persisted or logged. Returns an error if the app provider is not
// running (pairing requires a live gateway).
func MintActivePairKey(ttl time.Duration) (string, time.Time, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", time.Time{}, errors.New("app provider is not running")
	}
	key, exp := h.pairKeys.mint(ttl)
	return key, exp, nil
}

// withHub returns an http.HandlerFunc that resolves the active hub (503 if
// unconfigured) and delegates to fn. Each hub method does its own Bearer auth,
// so no shared middleware is needed.
func withHub(fn func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer recoverApp("http " + r.Method + " " + r.URL.Path)
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

// PushRegisterHandler returns the POST /app/push/register handler (refresh a
// device's FCM token out-of-band, e.g. after the OS rotates it while offline).
func PushRegisterHandler() http.HandlerFunc { return withHub((*Hub).ServePushRegister) }

// HistoryHandler returns the GET /app/history handler (restart reconciliation).
func HistoryHandler() http.HandlerFunc { return withHub((*Hub).ServeHistory) }

// ReplayHandler returns the GET /app/replay handler (durable content backfill).
func ReplayHandler() http.HandlerFunc { return withHub((*Hub).ServeReplay) }

// AvatarHandler returns the GET /app/avatar/<agentId> handler (agent avatar image).
func AvatarHandler() http.HandlerFunc { return withHub((*Hub).ServeAvatar) }

package app

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"foci/internal/app/fap"
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

// OpenSessionsForAgent returns the session keys of the agent's open chats from
// the persisted shared open-set (open_chats), or nil if the app provider is not
// running. Used by keepalive when warm_open_app_chats is set. Persisted rather
// than live-socket so keepalive keeps warming open chats across app disconnects.
func OpenSessionsForAgent(agentID string) []string {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return nil
	}
	return h.OpenSessionsForAgent(agentID)
}

// ActiveConnCount returns the number of live app sockets on the configured hub,
// or 0 if the app provider is not running. The goroutine monitor calls it each
// sample tick to grow its threshold with the live connection count (dynamic
// headroom for a quantity the startup formula cannot predict).
func ActiveConnCount() int {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return 0
	}
	return h.ConnCount()
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

// SetSubagentDetail routes a running-subagent status detail (or "" when none
// are running) to the conversation bound to sessionKey, updating its unified
// Activity indicator. No-op when the app provider is not running or the session
// has no live binding — the app is only one of several platforms and a session
// may not be an app conversation at all.
func SetSubagentDetail(sessionKey, detail string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		b.setSubagentDetail(detail)
	}
}

// SetCacheExpiry routes a prompt-cache expiry (unix ms; 0 = cold) to the
// conversation bound to sessionKey, refreshing its warmth indicator whenever
// the cache is (re)warmed — not only on a turn that completes through a live app
// sink. The agent's onCacheExpiry hook is wired here at gateway setup. No-op
// when the app provider isn't running or the session has no live binding.
func SetCacheExpiry(sessionKey string, expiryMs int64) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		b.setCacheExpiry(expiryMs)
	}
}

// DeliverExternalPrompt surfaces a prompt received via the external HTTP /send
// endpoint to the app clients bound to sessionKey, as a durable ExternalPrompt
// frame. No-op when the app provider is not running or the session has no live
// binding (foci send to a telegram/discord-only agent surfaces via that
// transport's injected-message header instead).
func DeliverExternalPrompt(sessionKey, text string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(sessionKey); b != nil {
		b.send(fap.ExternalPrompt{ConversationID: b.convID, MessageID: fap.NewULID(), Text: text})
	}
}

// SetWaiting marks the conversation bound to callerSessionKey as waiting on
// another foci agent (targetAgent), or clears it when targetAgent is "". Used by
// send_to_session so the caller's Activity shows "waiting" until its reply turn
// begins (which clears it). No-op when the app provider is not running or the
// session has no live binding.
func SetWaiting(callerSessionKey, targetAgent string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	if b := h.bindingForSession(callerSessionKey); b != nil {
		b.setWaitingDetail(targetAgent)
	}
}

// ResolvedActivity returns the resolved unified activity (kind + detail) for the
// conversation bound to sessionKey, so /status renders the same value the app
// sees. ok is false when the app provider is not running or the session has no
// app binding (the caller falls back gracefully).
func ResolvedActivity(sessionKey string) (kind, detail string, ok bool) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", "", false
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return "", "", false
	}
	b.mu.Lock()
	k, d := b.resolveActivity()
	b.mu.Unlock()
	return string(k), d, true
}

// MintFacetConversation surfaces a facet branch to app clients via the active
// hub (see Hub.mintFacetConversation). Injected into the command layer by the
// gateway as CommandContext.MintFacetConversation. Errors if the app provider
// is not running.
func MintFacetConversation(agentID, sessionKey string) (string, error) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return "", errors.New("app provider not running")
	}
	return h.mintFacetConversation(agentID, sessionKey)
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

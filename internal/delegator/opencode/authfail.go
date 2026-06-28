// authfail.go — auth-failure detection + Server-level fanout.
//
// opencode surfaces auth failures (expired OAuth tokens, invalid API
// keys) via two SSE paths: message.updated with error.name ==
// "ProviderAuthError" (Step 7 handlers) and session.error with the same.
// A third detection path is HTTP 401 from outbound requests (prompt POST,
// config PATCH, permission POST) — added here in Step 11.
//
// Because one Server serves all sessions for an agent, an auth failure
// on one session is account-wide. The Server fans the notification to
// every registered Backend so each can relay to its onAuthFailure
// callback (which the agent layer wires to the relogin gate in
// cmd/foci-gw/agents_delegated.go).
//
// fireAuthFailure is gated by an atomic flag per Backend so a flaky
// provider doesn't spam repeated notifications. The flag resets when
// the Backend is recreated (session restart).

package opencode

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"foci/internal/log"
)

// fireAuthFailure relays an auth-failure detail to the Backend's
// onAuthFailure callback. Gated by authFailureFired (atomic CAS) so
// only the first auth failure per Backend lifetime fires — subsequent
// calls are silent no-ops. This prevents a flaky 401 loop from
// spamming the user with repeated "auth failed" notifications.
func (b *Backend) fireAuthFailure(detail string) {
	if !b.authFailureFired.CompareAndSwap(false, true) {
		return // already fired for this Backend lifetime
	}
	b.mu.Lock()
	fn := b.onAuthFailure
	b.mu.Unlock()
	if fn != nil {
		log.Warnf(b.logComponent(), "firing onAuthFailure: %s", detail)
		fn(detail)
	} else {
		log.Warnf(b.logComponent(), "onAuthFailure is nil — auth failure not surfaced: %s", detail)
	}
}

// fanOutAuthFailure relays an auth-failure detail to every Backend
// registered on this Server. Called when any Backend detects a
// ProviderAuthError — the failure is account-wide so all sessions
// must know.
func (s *Server) fanOutAuthFailure(detail string) {
	s.sessionsMu.RLock()
	backends := make([]*Backend, 0, len(s.sessions))
	for _, be := range s.sessions {
		backends = append(backends, be)
	}
	s.sessionsMu.RUnlock()
	for _, be := range backends {
		be.fireAuthFailure(detail)
	}
	log.Warnf(s.logComponent(), "fanOutAuthFailure: dispatched to %d session(s)", len(backends))
}

// checkHTTP401 fires auth failure if the response status is 401
// Unauthorized. Called after every outbound HTTP request from the
// Backend (postMessage, patchConfig, postPermissionResponse).
func (b *Backend) checkHTTP401(statusCode int, url string) {
	if statusCode == http.StatusUnauthorized {
		if b.server != nil {
			b.server.fanOutAuthFailure(fmt.Sprintf("HTTP 401 from %s", url))
		} else {
			b.fireAuthFailure(fmt.Sprintf("HTTP 401 from %s", url))
		}
	}
}

// authFailureFired is the per-Backend CAS gate. Defined here as a
// comment anchor — the field lives on the Backend struct in opencode.go.
var _ atomic.Bool // keep import valid

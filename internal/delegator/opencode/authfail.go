// authfail.go — auth-failure detection + Server-level fanout.
//
// opencode surfaces auth failures (expired OAuth tokens, invalid API
// keys) via two SSE paths: message.updated with error.name ==
// "ProviderAuthError" (Step 7 handlers) and session.error with the same.
// A third detection path is HTTP 401 from outbound requests — caught
// here by wrapping the Server's HTTP transport so EVERY request goes
// through a 401 check. This is the architecturally correct approach per
// plan §11.2 ("wrap the Server's *http.Client transport"), avoiding the
// need to add explicit 401 checks to each individual HTTP-calling method.
//
// Because one Server serves all sessions for an agent, an auth failure
// on one session is account-wide. The Server fans the notification to
// every registered Backend so each can relay to its onAuthFailure
// callback.
//
// fireAuthFailure is gated by an atomic flag per Backend so a flaky
// provider doesn't spam repeated notifications. The flag resets when
// the Backend is recreated (session restart).

package opencode

import (
	"fmt"
	"net/http"

	"foci/internal/log"
)

// authCheckingTransport wraps an http.RoundTripper to detect 401
// responses and fire fanOutAuthFailure. Installed on every Server's
// HTTP client in newServer so all outbound requests are covered —
// POST /prompt_async, PATCH /config, POST /permissions, DELETE /session,
// etc. — without per-method checkHTTP401 calls.
type authCheckingTransport struct {
	base  http.RoundTripper
	on401 func(url string)
}

func (t *authCheckingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.on401(req.URL.String())
	}
	return resp, err
}

// wrapAuthCheckingTransport installs the 401-detecting transport on the
// Server's HTTP client. Called from newServer so production Servers
// always have it. Tests that manually construct Servers should call
// this if they want 401 detection (most test Servers use httptest
// which returns controlled status codes, so the wrapper is needed for
// the auth-failure tests).
func (s *Server) wrapAuthCheckingTransport() {
	if s.http == nil {
		s.http = &http.Client{}
	}
	base := s.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	s.http.Transport = &authCheckingTransport{
		base:  base,
		on401: func(url string) { s.fanOutAuthFailure(fmt.Sprintf("HTTP 401 from %s", url)) },
	}
}

// fireAuthFailure relays an auth-failure detail to the Backend's
// onAuthFailure callback. Gated by authFailureFired (atomic CAS) so
// only the first auth failure per Backend lifetime fires — subsequent
// calls are silent no-ops.
func (b *Backend) fireAuthFailure(detail string) {
	if !b.authFailureFired.CompareAndSwap(false, true) {
		return
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
// registered on this Server.
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

// checkHTTP401 is a legacy per-method 401 check. Kept for explicit
// call sites that want immediate detection (e.g., postMessage which
// runs before the transport wrapper's RoundTrip returns). The transport
// wrapper is the primary detection mechanism; this is belt-and-suspenders.
// The CAS gate in fireAuthFailure prevents double-firing.
func (b *Backend) checkHTTP401(statusCode int, url string) {
	if statusCode == http.StatusUnauthorized {
		if b.server != nil {
			b.server.fanOutAuthFailure(fmt.Sprintf("HTTP 401 from %s", url))
		} else {
			b.fireAuthFailure(fmt.Sprintf("HTTP 401 from %s", url))
		}
	}
}

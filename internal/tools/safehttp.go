package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"foci/internal/ratelimit"
)

// SSRF and transport limits shared by web_fetch and http_request. Centralised
// here rather than inlined so both tools share one safe client. (Reasonable
// candidates for promotion to config if a deployment needs to tune them.)
const (
	// defaultMaxRedirects caps redirect chains (each hop is re-validated by the
	// dialer, so this bounds work and redirect loops).
	defaultMaxRedirects = 10
	// defaultFetchTimeout is the wall-clock cap for a single web_fetch.
	defaultFetchTimeout = 30 * time.Second
	// defaultFetchParseTimeout bounds the readability/HTML parse step, which
	// runs on attacker-controlled HTML after the body is read. The x/net bump
	// removes the known html.Parse DoS; this is independent defence-in-depth so
	// a future parser pathology can't hang web_fetch past the network timeout.
	defaultFetchParseTimeout = 10 * time.Second
)

// isBlockedIP reports whether ip is a non-public address that must not be the
// target of an outbound request: loopback, RFC1918 private, IPv6 ULA, link-local
// (which covers the 169.254.169.254 cloud-metadata endpoint), multicast, or the
// unspecified address (0.0.0.0 / ::, which routes to localhost). This is the SSRF
// allow/deny core — applied to the *resolved* IP at dial time.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// Explicit cloud-metadata pins (169.254.169.254 is already link-local; the
	// IPv6 form is listed for clarity and future-proofing).
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	return false
}

// blockedIP is the SSRF predicate the dialer applies to each resolved address.
// It is a variable (rather than a direct isBlockedIP call) so tests can permit
// loopback httptest servers, and so the gateway can relax loopback on a
// skip_security_checks host (see PermitLoopbackHTTP); production with the flag
// unset never reassigns it. See safehttp_test.go.
var blockedIP = isBlockedIP

// PermitLoopbackHTTP relaxes the SSRF dialer to allow loopback targets (e.g. a
// 127.0.0.1 test server) while keeping every other block strict — private
// ranges, link-local/cloud-metadata, ULA, multicast, and the unspecified
// address all stay denied. The gateway calls this ONLY when
// skip_security_checks is set (a dev/test host that has already opted out of
// the strict secrets posture); production with the flag unset never calls it.
func PermitLoopbackHTTP() {
	blockedIP = func(ip net.IP) bool {
		if ip != nil && ip.IsLoopback() {
			return false
		}
		return isBlockedIP(ip)
	}
}

// safeDialContext resolves the target host, rejects the connection if ANY
// resolved address is non-public, and then dials a validated IP directly. By
// connecting to the exact IP it validated (rather than re-resolving the
// hostname) it closes the DNS-rebinding TOCTOU window.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}
	for _, ipa := range ips {
		if blockedIP(ipa.IP) {
			return nil, fmt.Errorf("blocked request to non-public address %s", ipa.IP)
		}
	}
	var dialer net.Dialer
	var firstErr error
	for _, ipa := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if err != nil {
			firstErr = err
			continue
		}
		return conn, nil
	}
	return nil, firstErr
}

// newSafeClient returns an *http.Client whose transport validates the resolved
// IP on every dial (covering redirects, since each hop dials afresh) and whose
// CheckRedirect caps the chain and blocks non-HTTP redirect schemes. Use it for
// all outbound requests from agent-reachable tools.
func newSafeClient(timeout time.Duration, maxRedirects int) *http.Client {
	transport := &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// Rate-limit aware: a 429/503 with a short Retry-After is retried transparently
	// (bounded — the client Timeout cancels the request context, so a wait can never
	// outlast the call's own deadline); a limit beyond the inline budget is passed
	// through unchanged so the agent still sees the real status and body. Wraps the
	// SSRF-safe transport, preserving per-dial IP validation on every retry.
	rlTransport := &ratelimit.Transport{
		Base:          transport,
		Kind:          ratelimit.KindRequest,
		Mode:          ratelimit.ModePassthrough,
		MaxInlineWait: 10 * time.Second,
		MaxRetries:    2,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: rlTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("blocked redirect to non-HTTP scheme %q", req.URL.Scheme)
			}
			// Block an https->http downgrade: a chain that began over TLS must
			// not be bounced onto a cleartext hop (which would expose any secret
			// header or sensitive response). A chain that started on http has no
			// TLS expectation, so http->http stays allowed. (P2-2.)
			if len(via) > 0 && via[0] != nil && via[0].URL.Scheme == "https" && req.URL.Scheme == "http" {
				return fmt.Errorf("blocked https->http downgrade redirect to %q", req.URL.Host)
			}
			return nil
		},
	}
}

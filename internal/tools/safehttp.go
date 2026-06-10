package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
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
// It is a variable (rather than a direct isBlockedIP call) solely so tests can
// permit loopback httptest servers while keeping every other block strict;
// production never reassigns it. See safehttp_test.go.
var blockedIP = isBlockedIP

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
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("blocked redirect to non-HTTP scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

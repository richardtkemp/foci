package tools

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// init relaxes the dialer's loopback block for tests only: httptest servers bind
// 127.0.0.1, so the existing http_request/web_fetch suites would otherwise fail
// to connect. Every other SSRF block stays strict, and isBlockedIP itself
// (tested directly below) still rejects loopback in production. Set once before
// any test runs, so parallel tests only read it — no race.
func init() {
	blockedIP = func(ip net.IP) bool {
		if ip != nil && ip.IsLoopback() {
			return false
		}
		return isBlockedIP(ip)
	}
}

// TestPermitLoopbackHTTP proves the skip_security_checks loopback relaxation
// allows loopback targets but keeps every other SSRF block strict — cloud
// metadata, private ranges, the unspecified address, and ULA all stay denied.
// Not parallel: it mutates the shared blockedIP var, so it runs in the
// sequential phase while parallel tests are paused, and restores on cleanup.
func TestPermitLoopbackHTTP(t *testing.T) {
	orig := blockedIP
	t.Cleanup(func() { blockedIP = orig })

	blockedIP = isBlockedIP // strict baseline
	if !blockedIP(net.ParseIP("127.0.0.1")) {
		t.Fatal("baseline should block loopback")
	}

	PermitLoopbackHTTP()
	if blockedIP(net.ParseIP("127.0.0.1")) {
		t.Error("loopback should be permitted after PermitLoopbackHTTP")
	}
	for _, s := range []string{"169.254.169.254", "10.0.0.1", "192.168.1.1", "0.0.0.0", "fd00::1", "fe80::1"} {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s must stay blocked after PermitLoopbackHTTP", s)
		}
	}
}

// TestIsBlockedIP proves the SSRF address filter rejects every non-public range
// an attacker could use to reach internal services or cloud metadata — including
// the unspecified address (0.0.0.0 / ::) that the old isPrivateIP missed — while
// allowing ordinary public addresses.
func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "::", // unspecified (route to localhost) — the P1-4 bypass
		"127.0.0.1", "::1", // loopback
		"10.0.0.1", "192.168.1.1", "172.16.0.1", // private v4
		"169.254.169.254",    // cloud metadata (link-local)
		"fc00::1", "fd00::1", // IPv6 ULA
		"fe80::1", // IPv6 link-local
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

// TestSafeDialContextRejectsBlocked proves the dialer refuses to connect to a
// non-public address. Because validation happens at dial time against the
// resolved IP, this is what defeats DNS rebinding — the connection only ever
// goes to an address that passed the check.
func TestSafeDialContextRejectsBlocked(t *testing.T) {
	// Loopback is omitted here because the test init() permits it (httptest);
	// production loopback blocking is covered by TestIsBlockedIP.
	for _, addr := range []string{"0.0.0.0:80", "169.254.169.254:80", "10.0.0.1:80"} {
		if _, err := safeDialContext(context.Background(), "tcp", addr); err == nil {
			t.Errorf("dial %s should be blocked", addr)
		}
	}
}

// TestSafeClientRedirectPolicy proves the shared client caps redirect count and
// refuses redirects to non-HTTP schemes (gopher://, file://, …). The resolved-IP
// re-check on each hop is enforced by safeDialContext (tested above), so a
// 302 → http://169.254.169.254/ is blocked when the hop is dialled.
func TestSafeClientRedirectPolicy(t *testing.T) {
	client := newSafeClient(5*time.Second, 3)

	fileReq := &http.Request{URL: &url.URL{Scheme: "file", Path: "/etc/passwd"}}
	if err := client.CheckRedirect(fileReq, nil); err == nil {
		t.Error("redirect to file:// scheme should be blocked")
	}

	httpReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}
	via := make([]*http.Request, 3)
	if err := client.CheckRedirect(httpReq, via); err == nil {
		t.Error("redirect past the cap should be blocked")
	}
	if err := client.CheckRedirect(httpReq, make([]*http.Request, 1)); err != nil {
		t.Errorf("a normal http redirect within the cap should be allowed: %v", err)
	}
}

// TestSafeClientBlocksHTTPSDowngrade proves the shared client refuses a redirect
// that downgrades https->http, so a fetch (or secret-bearing request) that began
// over TLS can't be silently bounced onto a cleartext hop. A chain that started
// on http has no TLS expectation, so http->http stays allowed. (P2-2.)
func TestSafeClientBlocksHTTPSDowngrade(t *testing.T) {
	client := newSafeClient(5*time.Second, 5)
	httpsStart := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	httpStart := &http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}
	toHTTP := &http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}
	toHTTPS := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}

	if err := client.CheckRedirect(toHTTP, []*http.Request{httpsStart}); err == nil {
		t.Error("https->http downgrade redirect should be blocked")
	}
	if err := client.CheckRedirect(toHTTPS, []*http.Request{httpsStart}); err != nil {
		t.Errorf("https->https redirect should be allowed: %v", err)
	}
	if err := client.CheckRedirect(toHTTP, []*http.Request{httpStart}); err != nil {
		t.Errorf("http->http redirect should be allowed: %v", err)
	}
}

// TestWebFetchBlocksUnspecified proves web_fetch — the default builtin, reachable
// by any agent and by untrusted fetched content — refuses an SSRF target, where
// previously it performed no filtering at all (P1-3).
func TestWebFetchBlocksUnspecified(t *testing.T) {
	params, _ := json.Marshal(map[string]any{"url": "http://0.0.0.0/"})
	if _, err := webFetch(context.Background(), params); err == nil {
		t.Fatal("web_fetch to 0.0.0.0 should be blocked")
	}
}

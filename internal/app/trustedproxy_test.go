package app

// #3078 (#2968 F8): the login lockout's bucket key. Forwarding headers count
// only when the socket peer is a trusted proxy; through a trusted chain the key
// is the first untrusted hop from the right; a valid device token is not locked
// out by a bucket it merely shares.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"foci/internal/config"
)

func ipReq(remoteAddr string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestRemoteIP_TrustedProxies(t *testing.T) {
	h := newTestHub()
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		// A client that reaches foci directly cannot pick its own bucket.
		{"untrusted peer, spoofed XFF ignored", "203.0.113.9:5000", []string{"1.1.1.1"}, "203.0.113.9"},
		{"untrusted peer, spoofed chain ignored", "203.0.113.9:5000", []string{"10.0.0.1, 1.1.1.1"}, "203.0.113.9"},
		{"tailnet peer is not a trusted proxy", "100.88.1.2:5000", []string{"1.1.1.1"}, "100.88.1.2"},
		// The NUC: cloudflared (10.0.5.x) -> traefik appends cloudflared's
		// address -> socat bridge (172.17.0.2, seen as an IPv4-mapped v6 peer).
		{"NUC chain, real client", "[::ffff:172.17.0.2]:57820", []string{"198.51.100.7, 10.0.5.4"}, "198.51.100.7"},
		{"NUC chain, client-spoofed left hops ignored", "[::ffff:172.17.0.2]:57820", []string{"6.6.6.6, 198.51.100.7, 10.0.5.4"}, "198.51.100.7"},
		{"NUC chain, IPv6 client", "172.17.0.2:1", []string{"2a01:4b00::1, 10.0.5.4"}, "2a01:4b00::1"},
		{"split header lines read in order", "127.0.0.1:1", []string{"6.6.6.6", "198.51.100.7, 10.0.5.4"}, "198.51.100.7"},
		{"trusted peer, no XFF -> peer", "127.0.0.1:1", nil, "127.0.0.1"},
		{"all hops trusted -> leftmost", "127.0.0.1:1", []string{"192.168.1.5, 10.0.5.4"}, "192.168.1.5"},
		{"empty hops skipped", "127.0.0.1:1", []string{"198.51.100.7, , 10.0.5.4,"}, "198.51.100.7"},
		{"hop with port", "127.0.0.1:1", []string{"198.51.100.7:4444"}, "198.51.100.7"},
		{"untrusted peer, no XFF", "5.5.5.5:1234", nil, "5.5.5.5"},
	}
	for _, c := range cases {
		if got := h.remoteIP(ipReq(c.remote, c.xff...)); got != c.want {
			t.Errorf("%s: remoteIP = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRemoteIP_TrustedProxiesConfig(t *testing.T) {
	h := newTestHub()
	nuc := ipReq("172.17.0.2:1", "198.51.100.7, 10.0.5.4")

	// "none": no peer is trusted, so the bucket is the socket peer.
	h.configureTrustedProxies(&config.AppSpecific{TrustedProxies: []string{"none"}})
	if got := h.remoteIP(nuc); got != "172.17.0.2" {
		t.Errorf("trusted_proxies=none: remoteIP = %q, want the socket peer 172.17.0.2", got)
	}

	// An explicit list replaces the defaults; a bare IP is a /32; an invalid
	// entry is skipped, not fatal.
	h.configureTrustedProxies(&config.AppSpecific{TrustedProxies: []string{"172.17.0.2", "bogus", "10.0.5.0/24"}})
	if got := h.remoteIP(nuc); got != "198.51.100.7" {
		t.Errorf("explicit list: remoteIP = %q, want 198.51.100.7", got)
	}
	if got := h.remoteIP(ipReq("127.0.0.1:1", "1.1.1.1")); got != "127.0.0.1" {
		t.Errorf("explicit list without loopback: remoteIP = %q, want 127.0.0.1", got)
	}

	// Unset (nil app, or nil list) restores the defaults.
	h.configureTrustedProxies(nil)
	if got := h.remoteIP(nuc); got != "198.51.100.7" {
		t.Errorf("defaults restored: remoteIP = %q, want 198.51.100.7", got)
	}
}

// Two devices behind the NUC's proxy chain keep separate lockout buckets: bad
// logins from one do not lock out the other.
func TestAuthenticate_PerClientBucketsBehindTrustedChain(t *testing.T) {
	h, _ := secHub(t)
	viaNUC := func(client, auth string) int {
		w := httptest.NewRecorder()
		r := secReq(http.MethodGet, "/app/history?conversationId=c", "", auth)
		r.RemoteAddr = "[::ffff:172.17.0.2]:40000"
		r.Header.Set("X-Forwarded-For", client+", 10.0.5.4")
		h.ServeHistory(w, r)
		return w.Code
	}
	for i := 0; i < authFailMax; i++ {
		viaNUC("198.51.100.66", "Bearer wrong")
	}
	if code := viaNUC("198.51.100.66", "Bearer wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("attacker after %d failures: code %d, want 429", authFailMax, code)
	}
	if code := viaNUC("198.51.100.7", "Bearer also-wrong"); code == http.StatusTooManyRequests {
		t.Errorf("a different client behind the same proxy chain is locked out by the attacker's failures")
	}
}

// A valid device token gets through a locked bucket it shares with an
// attacker; a bad one is still refused, and the valid login does not unlock
// the bucket for the attacker.
func TestAuthenticate_ValidTokenNotLockedOutBySharedBucket(t *testing.T) {
	h, tok := secHub(t)
	h.configureTrustedProxies(&config.AppSpecific{TrustedProxies: []string{"none"}}) // everyone shares the peer's bucket
	req := func(auth string) int {
		w := httptest.NewRecorder()
		r := secReq(http.MethodGet, "/app/devices", "", auth)
		r.RemoteAddr = "172.17.0.2:40000"
		h.ServeDevices(w, r)
		return w.Code
	}
	for i := 0; i < authFailMax; i++ {
		req("Bearer wrong")
	}
	if code := req("Bearer wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("bad token in locked bucket: code %d, want 429", code)
	}
	if code := req(""); code != http.StatusTooManyRequests {
		t.Errorf("no token in locked bucket: code %d, want 429", code)
	}
	if code := req("Bearer " + tok); code != http.StatusOK {
		t.Errorf("valid device token in a locked bucket: code %d, want 200", code)
	}
	if code := req("Bearer wrong"); code != http.StatusTooManyRequests {
		t.Errorf("after a valid login the attacker's bucket is unlocked: code %d, want 429", code)
	}
}

// Pairing keys are short (a ~52-bit passphrase), so pairing keeps the strict
// lockout: a locked bucket refuses even a valid key.
func TestServePair_LockoutStaysStrict(t *testing.T) {
	h, _ := secHub(t)
	pair := func(key string) int {
		w := httptest.NewRecorder()
		r := secReq(http.MethodPost, "/app/pair", `{"deviceId":"d2"}`, "Bearer "+key)
		r.RemoteAddr = "203.0.113.9:1"
		h.ServePair(w, r)
		return w.Code
	}
	for i := 0; i < authFailMax; i++ {
		pair("wrong")
	}
	if code := pair(secPairKey(h)); code != http.StatusTooManyRequests {
		t.Errorf("valid pairing key from a locked bucket: code %d, want 429", code)
	}
}

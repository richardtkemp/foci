package app

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"

	"foci/internal/config"
)

// trustedProxyNone is the trusted_proxies value that trusts no peer.
const trustedProxyNone = "none"

// defaultTrustedProxies are the peers whose X-Forwarded-For counts when
// [platforms.app] trusted_proxies is unset: loopback and the private ranges.
// They cover the NUC's ingress with no config: Cloudflare -> cloudflared
// (docker, 10.0.x) -> traefik, which appends cloudflared's address and keeps
// its X-Forwarded-For (traefik trusts 10/8 and 172.16/12) -> the socat bridge
// (docker, 172.17.x) -> foci. The tailnet (100.64.0.0/10) and every public
// address are NOT trusted: a client there that sets its own X-Forwarded-For is
// still locked out by its socket address.
var defaultTrustedProxies = mustPrefixes(
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7",
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// proxyTrust is the hub's live trusted-proxy set. The zero value (and a nil
// *proxyTrust) means defaultTrustedProxies, so a hub built without config, as
// in tests, behaves like an unconfigured server.
type proxyTrust struct {
	nets atomic.Pointer[[]netip.Prefix]
}

func (p *proxyTrust) get() []netip.Prefix {
	if p == nil {
		return defaultTrustedProxies
	}
	if n := p.nets.Load(); n != nil {
		return *n
	}
	return defaultTrustedProxies
}

// parseTrustedProxies turns trusted_proxies into prefixes. nil (unset) gives
// the defaults; "none" anywhere gives an empty set. A bare IP is a single-host
// prefix. Invalid entries are returned in bad and otherwise skipped.
func parseTrustedProxies(entries []string) (nets []netip.Prefix, bad []string) {
	if entries == nil {
		return defaultTrustedProxies, nil
	}
	nets = []netip.Prefix{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if strings.EqualFold(e, trustedProxyNone) {
			return []netip.Prefix{}, nil
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			nets = append(nets, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			a = a.Unmap()
			nets = append(nets, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		bad = append(bad, e)
	}
	return nets, bad
}

// configureTrustedProxies applies [platforms.app] trusted_proxies (nil app or
// unset key = defaults), warning once per invalid entry.
func (h *Hub) configureTrustedProxies(app *config.AppSpecific) {
	var entries []string
	if app != nil {
		entries = app.TrustedProxies
	}
	nets, bad := parseTrustedProxies(entries)
	for _, b := range bad {
		appLog.Warnf("platforms.app.trusted_proxies: %q is not an IP or CIDR range; ignored", b)
	}
	if h.trust == nil {
		h.trust = &proxyTrust{}
	}
	h.trust.nets.Store(&nets)
}

// ConfigureTrustedProxies is the live applier for [platforms.app]
// trusted_proxies (cmd/foci-gw/liveapply.go). A no-op when the app provider is
// not running.
func ConfigureTrustedProxies(app *config.AppSpecific) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.trust == nil {
		return
	}
	h.configureTrustedProxies(app)
}

// remoteIP is the login lockout's bucket key (#3078). X-Forwarded-For counts
// only when the socket peer is a trusted proxy; the chain is then walked from
// the right (the most recent hop, written by the nearest proxy) and the first
// address that is not itself a trusted proxy is the client. Everything to the
// left of that hop was written by the client and is ignored, so a client
// cannot rotate a spoofed X-Forwarded-For into a fresh bucket. With no
// header, or a peer that is not trusted, the key is the socket peer.
func (h *Hub) remoteIP(r *http.Request) string {
	return clientIPThrough(r, h.trust.get())
}

func clientIPThrough(r *http.Request, trusted []netip.Prefix) string {
	host := r.RemoteAddr
	if hst, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = hst
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap().WithZone("")
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(v, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	if len(hops) == 0 {
		return peer.String()
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			// A trusted proxy wrote something that is not an address:
			// it is the first hop we cannot vouch for, so it is the key.
			return hops[i]
		}
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	// Every hop is a trusted proxy (a LAN client through the local proxy):
	// the leftmost is the origin.
	a, _ := parseHop(hops[0])
	return a.String()
}

// parseHop reads one X-Forwarded-For entry: an address, optionally with a port
// ("1.2.3.4:5", "[::1]:5").
func parseHop(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

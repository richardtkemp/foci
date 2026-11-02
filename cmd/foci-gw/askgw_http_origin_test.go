package main

import (
	"context"
	"errors"
	"testing"
)

// askgwOrigin (#2800) names the host an HTTP ask came from, from the peer
// address only: loopback is this host, else the short reverse-DNS name, else
// the bare IP.
func TestAskgwOrigin(t *testing.T) {
	names := map[string][]string{
		"100.87.228.115": {"richards-macbook-pro.tail1234.ts.net."},
		"192.168.1.20":   {"air.lan."},
	}
	lookup := func(_ context.Context, ip string) ([]string, error) {
		if n, ok := names[ip]; ok {
			return n, nil
		}
		return nil, errors.New("no such host")
	}
	for _, tc := range []struct{ remote, want string }{
		{"100.87.228.115:53122", "richards-macbook-pro"},
		{"192.168.1.20:40000", "air"},
		{"192.168.1.150:40000", "192.168.1.150"}, // router DNS does not know it
		{"127.0.0.1:5555", "nuc"},
		{"[::1]:5555", "nuc"},
		{"[fd7a:115c:a1e0::1]:5555", "fd7a:115c:a1e0::1"},
	} {
		if got := askgwOrigin(context.Background(), tc.remote, "nuc", lookup); got != tc.want {
			t.Errorf("askgwOrigin(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// A resolver that never answers must not hold the POST past the lookup bound.
func TestAskgwOriginLookupIsBounded(t *testing.T) {
	lookup := func(ctx context.Context, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if got := askgwOrigin(context.Background(), "192.168.1.150:1", "nuc", lookup); got != "192.168.1.150" {
		t.Errorf("got %q, want the bare IP", got)
	}
}

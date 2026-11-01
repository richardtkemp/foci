package slack

// timeout_test.go: every Slack I/O path is bounded (#2459 A1 fix for the
// review's F4) — the Web API HTTP client carries a timeout (config
// request_timeout, default 30s), the Socket Mode dial handshake carries one
// (connect_timeout, default 10s), and a stalled endpoint returns an error
// instead of hanging the session's worker or the socket read loop.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// TestProvider_TimeoutsFromConfig proves Init wires the configured timeouts
// into the HTTP client (every Web API call and apps.connections.open) and the
// socket dialer, with the documented defaults when unset.
func TestProvider_TimeoutsFromConfig(t *testing.T) {
	initWith := func(mutate func(s *config.SlackSpecific)) *slackProvider {
		t.Helper()
		f := newFakeSlack(t)
		deps := providerDeps(t, f, "xoxb-ok", "xapp-ok")
		mutate(deps.Config.Platform(platformName).Slack)
		p := &slackProvider{}
		if err := p.Init(deps); err != nil {
			t.Fatal(err)
		}
		if p.mgr == nil {
			t.Fatal("Init left the provider inert despite healthy credentials")
		}
		return p
	}

	t.Run("configured values are honoured", func(t *testing.T) {
		p := initWith(func(s *config.SlackSpecific) {
			s.RequestTimeout = "250ms"
			s.ConnectTimeout = "750ms"
		})
		if got := p.mgr.api.hc.Timeout; got != 250*time.Millisecond {
			t.Errorf("web api client timeout = %v, want 250ms (request_timeout)", got)
		}
		if got := p.mgr.sock.dialTimeout; got != 750*time.Millisecond {
			t.Errorf("socket dial timeout = %v, want 750ms (connect_timeout)", got)
		}
	})
	t.Run("defaults when unset", func(t *testing.T) {
		p := initWith(func(*config.SlackSpecific) {})
		if got := p.mgr.api.hc.Timeout; got != config.DefaultSlackRequestTimeout {
			t.Errorf("web api client timeout = %v, want the default %v", got, config.DefaultSlackRequestTimeout)
		}
		if got := p.mgr.sock.dialTimeout; got != config.DefaultSlackConnectTimeout {
			t.Errorf("socket dial timeout = %v, want the default %v", got, config.DefaultSlackConnectTimeout)
		}
	})
}

// TestWebAPI_StalledEndpointTimesOut proves the HTTP timeout bounds a real
// call: a server that accepts the request and never answer produces an error
// within (roughly) the client timeout, never an indefinite block. The call
// runs under its own watchdog, so a regression that removes the timeout
// FAILS this test promptly instead of hanging it (and the suite) forever.
func TestWebAPI_StalledEndpointTimesOut(t *testing.T) {
	// The handler holds every response until the test ends (NOT
	// r.Context().Done — the server never sees the client give up on an
	// idle keep-alive connection, so waiting on it would hang Close).
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	hc := srv.Client()
	hc.Timeout = 50 * time.Millisecond
	api := newWebAPI(srv.URL+"/api", "xoxb-fake", hc)

	start := time.Now()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := api.postMessage(context.Background(), postParams{Channel: "C1", Text: "hi"})
		done <- result{err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("a call to a never-answering endpoint returned nil; want the timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("postMessage still blocked after 5s against a stalled endpoint; the client timeout must bound it")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("postMessage blocked %v against a stalled endpoint; the client timeout must bound it", elapsed)
	}
}

// TestSocket_DialHandshakeTimeoutBounded proves the configured dial timeout
// bounds the WebSocket handshake: a server that accepts the TCP connection
// but never completes the handshake makes run return the dial error instead
// of hanging the transport. run works under its own watchdog, so a
// regression that removes the handshake timeout FAILS this test promptly
// instead of hanging it forever.
func TestSocket_DialHandshakeTimeoutBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	held := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held <- c // hold the connection open: no HTTP response, no handshake
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case c := <-held:
				_ = c.Close()
			default:
				return
			}
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + ln.Addr().String() + "/socket"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sock := newSocketClient(srv.URL+"/api", "xapp-fake", srv.Client(), 40*time.Millisecond, func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	})
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- sock.run(context.Background(), func(envelope) {})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial to a never-handshaking server returned nil; want the handshake timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dial still blocked after 5s against a never-handshaking server; the handshake timeout must bound it")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("dial blocked %v; the handshake timeout must bound it", elapsed)
	}
}

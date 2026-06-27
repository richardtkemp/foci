package app

// Graceful-shutdown signalling (#900): Hub.Close runs only on the deploy/restart
// path (a crash skips the main defer chain that reaches it), so it closes every
// live app socket with WebSocket code 1012 ServerRestart. App clients key the
// fast restart-reconnect regime off that code, telling a deliberate bounce apart
// from a network drop.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/app/fap"
	"foci/internal/config"
	"foci/internal/platform"
)

func TestHubClose_ClosesClientsWithServerRestart(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev", "")
	h.deps = platform.ProviderDeps{Config: &config.Config{}}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("/app/ws", WSHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/app/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization":          {"Bearer " + d.Token},
		"Sec-WebSocket-Protocol": {fap.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// ServeWS registers the client (addClient) just after the 101 handshake,
	// concurrently with this goroutine — poll until the hub sees it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.RLock()
		n := len(h.clients)
		h.mu.RUnlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client never registered (clients=%d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Graceful shutdown.
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The client must observe a 1012 ServerRestart close frame (not an abnormal
	// 1006 drop, which is how a crash/network loss would present).
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, readErr := conn.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(readErr, &ce) {
		t.Fatalf("read after Close: got %v, want *websocket.CloseError", readErr)
	}
	if ce.Code != fap.CloseServerRestart {
		t.Errorf("close code = %d, want %d (ServerRestart)", ce.Code, fap.CloseServerRestart)
	}
}

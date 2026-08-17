package app

// The connect line must name the device.
//
// A socket that dies before its hello has no other identity anywhere: the hello
// is where deviceID normally arrives, and these sockets never send one. During
// #1713 that left a 300-connect-a-day failure unattributable from the server —
// the diagnosis had to go to client-side logging on a device that was not always
// reachable. Device-token auth already resolves the device BEFORE the upgrade,
// so the information was in scope and being discarded.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/app/fap"
	"foci/internal/config"
	flog "foci/internal/log"
	"foci/internal/platform"
)

// dialAndCaptureConnectLog drives the REAL ServeWS path over a real websocket
// and returns everything logged. Asserting against a format string retyped in
// the test would pass with the log line deleted.
func dialAndCaptureConnectLog(t *testing.T, deviceID string) (out string, token string) {
	t.Helper()

	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	// Restore os.Stderr, the package default — NOT nil, which SIGSEGVs the next
	// write and takes down unrelated tests in this package (observed 2026-08-15).
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	h := newTestHub()
	d := h.devices.pair(deviceID, "")
	h.deps = platform.ProviderDeps{Config: &config.Config{}}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	mux := http.NewServeMux()
	mux.HandleFunc("/app/ws", WSHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if d.Token == "" {
		t.Fatal("device has no token — the leak assertion below could not fire")
	}
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/app/ws",
		http.Header{
			"Authorization":          {"Bearer " + d.Token},
			"Sec-WebSocket-Protocol": {fap.Subprotocol},
		})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// ServeWS logs concurrently with this goroutine — poll rather than sleep, so
	// a loaded machine waits longer instead of failing.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "device connected") {
		if time.Now().After(deadline) {
			t.Fatalf("no connect line was logged at all:\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return buf.String(), d.Token
}

// The case that matters: a device-token socket is identified up front, so the
// connect line can name it — including for a socket that never sends a hello.
func TestConnectLog_NamesDeviceForTokenAuth(t *testing.T) {
	out, _ := dialAndCaptureConnectLog(t, "dev-under-test")

	if !strings.Contains(out, "device connected: device=dev-under-test") {
		t.Errorf("connect line does not name the device — a hello-less socket stays unattributable:\n%s", out)
	}
}

// The token is the credential; the id is not. This is the assertion that keeps
// the change safe to keep.
func TestConnectLog_NeverLogsTheDeviceToken(t *testing.T) {
	out, token := dialAndCaptureConnectLog(t, "leak-check")

	if strings.Contains(out, token) {
		t.Fatalf("device TOKEN leaked into the log — it is the credential, the id is not")
	}
	if !strings.Contains(out, "device=leak-check") {
		t.Errorf("expected the id to be present while the token is absent:\n%s", out)
	}
}

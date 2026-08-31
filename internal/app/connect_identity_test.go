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
	return dialAndCaptureConnectLogWithHeaders(t, deviceID, nil)
}

// dialAndCaptureConnectLogWithHeaders is the same, with extra request headers —
// used to drive the CF-Connecting-IP path that only exists behind the tunnel.
func dialAndCaptureConnectLogWithHeaders(t *testing.T, deviceID string, extra map[string]string) (out string, token string) {
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
		func() http.Header {
			h := http.Header{
				"Authorization":          {"Bearer " + d.Token},
				"Sec-WebSocket-Protocol": {fap.Subprotocol},
			}
			for k, v := range extra {
				h.Set(k, v)
			}
			return h
		}())
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

// The connect line must carry the client IP. deviceID answers "who"; ip answers
// "from where" — and the difference between a client that BROKE and one that
// merely MOVED NETWORK is a different diagnosis with a different owner. On
// 2026-08-17/18 a phone moved from Wi-Fi to 5G unnoticed, and the resulting
// behaviour change was attributed to a Cloudflare config edit, which was then
// reverted on a false premise (#1728).
func TestConnectLog_NamesClientIP(t *testing.T) {
	out, _ := dialAndCaptureConnectLog(t, "ip-check")

	if !strings.Contains(out, "ip=") {
		t.Errorf("connect line carries no client IP:\n%s", out)
	}
	if !strings.Contains(out, "device=ip-check") {
		t.Errorf("expected the device id alongside the ip:\n%s", out)
	}
}

// Behind the Cloudflare tunnel the ORIGIN client address arrives in
// CF-Connecting-IP; the socket peer and the rightmost X-Forwarded-For hop are
// both the proxy. Preferring it is the whole point of clientIPForLog — using
// remoteIP here would log a constant and answer nothing.
func TestConnectLog_PrefersCFConnectingIP(t *testing.T) {
	const origin = "203.0.113.77"
	out, _ := dialAndCaptureConnectLogWithHeaders(t, "cf-ip-check", map[string]string{
		"CF-Connecting-IP": origin,
		// A hostile leftmost XFF that must NOT win.
		"X-Forwarded-For": "198.51.100.1",
	})

	if !strings.Contains(out, "ip="+origin) {
		t.Errorf("connect line did not use CF-Connecting-IP (%s):\n%s", origin, out)
	}
	if strings.Contains(out, "ip=198.51.100.1") {
		t.Errorf("a spoofable X-Forwarded-For value won over CF-Connecting-IP:\n%s", out)
	}
}

// TestStallWarn_NamesDeviceAndIP guards #1782 site 1. The stall WARN is the
// ONLY record of a slow client being closed, and it was 879 of 919 WARNs in a
// live log — yet it named neither the device nor its IP, so "one bad client or
// a server fault?" needed three greps and a manual timestamp correlation
// against adjacent connect/hello lines, every single time it was asked.
//
// The IP is not decoration: per the connect line's own rationale (#1728), it is
// what distinguishes a client that BROKE from one that merely MOVED NETWORK —
// different owners, different fixes — and the stall is exactly where that
// question gets asked.
func TestStallWarn_NamesDeviceAndIP(t *testing.T) {
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	// enqueueBlockWait is a const, so this test really does wait it out. The
	// duration is itself evidence the blocking path ran rather than the fast
	// path returning early.

	h := newTestHub()
	c := newWsClient(nil, h, "203.0.113.9") // nil ws: close() documents this as the test shape
	c.deviceID = "stalled-dev"

	// Premise guard: the blocking path is only reached with a genuinely FULL
	// queue. Without this a capacity change would make the test pass by never
	// stalling at all.
	if cap(c.send) == 0 {
		t.Fatal("send buffer has zero capacity — the test would stall for the wrong reason")
	}
	for i := 0; i < cap(c.send); i++ {
		c.send <- []byte("filler")
	}

	c.enqueue(`{"t":"noop"}`)

	out := buf.String()
	if !strings.Contains(out, "outbound queue stalled") {
		t.Fatalf("the stall warn never fired — test premise broken:\n%s", out)
	}
	if !strings.Contains(out, "device=stalled-dev") {
		t.Errorf("stall warn does not name the device:\n%s", out)
	}
	if !strings.Contains(out, "ip=203.0.113.9") {
		t.Errorf("stall warn does not name the client IP:\n%s", out)
	}
}

// TestHelloLog_NamesClientBuild guards #1782 site 2. Asked "is the latest
// client code deployed?", the server could answer for itself from its binary
// and could not answer for the app at all — so a client-side regression was
// invisible in server logs and could not be correlated against a release.
//
// The fix is server-only, which was worth checking rather than assuming: the
// wire already carries it. fap.ClientInfo has App/OS/Version and the Android
// client already populates all three; the hello line simply dropped them.
func TestHelloLog_NamesClientBuild(t *testing.T) {
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	h := newTestHub()
	c := newWsClient(nil, h, "198.51.100.4")

	hello := `{"v":1,"t":"` + fap.TypeHello + `","id":"h1","seq":1,"d":{"client":` +
		`{"app":"foci-android","os":"Android 14","version":"1.4.2","deviceId":"dev-x"}}}`
	h.dispatchInbound(c, []byte(hello))

	out := buf.String()
	if !strings.Contains(out, "hello: device=dev-x") {
		t.Fatalf("the hello line never fired — test premise broken:\n%s", out)
	}
	for _, want := range []string{"app=foci-android", `os="Android 14"`, "version=1.4.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("hello line lacks %s:\n%s", want, out)
		}
	}
}

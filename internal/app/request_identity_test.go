package app

// Every authenticated app endpoint must name the device it served.
//
// The WS connect line got this in c667a9ec, but the HTTP endpoints kept
// discarding the identity they had already resolved — ServeReplay's gate was
// literally `if _, ok := h.authenticate(w, r); !ok`. On 2026-08-17 that cost a
// day and a wrong conclusion: 1370 successful GET /app/replay calls proved some
// client's HTTPS was working fine while its WebSockets died, but nothing in the
// log could say WHICH client, so "the network is broken" and "only WebSockets
// are broken" stayed indistinguishable from the server side.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"foci/internal/config"
	flog "foci/internal/log"
	"foci/internal/platform"
)

// serveAndCapture drives a REAL handler over REAL HTTP with a REAL device token
// and returns everything logged. Asserting against a retyped format string would
// pass with the log line deleted.
func serveAndCapture(t *testing.T, path string, register func(*http.ServeMux)) (out, token, deviceID string) {
	t.Helper()

	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	// Restore os.Stderr, the package default — NOT nil, which SIGSEGVs the next
	// write and takes down unrelated tests in this package.
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	h := newTestHub()
	deviceID = "dev-attr-probe"
	d := h.devices.pair(deviceID, "")
	h.deps = platform.ProviderDeps{Config: &config.Config{}}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	mux := http.NewServeMux()
	register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if d.Token == "" {
		t.Fatal("device has no token — the leak assertion below could not fire")
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	_ = resp.Body.Close()
	return buf.String(), d.Token, deviceID
}

func TestHTTPEndpoints_NameTheDevice(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		register   func(*http.ServeMux)
	}{
		{
			// The one that actually mattered: 1370 of these in 18h, all anonymous.
			name: "replay",
			path: "/app/replay?conversationId=c1&fromSeq=0",
			register: func(m *http.ServeMux) {
				m.HandleFunc("/app/replay", ReplayHandler())
			},
		},
		{
			name: "history",
			path: "/app/history?conversationId=c1",
			register: func(m *http.ServeMux) {
				m.HandleFunc("/app/history", HistoryHandler())
			},
		},
		{
			name: "devices",
			path: "/app/devices",
			register: func(m *http.ServeMux) {
				m.HandleFunc("/app/devices", DevicesHandler())
			},
		},
		{
			name: "blob",
			path: "/app/blob/some-blob-id",
			register: func(m *http.ServeMux) {
				m.HandleFunc("/app/blob/", BlobDownloadHandler())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, token, deviceID := serveAndCapture(t, tc.path, tc.register)
			if !bytes.Contains([]byte(out), []byte("device="+deviceID)) {
				t.Errorf("%s served a request without naming the device:\n%s", tc.name, out)
			}
			// deviceID is an identifier; the Token is the credential and must
			// never reach a log line.
			if bytes.Contains([]byte(out), []byte(token)) {
				t.Fatalf("%s LEAKED the device token into the log:\n%s", tc.name, out)
			}
		})
	}
}

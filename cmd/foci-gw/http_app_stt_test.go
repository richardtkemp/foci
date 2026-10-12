package main

// Gateway-side registration tests for GET /app/stt (#2315). The hub's own
// behaviour (auth, wire shape, refusal bodies) is pinned in internal/app; these
// prove the gateway actually exposes it: the route is on the mux and the
// startup endpoint inventory names it.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"foci/internal/app"
	"foci/internal/config"
	flog "foci/internal/log"
	"foci/internal/platform"
)

// enableAppProvider brings the real app provider up on a throwaway data dir —
// the production path to app.Enabled(), which gates the whole /app/* block in
// registerHTTPHandlers. Ctx stays nil so the hub starts no background workers;
// plat.Close (t.Cleanup) flushes and closes its stores. activeHub has no unset,
// so the provider stays enabled for the rest of the test binary: no other test
// in this package asserts on /app routes, so the residual state is inert.
func enableAppProvider(t *testing.T) {
	t.Helper()
	cfg := &config.Config{
		DataDir:   t.TempDir(),
		Platforms: []config.PlatformConfig{{ID: "app"}},
	}
	plat, err := platform.InitMessaging(cfg, platform.ProviderDeps{Config: cfg})
	if err != nil {
		t.Fatalf("InitMessaging: %v", err)
	}
	t.Cleanup(func() { _ = plat.Close() })
	if !app.Enabled() {
		t.Fatal("app provider enabled but app.Enabled() reports false")
	}
}

// TestAppSTTEndpoint_Registered proves the gateway mux routes GET /app/stt
// into the hub once the app provider is enabled: with no Bearer sent, the
// request must hit the hub's own auth gate (401) — a 404 is the mux's
// route-less answer, i.e. the endpoint was never registered.
func TestAppSTTEndpoint_Registered(t *testing.T) {
	enableAppProvider(t)
	d, _ := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	r := httptest.NewRequest(http.MethodGet, "/app/stt?agentId="+testAgentID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (the hub's auth gate; 404 would mean /app/stt is not registered)", w.Code)
	}
}

// TestAppSTTEndpoint_NamedInStartupLog proves the endpoint inventory the
// operator greps at startup ("registered endpoints: ..." and the per-block
// "... endpoints enabled" line) names /app/stt when the app block is
// registered — an endpoint missing from the inventory reads as missing from
// the server.
//
// Serial, for two reasons the log tap does not remove (#2357):
// enableAppProvider turns on the process-global app hub, and the inventory
// line carries nothing unique to this run, so in the parallel phase another
// test's handler registration could satisfy the assertion.
func TestAppSTTEndpoint_NamedInStartupLog(t *testing.T) {
	enableAppProvider(t)
	logs := captureLog(t, flog.INFO)

	d, _ := httpTestSetup(t, httpTestOpts{})
	_ = newTestMux(d) // registering the handlers is what logs

	if len(logs.Lines("/app/stt")) == 0 {
		t.Errorf("registered-endpoints log does not name /app/stt:\n%s", logs)
	}
}
